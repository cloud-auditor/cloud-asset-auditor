// Package netbird inventories a NetBird (WireGuard mesh / zero-trust) account
// via its REST Management API. Auth is a Personal Access Token only. Like the
// other providers it streams core.Assets; unlike Cloudflare/OCI/Kubernetes it
// talks to the API through a hand-rolled stdlib client (see client.go) instead
// of a vendored SDK.
package netbird

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

const (
	providerName          = "netbird"
	defaultMaxConcurrency = 5
)

// Config drives provider construction. APIToken is required; ManagementURL
// defaults to the NetBird cloud endpoint and is overridden for self-hosted.
type Config struct {
	APIToken       string
	ManagementURL  string
	MaxConcurrency int
	IncludeRaw     bool
}

// Provider implements core.Provider for NetBird.
type Provider struct {
	client *client
	cfg    Config

	// account state is resolved once (a single GET /api/accounts) and shared:
	// accountID is stamped onto every asset; accounts is the full list the
	// account collector emits; accountErr is the resolve error, surfaced by
	// that collector. One round-trip, one error path.
	accountOnce sync.Once
	accounts    []account
	accountID   string
	accountErr  error
}

// Compile-time checks for the optional Configurable interfaces.
var (
	_ core.Provider                = (*Provider)(nil)
	_ core.ConcurrencyConfigurable = (*Provider)(nil)
	_ core.IncludeRawConfigurable  = (*Provider)(nil)
	_ core.NetbirdConfigurable     = (*Provider)(nil)
)

// init registers the NetBird provider. The factory reads NETBIRD_API_TOKEN and
// NETBIRD_MANAGEMENT_URL at call time so missing creds surface as a clean
// provider-selection warning rather than a startup crash.
func init() {
	core.Register(providerName, func() (core.Provider, error) {
		return New(Config{
			APIToken:      os.Getenv("NETBIRD_API_TOKEN"),
			ManagementURL: os.Getenv("NETBIRD_MANAGEMENT_URL"),
		})
	})
}

// New constructs a configured Provider. Returns an error if APIToken is empty.
func New(cfg Config) (*Provider, error) {
	if cfg.APIToken == "" {
		return nil, errors.New("netbird: NETBIRD_API_TOKEN is not set")
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = defaultMaxConcurrency
	}
	return &Provider{
		client: newClient(cfg.ManagementURL, cfg.APIToken),
		cfg:    cfg,
	}, nil
}

func (p *Provider) Name() string { return providerName }

// SetMaxConcurrency wires --max-concurrency. Non-positive values are ignored.
func (p *Provider) SetMaxConcurrency(n int) {
	if n > 0 {
		p.cfg.MaxConcurrency = n
	}
}

func (p *Provider) SetIncludeRaw(b bool) { p.cfg.IncludeRaw = b }

// SetManagementURL wires --netbird-management-url (self-hosted base URL). An
// empty value leaves the configured default in place; a non-empty one rebuilds
// the client against the new base URL.
func (p *Provider) SetManagementURL(u string) {
	if u != "" {
		p.cfg.ManagementURL = u
		p.client = newClient(u, p.cfg.APIToken)
	}
}

// Validate confirms the token works by listing accounts — the cheapest
// authenticated call. A 401/403 is reported as a credential problem.
func (p *Provider) Validate(ctx context.Context) error {
	var accts []json.RawMessage
	if err := p.client.getJSON(ctx, "/api/accounts", &accts); err != nil {
		if isAuthError(err) {
			return fmt.Errorf("netbird: NETBIRD_API_TOKEN rejected (check the token and its permissions): %w", err)
		}
		return fmt.Errorf("netbird: validate: %w", err)
	}
	return nil
}

// rawOf marshals v for Asset.Raw when --include-raw is set; otherwise nil.
// Marshal errors are swallowed — one asset's Raw can't abort the audit.
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

// sendAsset is the ctx-cancel-aware send used by every collector. false means
// "ctx cancelled — stop".
func sendAsset(ctx context.Context, out chan<- core.Asset, a core.Asset) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- a:
		return true
	}
}

// timePtr converts a parsed timestamp into the *time.Time Asset.CreatedAt
// expects, returning nil for the zero value.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
