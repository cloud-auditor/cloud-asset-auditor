// Package cloudflare is the Phase 2 provider. It enumerates the resources
// listed in init-plan.md §3 against a single Cloudflare account via the v4
// generated SDK. Auth is API-token only — the legacy email+key path is
// intentionally not supported.
package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/accounts"
	"github.com/cloudflare/cloudflare-go/v4/option"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

const (
	providerName          = "cloudflare"
	defaultMaxConcurrency = 5
)

// Config drives provider construction. APIToken is required; the rest have
// sensible defaults.
type Config struct {
	APIToken       string
	MaxConcurrency int
	IncludeRaw     bool
}

// Provider implements core.Provider for Cloudflare.
type Provider struct {
	client *cf.Client
	cfg    Config

	// listAccounts cache — see accounts.go.
	accountsOnce sync.Once
	accounts     []accounts.Account
	accountsErr  error
}

// Compile-time check that we satisfy the optional Configurable interfaces.
var (
	_ core.Provider                = (*Provider)(nil)
	_ core.ConcurrencyConfigurable = (*Provider)(nil)
	_ core.IncludeRawConfigurable  = (*Provider)(nil)
)

// init registers the Cloudflare provider into the core registry. The factory
// reads CLOUDFLARE_API_TOKEN at call time so missing creds produce a useful
// error message via selectProviders' warning path, not a startup crash.
func init() {
	core.Register(providerName, func() (core.Provider, error) {
		return New(Config{APIToken: os.Getenv("CLOUDFLARE_API_TOKEN")})
	})
}

// New constructs a configured Provider. Returns an error if APIToken is empty.
func New(cfg Config) (*Provider, error) {
	if cfg.APIToken == "" {
		return nil, errors.New("cloudflare: CLOUDFLARE_API_TOKEN is not set")
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = defaultMaxConcurrency
	}
	return &Provider{
		client: cf.NewClient(option.WithAPIToken(cfg.APIToken)),
		cfg:    cfg,
	}, nil
}

func (p *Provider) Name() string { return providerName }

// SetMaxConcurrency wires --max-concurrency from the audit CLI. Non-positive
// values are ignored so a missing flag value can't accidentally serialize
// all resource collectors onto one goroutine.
func (p *Provider) SetMaxConcurrency(n int) {
	if n > 0 {
		p.cfg.MaxConcurrency = n
	}
}

func (p *Provider) SetIncludeRaw(b bool) { p.cfg.IncludeRaw = b }

// Validate verifies the API token by calling the v4 /user/tokens/verify
// endpoint. Cheap and unambiguous — no resource enumeration involved.
func (p *Provider) Validate(ctx context.Context) error {
	if _, err := p.client.User.Tokens.Verify(ctx); err != nil {
		return fmt.Errorf("cloudflare: verify api token: %w", err)
	}
	return nil
}

// rawOf marshals v for Asset.Raw when --include-raw is set; otherwise nil.
// Marshal errors are silently swallowed — a single asset's Raw can't be
// allowed to abort the whole audit.
func (p *Provider) rawOf(v any) json.RawMessage {
	if !p.cfg.IncludeRaw {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// sendAsset is the ctx-cancel-aware send used by every collector. Returning
// false signals "ctx cancelled — stop now" to the caller.
func sendAsset(ctx context.Context, out chan<- core.Asset, a core.Asset) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- a:
		return true
	}
}

// timePtr converts an SDK time.Time (often zero-valued when the API omitted
// the field) into the *time.Time that Asset.CreatedAt expects.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
