package cloudflare

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v4/page_rules"
	"github.com/cloudflare/cloudflare-go/v4/zones"
)

func pageRuleTestZone() zones.Zone {
	return zones.Zone{
		ID:      "zone-id-pr",
		Name:    "example.com",
		Account: zones.ZoneAccount{ID: "acct-id-pr"},
	}
}

func pageRuleTestTarget(pattern string) page_rules.Target {
	return page_rules.Target{
		Constraint: page_rules.TargetConstraint{
			Operator: page_rules.TargetConstraintOperatorMatches,
			Value:    pattern,
		},
		Target: page_rules.TargetTargetURL,
	}
}

func TestPageRuleToAsset_BasicMapping(t *testing.T) {
	p := &Provider{}
	created := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	r := page_rules.PageRule{
		ID:        "pr-1",
		Priority:  3,
		Status:    page_rules.PageRuleStatusActive,
		CreatedOn: created,
		Targets:   []page_rules.Target{pageRuleTestTarget("https://example.com/images/*")},
	}

	a := p.pageRuleToAsset(pageRuleTestZone(), r)

	if a.Provider != "cloudflare" {
		t.Errorf("Provider = %q, want cloudflare", a.Provider)
	}
	if a.Type != "cloudflare.page_rule" {
		t.Errorf("Type = %q, want cloudflare.page_rule", a.Type)
	}
	if a.AccountID != "acct-id-pr" {
		t.Errorf("AccountID = %q, want acct-id-pr (inherited from zone)", a.AccountID)
	}
	if a.ID != "pr-1" {
		t.Errorf("ID = %q, want pr-1", a.ID)
	}
	if a.Name != "https://example.com/images/*" {
		t.Errorf("Name = %q, want first target URL pattern", a.Name)
	}
	if a.Status != "active" {
		t.Errorf("Status = %q, want active", a.Status)
	}
	if a.CreatedAt == nil || !a.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt, created)
	}
	if a.Tags["zone_id"] != "zone-id-pr" {
		t.Errorf("Tags[zone_id] = %q, want zone-id-pr", a.Tags["zone_id"])
	}
	if a.Tags["zone_name"] != "example.com" {
		t.Errorf("Tags[zone_name] = %q, want example.com", a.Tags["zone_name"])
	}
	if a.Tags["priority"] != "3" {
		t.Errorf("Tags[priority] = %q, want 3", a.Tags["priority"])
	}
	if a.Tags["status"] != "active" {
		t.Errorf("Tags[status] = %q, want active", a.Tags["status"])
	}
	if a.Tags["targets"] != "https://example.com/images/*" {
		t.Errorf("Tags[targets] = %q", a.Tags["targets"])
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestPageRuleToAsset_NameFallsBackToRuleID(t *testing.T) {
	p := &Provider{}
	r := page_rules.PageRule{
		ID:     "pr-no-targets",
		Status: page_rules.PageRuleStatusDisabled,
	}

	a := p.pageRuleToAsset(pageRuleTestZone(), r)

	if a.Name != "pr-no-targets" {
		t.Errorf("Name = %q, want rule ID fallback pr-no-targets", a.Name)
	}
	if a.Status != "disabled" {
		t.Errorf("Status = %q, want disabled", a.Status)
	}
	if a.Tags["targets"] != "" {
		t.Errorf("Tags[targets] = %q, want empty", a.Tags["targets"])
	}
	if a.CreatedAt != nil {
		t.Errorf("CreatedAt should be nil for zero time, got %v", a.CreatedAt)
	}
}

func TestPageRuleToAsset_MultipleTargetsJoined(t *testing.T) {
	p := &Provider{}
	r := page_rules.PageRule{
		ID:       "pr-multi",
		Priority: 10,
		Status:   page_rules.PageRuleStatusActive,
		Targets: []page_rules.Target{
			pageRuleTestTarget("https://example.com/a/*"),
			pageRuleTestTarget("https://example.com/b/*"),
		},
	}

	a := p.pageRuleToAsset(pageRuleTestZone(), r)

	if a.Name != "https://example.com/a/*" {
		t.Errorf("Name = %q, want first target pattern", a.Name)
	}
	want := "https://example.com/a/*,https://example.com/b/*"
	if a.Tags["targets"] != want {
		t.Errorf("Tags[targets] = %q, want %q", a.Tags["targets"], want)
	}
	if a.Tags["priority"] != "10" {
		t.Errorf("Tags[priority] = %q, want 10", a.Tags["priority"])
	}
}

func TestPageRuleToAsset_SkipsEmptyTargetConstraints(t *testing.T) {
	p := &Provider{}
	r := page_rules.PageRule{
		ID: "pr-empty-first",
		Targets: []page_rules.Target{
			{Target: page_rules.TargetTargetURL}, // empty constraint value
			pageRuleTestTarget("https://example.com/real/*"),
		},
	}

	a := p.pageRuleToAsset(pageRuleTestZone(), r)

	if a.Name != "https://example.com/real/*" {
		t.Errorf("Name = %q, want first non-empty target pattern", a.Name)
	}
	if a.Tags["targets"] != "https://example.com/real/*" {
		t.Errorf("Tags[targets] = %q, want empty constraints skipped", a.Tags["targets"])
	}
}

func TestPageRuleToAsset_IncludeRawRoundTrip(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	r := page_rules.PageRule{
		ID:       "pr-raw",
		Priority: 1,
		Status:   page_rules.PageRuleStatusActive,
		Targets:  []page_rules.Target{pageRuleTestTarget("https://example.com/*")},
	}

	a := p.pageRuleToAsset(pageRuleTestZone(), r)
	if a.Raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(a.Raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if back["id"] != "pr-raw" {
		t.Errorf("Raw.id = %v, want pr-raw", back["id"])
	}
}
