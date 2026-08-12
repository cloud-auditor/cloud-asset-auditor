// Package gcp inventories Google Cloud assets via the Cloud Asset Inventory
// API's searchAllResources method — one call that returns every resource type
// (compute, storage, GKE, networking, IAM, …) across a project, folder, or
// organization. That's the same "ask the platform what exists" universality the
// Kubernetes provider gets from discovery, instead of wiring 50 service SDKs.
//
// Auth is Application Default Credentials. Like NetBird, this provider uses a
// hand-rolled REST client rather than a vendored SDK — the google-cloud-go
// asset client pulls gRPC and a large dependency tree for what is, over REST, a
// single paginated GET. ADC token sourcing comes from golang.org/x/oauth2/google.
package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

const (
	providerName          = "gcp"
	defaultMaxConcurrency = 5
)

// Config drives provider construction. Scope is required (the search root);
// BaseURL is a test seam.
type Config struct {
	Scope          string // projects/<id> | folders/<num> | organizations/<num>
	MaxConcurrency int
	IncludeRaw     bool
	BaseURL        string // empty = the cloud Asset Inventory endpoint
}

// Provider implements core.Provider for Google Cloud.
type Provider struct {
	client *client
	cfg    Config
}

var (
	_ core.Provider                = (*Provider)(nil)
	_ core.ConcurrencyConfigurable = (*Provider)(nil)
	_ core.IncludeRawConfigurable  = (*Provider)(nil)
	_ core.GCPConfigurable         = (*Provider)(nil)
)

// init registers the GCP provider. The factory always succeeds (constructing
// with the env-derived scope, which may be empty) so that --gcp-project /
// --gcp-scope — applied AFTER the factory runs — can supply the scope on their
// own. With no scope from any source, Collect is a quiet no-op, so an
// all-provider audit on a non-GCP machine produces nothing and no noise.
func init() {
	core.Register(providerName, func() (core.Provider, error) {
		return New(Config{Scope: scopeFromEnv()})
	})
}

// scopeRE matches the three valid Cloud Asset Inventory scope shapes. It also
// excludes the URL-significant characters (/ ? # : and whitespace) so a
// fat-fingered scope can't mangle the request URL — see validateScope.
var scopeRE = regexp.MustCompile(`^(projects|folders|organizations)/[^/?#:\s]+$`)

// validateScope rejects a non-empty scope that isn't one of the canonical
// shapes, with an actionable message. Empty is allowed (it means "unconfigured"
// and is handled as a no-op upstream).
func validateScope(scope string) error {
	if scope == "" || scopeRE.MatchString(scope) {
		return nil
	}
	return fmt.Errorf("gcp: invalid scope %q (want projects/<id>, folders/<num>, or organizations/<num>)", scope)
}

// scopeFromEnv resolves the default search scope: GCP_SCOPE verbatim (for a
// folder/org), else GOOGLE_CLOUD_PROJECT wrapped as projects/<id>.
func scopeFromEnv() string {
	if s := strings.TrimSpace(os.Getenv("GCP_SCOPE")); s != "" {
		return s
	}
	if p := strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT")); p != "" {
		return "projects/" + p
	}
	return ""
}

// quotaProject returns the project to bill for the API quota (the
// X-Goog-User-Project header). Explicit GOOGLE_CLOUD_QUOTA_PROJECT wins;
// otherwise a projects/<id> scope implies its own project. A folder/org scope
// has no implicit project — user-credential callers must set the env var (a
// service-account caller doesn't need one).
func (p *Provider) quotaProject() string {
	if q := strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_QUOTA_PROJECT")); q != "" {
		return q
	}
	if proj, ok := strings.CutPrefix(p.cfg.Scope, "projects/"); ok {
		return proj
	}
	return ""
}

// New constructs a configured Provider.
func New(cfg Config) (*Provider, error) {
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = defaultMaxConcurrency
	}
	return &Provider{client: newClient(cfg.BaseURL), cfg: cfg}, nil
}

func (p *Provider) Name() string { return providerName }

func (p *Provider) SetMaxConcurrency(n int) {
	if n > 0 {
		p.cfg.MaxConcurrency = n
	}
}

func (p *Provider) SetIncludeRaw(b bool) { p.cfg.IncludeRaw = b }

// SetScope overrides the search scope (--gcp-scope / --gcp-project). Empty
// leaves the env-derived default in place.
func (p *Provider) SetScope(scope string) {
	if scope != "" {
		p.cfg.Scope = scope
	}
}

// Validate confirms ADC + the Cloud Asset API + the cloudasset.viewer
// permission by fetching the first page of results.
func (p *Provider) Validate(ctx context.Context) error {
	if p.cfg.Scope == "" {
		return errors.New("gcp: no scope configured (set GOOGLE_CLOUD_PROJECT/GCP_SCOPE or --gcp-project/--gcp-scope)")
	}
	if err := validateScope(p.cfg.Scope); err != nil {
		return err
	}
	if _, err := p.client.searchAllResources(ctx, p.cfg.Scope, "", p.quotaProject()); err != nil {
		return fmt.Errorf("gcp: validate %s: %w", p.cfg.Scope, err)
	}
	return nil
}

// rawOf marshals v for Asset.Raw when --include-raw is set; otherwise nil.
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

// sendAsset is the ctx-cancel-aware send; false means "ctx cancelled — stop".
func sendAsset(ctx context.Context, out chan<- core.Asset, a core.Asset) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- a:
		return true
	}
}
