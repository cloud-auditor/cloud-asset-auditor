package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/cloudflare/cloudflare-go/v4/zones"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func TestNew_RequiresToken(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("expected error when APIToken is empty")
	}
}

func TestNew_AppliesDefaults(t *testing.T) {
	p, err := New(Config{APIToken: "fake-token"})
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.MaxConcurrency != defaultMaxConcurrency {
		t.Errorf("MaxConcurrency = %d, want %d", p.cfg.MaxConcurrency, defaultMaxConcurrency)
	}
	if p.Name() != "cloudflare" {
		t.Errorf("Name() = %q, want cloudflare", p.Name())
	}
}

func TestSetMaxConcurrency_IgnoresNonPositive(t *testing.T) {
	p, _ := New(Config{APIToken: "fake-token", MaxConcurrency: 7})
	if p.cfg.MaxConcurrency != 7 {
		t.Fatalf("initial MaxConcurrency = %d, want 7", p.cfg.MaxConcurrency)
	}
	p.SetMaxConcurrency(0)
	if p.cfg.MaxConcurrency != 7 {
		t.Errorf("zero should be ignored; MaxConcurrency = %d, want 7", p.cfg.MaxConcurrency)
	}
	p.SetMaxConcurrency(-3)
	if p.cfg.MaxConcurrency != 7 {
		t.Errorf("negative should be ignored; MaxConcurrency = %d, want 7", p.cfg.MaxConcurrency)
	}
	p.SetMaxConcurrency(12)
	if p.cfg.MaxConcurrency != 12 {
		t.Errorf("positive value not applied; MaxConcurrency = %d, want 12", p.cfg.MaxConcurrency)
	}
}

func TestSetIncludeRaw(t *testing.T) {
	p, _ := New(Config{APIToken: "fake-token"})
	if p.cfg.IncludeRaw {
		t.Fatal("IncludeRaw should default to false")
	}
	p.SetIncludeRaw(true)
	if !p.cfg.IncludeRaw {
		t.Error("SetIncludeRaw(true) didn't apply")
	}
}

func TestZoneToAsset_BasicMapping(t *testing.T) {
	p := &Provider{}
	created := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	z := zones.Zone{
		ID:        "zone-id-1",
		Name:      "example.com",
		Status:    zones.ZoneStatusActive,
		Account:   zones.ZoneAccount{ID: "acct-id-1", Name: "Test Account"},
		CreatedOn: created,
		Paused:    false,
	}

	a := p.zoneToAsset(z)

	want := core.Asset{
		Provider:  "cloudflare",
		AccountID: "acct-id-1",
		Type:      "cloudflare.zone",
		ID:        "zone-id-1",
		Name:      "example.com",
		Status:    "active",
		CreatedAt: &created,
	}
	if a.Provider != want.Provider || a.AccountID != want.AccountID ||
		a.Type != want.Type || a.ID != want.ID || a.Name != want.Name ||
		a.Status != want.Status {
		t.Errorf("scalar fields mismatched\n got: %+v\nwant: %+v", a, want)
	}
	if a.CreatedAt == nil || !a.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt, &created)
	}
	if a.Tags["account_name"] != "Test Account" {
		t.Errorf("Tags[account_name] = %q, want Test Account", a.Tags["account_name"])
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestZoneToAsset_ZeroCreatedOnOmitted(t *testing.T) {
	p := &Provider{}
	z := zones.Zone{ID: "z", Name: "x", Account: zones.ZoneAccount{ID: "a"}}
	a := p.zoneToAsset(z)
	if a.CreatedAt != nil {
		t.Errorf("CreatedAt should be nil for zero time, got %v", a.CreatedAt)
	}
}

func TestZoneToAsset_IncludeRawRoundTrip(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	z := zones.Zone{
		ID:      "zone-1",
		Name:    "ex.com",
		Status:  zones.ZoneStatusActive,
		Account: zones.ZoneAccount{ID: "a"},
	}

	a := p.zoneToAsset(z)
	if a.Raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(a.Raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if back["id"] != "zone-1" {
		t.Errorf("Raw.id = %v, want zone-1", back["id"])
	}
}

func TestDNSRecordToAsset(t *testing.T) {
	p := &Provider{}
	z := zones.Zone{
		ID:      "zone-id-1",
		Name:    "example.com",
		Account: zones.ZoneAccount{ID: "acct-id-1"},
	}
	r := dns.RecordResponse{
		ID:      "rec-1",
		Name:    "www.example.com",
		Type:    dns.RecordResponseTypeA,
		Content: "192.0.2.1",
		Proxied: true,
	}

	a := p.dnsRecordToAsset(z, r)

	if a.Provider != "cloudflare" {
		t.Errorf("Provider = %q", a.Provider)
	}
	if a.Type != "cloudflare.dns_record" {
		t.Errorf("Type = %q, want cloudflare.dns_record", a.Type)
	}
	if a.AccountID != "acct-id-1" {
		t.Errorf("AccountID = %q, want acct-id-1 (inherited from zone)", a.AccountID)
	}
	if a.ID != "rec-1" || a.Name != "www.example.com" {
		t.Errorf("id/name = %q / %q", a.ID, a.Name)
	}
	if a.Tags["zone_name"] != "example.com" {
		t.Errorf("Tags[zone_name] = %q", a.Tags["zone_name"])
	}
	if a.Tags["content"] != "192.0.2.1" {
		t.Errorf("Tags[content] = %q", a.Tags["content"])
	}
	if a.Tags["type"] != "A" {
		t.Errorf("Tags[type] = %q", a.Tags["type"])
	}
	if a.Tags["proxied"] != "true" {
		t.Errorf("Tags[proxied] = %q, want true", a.Tags["proxied"])
	}
}

func TestInit_RegistersProvider(t *testing.T) {
	// init() runs at package load. The factory should be registered even
	// before any other test calls New().
	if _, ok := core.Lookup("cloudflare"); !ok {
		t.Fatal("expected cloudflare to be registered by init()")
	}
}

func TestRegisteredFactory_FailsWithoutEnvToken(t *testing.T) {
	// Save and clear the env so the factory hits its missing-token branch
	// deterministically regardless of the test runner's environment.
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	factory, ok := core.Lookup("cloudflare")
	if !ok {
		t.Fatal("provider not registered")
	}
	_, err := factory()
	if err == nil {
		t.Fatal("expected factory error with empty CLOUDFLARE_API_TOKEN")
	}
}

func TestValidate_NetworkPath_Skipped(_ *testing.T) {
	// Validate hits the real Cloudflare API, so it lives in an integration
	// test (build-tag-gated) — not here. This placeholder keeps the
	// intent visible.
	_ = context.Background
	_ = errors.New
}
