package cloudflare

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v4/rulesets"
	"github.com/cloudflare/cloudflare-go/v4/zones"
)

func rulesetSyntheticListResponse() rulesets.RulesetListResponse {
	return rulesets.RulesetListResponse{
		ID:          "rs-1",
		Kind:        rulesets.KindZone,
		LastUpdated: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
		Name:        "default firewall",
		Phase:       rulesets.PhaseHTTPRequestFirewallCustom,
		Version:     "3",
		Description: "synthetic test ruleset",
	}
}

func TestRulesetAccountToAsset_BasicMapping(t *testing.T) {
	p := &Provider{}
	r := rulesetSyntheticListResponse()
	r.Kind = rulesets.KindRoot

	a := p.accountRulesetToAsset("acct-1", r)

	if a.Provider != "cloudflare" {
		t.Errorf("Provider = %q, want cloudflare", a.Provider)
	}
	if a.Type != "cloudflare.ruleset" {
		t.Errorf("Type = %q, want cloudflare.ruleset", a.Type)
	}
	if a.AccountID != "acct-1" {
		t.Errorf("AccountID = %q, want acct-1", a.AccountID)
	}
	if a.ID != "rs-1" || a.Name != "default firewall" {
		t.Errorf("id/name = %q / %q", a.ID, a.Name)
	}
	if a.Status != "" {
		t.Errorf("Status = %q, want empty (rulesets have no natural status)", a.Status)
	}
	if a.Tags["scope"] != "account" {
		t.Errorf("Tags[scope] = %q, want account", a.Tags["scope"])
	}
	if a.Tags["phase"] != "http_request_firewall_custom" {
		t.Errorf("Tags[phase] = %q", a.Tags["phase"])
	}
	if a.Tags["kind"] != "root" {
		t.Errorf("Tags[kind] = %q, want root", a.Tags["kind"])
	}
	if a.Tags["last_updated"] != "2025-06-01T12:00:00Z" {
		t.Errorf("Tags[last_updated] = %q, want 2025-06-01T12:00:00Z", a.Tags["last_updated"])
	}
	if _, ok := a.Tags["zone_id"]; ok {
		t.Error("account-scoped ruleset must not carry a zone_id tag")
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestRulesetAccountToAsset_ZeroLastUpdatedOmitted(t *testing.T) {
	p := &Provider{}
	r := rulesetSyntheticListResponse()
	r.LastUpdated = time.Time{}

	a := p.accountRulesetToAsset("acct-1", r)

	if _, ok := a.Tags["last_updated"]; ok {
		t.Errorf("Tags[last_updated] should be absent for zero time, got %q", a.Tags["last_updated"])
	}
}

func TestRulesetZoneToAsset_BasicMapping(t *testing.T) {
	p := &Provider{}
	z := zones.Zone{
		ID:      "zone-id-1",
		Name:    "example.com",
		Account: zones.ZoneAccount{ID: "acct-id-1", Name: "Test Account"},
	}

	a := p.zoneRulesetToAsset(z, rulesetSyntheticListResponse())

	if a.Provider != "cloudflare" {
		t.Errorf("Provider = %q, want cloudflare", a.Provider)
	}
	if a.Type != "cloudflare.ruleset" {
		t.Errorf("Type = %q, want cloudflare.ruleset", a.Type)
	}
	if a.AccountID != "acct-id-1" {
		t.Errorf("AccountID = %q, want acct-id-1 (inherited from zone)", a.AccountID)
	}
	if a.ID != "rs-1" || a.Name != "default firewall" {
		t.Errorf("id/name = %q / %q", a.ID, a.Name)
	}
	if a.Tags["scope"] != "zone" {
		t.Errorf("Tags[scope] = %q, want zone", a.Tags["scope"])
	}
	if a.Tags["zone_id"] != "zone-id-1" {
		t.Errorf("Tags[zone_id] = %q, want zone-id-1", a.Tags["zone_id"])
	}
	if a.Tags["zone_name"] != "example.com" {
		t.Errorf("Tags[zone_name] = %q, want example.com", a.Tags["zone_name"])
	}
	if a.Tags["phase"] != "http_request_firewall_custom" {
		t.Errorf("Tags[phase] = %q", a.Tags["phase"])
	}
	if a.Tags["kind"] != "zone" {
		t.Errorf("Tags[kind] = %q, want zone", a.Tags["kind"])
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestRulesetToAsset_IncludeRawRoundTrip(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	z := zones.Zone{ID: "z-1", Name: "ex.com", Account: zones.ZoneAccount{ID: "a-1"}}

	for name, a := range map[string]struct {
		raw json.RawMessage
	}{
		"account": {p.accountRulesetToAsset("a-1", rulesetSyntheticListResponse()).Raw},
		"zone":    {p.zoneRulesetToAsset(z, rulesetSyntheticListResponse()).Raw},
	} {
		if a.raw == nil {
			t.Fatalf("%s: Raw should be populated when IncludeRaw=true", name)
		}
		var back map[string]any
		if err := json.Unmarshal(a.raw, &back); err != nil {
			t.Fatalf("%s: Raw is not valid JSON: %v", name, err)
		}
		if back["id"] != "rs-1" {
			t.Errorf("%s: Raw.id = %v, want rs-1", name, back["id"])
		}
	}
}
