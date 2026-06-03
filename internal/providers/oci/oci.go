// Package oci is the Phase 3 provider. It walks the tenancy's full
// compartment tree and enumerates the resources listed in init-plan.md §3
// against each (compartment, region) pair. Auth follows the chain:
// instance principal → resource principal → config file → env vars.
package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

const (
	providerName          = "oci"
	defaultMaxConcurrency = 5
)

// Config drives provider construction. Profile and Regions are populated
// from --oci-profile / --oci-regions via the optional Configurable
// interfaces in core; auth itself is resolved lazily on first use.
type Config struct {
	Profile        string   // ~/.oci/config profile name; "" means DEFAULT
	Regions        []string // explicit list, or {"all"}, or empty for home region only
	MaxConcurrency int
	IncludeRaw     bool
}

// Provider implements core.Provider for OCI. Auth resolution happens once
// on first use (Validate or Collect) so construction stays cheap and the
// factory can run during package init() without touching the network.
type Provider struct {
	cfg Config

	authOnce sync.Once
	auth     common.ConfigurationProvider
	authErr  error

	tenancyOCID string
	homeRegion  string

	// Object Storage's namespace is tenancy-global; resolve it once and
	// share the result across every per-(region, compartment) bucket
	// collector instead of paying a GetNamespace round-trip each time.
	nsOnce sync.Once
	nsName string
	nsErr  error
}

// Compile-time checks for the optional interfaces.
var (
	_ core.Provider                = (*Provider)(nil)
	_ core.ConcurrencyConfigurable = (*Provider)(nil)
	_ core.IncludeRawConfigurable  = (*Provider)(nil)
	_ core.ProfileConfigurable     = (*Provider)(nil)
	_ core.RegionsConfigurable     = (*Provider)(nil)
)

func init() {
	core.Register(providerName, func() (core.Provider, error) {
		return New(Config{}), nil
	})
}

// New constructs a Provider with defaults. Auth resolution is deferred.
// The factory never errors — credential problems surface from Validate
// or Collect, where they belong.
func New(cfg Config) *Provider {
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = defaultMaxConcurrency
	}
	return &Provider{cfg: cfg}
}

func (p *Provider) Name() string { return providerName }

func (p *Provider) SetMaxConcurrency(n int) {
	if n > 0 {
		p.cfg.MaxConcurrency = n
	}
}

func (p *Provider) SetIncludeRaw(b bool) { p.cfg.IncludeRaw = b }

func (p *Provider) SetProfile(s string) {
	if s != "" {
		p.cfg.Profile = s
	}
}

func (p *Provider) SetRegions(regions []string) { p.cfg.Regions = regions }

// ensureAuth resolves credentials exactly once. Subsequent callers get the
// cached result (or cached error).
func (p *Provider) ensureAuth() (common.ConfigurationProvider, error) {
	p.authOnce.Do(func() {
		auth, err := resolveAuth(p.cfg.Profile)
		if err != nil {
			p.authErr = err
			return
		}
		p.auth = auth

		tenancyOCID, err := auth.TenancyOCID()
		if err != nil {
			p.authErr = fmt.Errorf("read tenancy OCID: %w", err)
			return
		}
		p.tenancyOCID = tenancyOCID

		region, err := auth.Region()
		if err != nil {
			p.authErr = fmt.Errorf("read home region: %w", err)
			return
		}
		p.homeRegion = region
	})
	return p.auth, p.authErr
}

// Validate hits identity.GetTenancy as a cheap end-to-end check. The
// request both proves the credentials are valid and confirms the tenancy
// OCID from auth matches what the service sees.
func (p *Provider) Validate(ctx context.Context) error {
	if _, err := p.ensureAuth(); err != nil {
		return fmt.Errorf("oci: %w", err)
	}
	if err := p.checkTenancyAccess(ctx); err != nil {
		return fmt.Errorf("oci: %w", err)
	}
	return nil
}

// rawOf marshals v for Asset.Raw when --include-raw is set; nil otherwise.
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

// sendAsset is the ctx-cancel-aware send used by every collector.
func sendAsset(ctx context.Context, out chan<- core.Asset, a core.Asset) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- a:
		return true
	}
}

// derefStr safely dereferences an OCI SDK *string (every SDK field is a
// pointer, so deref-with-nil-guard is the most-used helper in this package).
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefTime converts the SDK's *common.SDKTime to *time.Time, returning nil
// for nil or zero-valued inputs.
func derefTime(t *common.SDKTime) *time.Time {
	if t == nil || t.IsZero() { // SDKTime embeds time.Time, so IsZero promotes.
		return nil
	}
	out := t.Time
	return &out
}

// i64Str formats an optional *int64 (sizes, memory) as a tag value,
// returning "" for nil so an unset field reads as empty rather than "0".
func i64Str(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}

// iStr is i64Str for the SDK's *int fields (counts).
func iStr(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

// mergeFreeformTags copies freeform tags into a fresh map, leaving room for
// the caller to mix in additional key/value pairs without aliasing the SDK's
// underlying map.
func mergeFreeformTags(freeform map[string]string, extras ...[2]string) map[string]string {
	out := make(map[string]string, len(freeform)+len(extras))
	for k, v := range freeform {
		out[k] = v
	}
	for _, kv := range extras {
		out[kv[0]] = kv[1]
	}
	if len(out) == 0 {
		return nil // omitempty in Asset.Tags
	}
	return out
}

// ErrNoCredentials is returned by resolveAuth when every step in the chain
// fails. Exported for testing.
var ErrNoCredentials = errors.New("no OCI credentials found (tried instance principal, resource principal, config file, env)")
