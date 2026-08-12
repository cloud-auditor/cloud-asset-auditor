// Package tailscale inventories a Tailscale tailnet (WireGuard mesh /
// zero-trust) via the public v2 REST API. Auth is an API access token only.
// Like the other providers it streams core.Assets; like NetBird it talks to
// the API through a hand-rolled stdlib client (see client.go) instead of a
// vendored SDK.
package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

const (
	providerName          = "tailscale"
	defaultMaxConcurrency = 5

	// defaultTailnet is the API's "the token's default tailnet" path sentinel.
	// It doubles as the AccountID when TAILSCALE_TAILNET is unset — a
	// predictable label beats resolving the real name with an extra call.
	defaultTailnet = "-"
)

// Config drives provider construction. APIKey is required; Tailnet defaults
// to "-" (the token's default tailnet) and BaseURL to the Tailscale cloud
// API — override the latter for Headscale-style self-hosted control planes.
type Config struct {
	APIKey         string
	Tailnet        string
	BaseURL        string
	MaxConcurrency int
	IncludeRaw     bool
}

// Provider implements core.Provider for Tailscale.
type Provider struct {
	client *client
	cfg    Config
}

// Compile-time checks for the optional Configurable interfaces.
var (
	_ core.Provider                = (*Provider)(nil)
	_ core.ConcurrencyConfigurable = (*Provider)(nil)
	_ core.IncludeRawConfigurable  = (*Provider)(nil)
	_ core.TailscaleConfigurable   = (*Provider)(nil)
)

// init registers the Tailscale provider. The factory reads TAILSCALE_API_KEY,
// TAILSCALE_TAILNET and TAILSCALE_API_BASE_URL at call time so missing creds
// surface as a clean provider-selection warning rather than a startup crash.
func init() {
	core.Register(providerName, func() (core.Provider, error) {
		return New(Config{
			APIKey:  os.Getenv("TAILSCALE_API_KEY"),
			Tailnet: os.Getenv("TAILSCALE_TAILNET"),
			BaseURL: os.Getenv("TAILSCALE_API_BASE_URL"),
		})
	})
}

// New constructs a configured Provider. Returns an error if APIKey is empty.
func New(cfg Config) (*Provider, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("tailscale: TAILSCALE_API_KEY is not set")
	}
	if cfg.Tailnet == "" {
		cfg.Tailnet = defaultTailnet
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = defaultMaxConcurrency
	}
	return &Provider{
		client: newClient(cfg.BaseURL, cfg.APIKey),
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

// SetTailnet wires --tailscale-tailnet. An empty value keeps the configured
// tailnet (env TAILSCALE_TAILNET, or the "-" default-tailnet sentinel).
func (p *Provider) SetTailnet(name string) {
	if name != "" {
		p.cfg.Tailnet = name
	}
}

// SetAPIBaseURL wires --tailscale-api-url (self-hosted control-plane base
// URL). An empty value leaves the configured default in place; a non-empty
// one rebuilds the client against the new base URL.
func (p *Provider) SetAPIBaseURL(u string) {
	if u != "" {
		p.cfg.BaseURL = u
		p.client = newClient(u, p.cfg.APIKey)
	}
}

// Validate confirms the token works by reading the tailnet's DNS preferences
// — the cheapest authenticated call. A 401/403 is reported as a credential
// problem.
func (p *Provider) Validate(ctx context.Context) error {
	var prefs struct {
		MagicDNS bool `json:"magicDNS"`
	}
	if err := p.client.getJSON(ctx, p.tailnetPath("/dns/preferences"), &prefs); err != nil {
		if isAuthError(err) {
			return fmt.Errorf("tailscale: TAILSCALE_API_KEY rejected (check the token and its expiry): %w", err)
		}
		return fmt.Errorf("tailscale: validate: %w", err)
	}
	return nil
}

// tailnetPath builds "/api/v2/tailnet/<tailnet><suffix>". The tailnet rides
// in a path segment, so it's escaped (legacy org names can be email-shaped).
func (p *Provider) tailnetPath(suffix string) string {
	return "/api/v2/tailnet/" + url.PathEscape(p.cfg.Tailnet) + suffix
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
