package cloudflare

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v4/pages"
)

func TestPagesProjectToAsset_BasicMapping(t *testing.T) {
	p := &Provider{}
	created := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	pr := pages.Project{
		ID:               "proj-id-1",
		Name:             "my-site",
		ProductionBranch: "main",
		Subdomain:        "my-site.pages.dev",
		Domains:          []string{"example.com", "www.example.com"},
		CreatedOn:        created,
	}

	a := p.pagesProjectToAsset("acct-1", pr)

	if a.Provider != "cloudflare" {
		t.Errorf("Provider = %q, want cloudflare", a.Provider)
	}
	if a.Type != "cloudflare.pages_project" {
		t.Errorf("Type = %q, want cloudflare.pages_project", a.Type)
	}
	if a.AccountID != "acct-1" {
		t.Errorf("AccountID = %q, want acct-1", a.AccountID)
	}
	if a.ID != "proj-id-1" {
		t.Errorf("ID = %q, want proj-id-1", a.ID)
	}
	if a.Name != "my-site" {
		t.Errorf("Name = %q, want my-site", a.Name)
	}
	if a.Status != "" {
		t.Errorf("Status = %q, want empty (Pages projects have no natural status)", a.Status)
	}
	if a.CreatedAt == nil || !a.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt, created)
	}
	if a.Tags["production_branch"] != "main" {
		t.Errorf("Tags[production_branch] = %q, want main", a.Tags["production_branch"])
	}
	if a.Tags["subdomain"] != "my-site.pages.dev" {
		t.Errorf("Tags[subdomain] = %q, want my-site.pages.dev", a.Tags["subdomain"])
	}
	if a.Tags["domains"] != "example.com,www.example.com" {
		t.Errorf("Tags[domains] = %q, want example.com,www.example.com", a.Tags["domains"])
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestPagesProjectToAsset_IDFallsBackToName(t *testing.T) {
	p := &Provider{}
	a := p.pagesProjectToAsset("acct-1", pages.Project{Name: "no-id-project"})
	if a.ID != "no-id-project" {
		t.Errorf("ID = %q, want fallback to name no-id-project", a.ID)
	}
}

func TestPagesProjectToAsset_ZeroCreatedOnOmitted(t *testing.T) {
	p := &Provider{}
	a := p.pagesProjectToAsset("acct-1", pages.Project{ID: "x", Name: "y"})
	if a.CreatedAt != nil {
		t.Errorf("CreatedAt should be nil for zero time, got %v", a.CreatedAt)
	}
}

func TestPagesProjectToAsset_IncludeRawRoundTrip(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	pr := pages.Project{
		ID:        "proj-1",
		Name:      "raw-site",
		Subdomain: "raw-site.pages.dev",
	}

	a := p.pagesProjectToAsset("acct-1", pr)
	if a.Raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(a.Raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if back["name"] != "raw-site" {
		t.Errorf("Raw.name = %v, want raw-site", back["name"])
	}
	if back["subdomain"] != "raw-site.pages.dev" {
		t.Errorf("Raw.subdomain = %v, want raw-site.pages.dev", back["subdomain"])
	}
}

// The list endpoint is typed as []pages.Deployment by the v4 SDK even though
// the wire JSON is project objects. This test simulates the SDK decode path
// (json.Unmarshal routes through apijson and preserves raw JSON) and asserts
// the project-only fields are recovered.
func TestPagesProjectFromListItem_RecoversProjectFieldsFromRawJSON(t *testing.T) {
	wire := `{
		"id": "proj-id-9",
		"name": "wire-site",
		"subdomain": "wire-site.pages.dev",
		"domains": ["wire.example.com"],
		"production_branch": "release",
		"created_on": "2025-03-04T05:06:07Z"
	}`
	var d pages.Deployment
	if err := json.Unmarshal([]byte(wire), &d); err != nil {
		t.Fatalf("unmarshal wire JSON into Deployment: %v", err)
	}
	if d.JSON.RawJSON() == "" {
		t.Fatal("precondition: SDK unmarshal should preserve raw JSON")
	}

	pr := pagesProjectFromListItem(d)

	if pr.ID != "proj-id-9" {
		t.Errorf("ID = %q, want proj-id-9", pr.ID)
	}
	if pr.Name != "wire-site" {
		t.Errorf("Name = %q, want wire-site (only present in raw JSON)", pr.Name)
	}
	if pr.Subdomain != "wire-site.pages.dev" {
		t.Errorf("Subdomain = %q, want wire-site.pages.dev", pr.Subdomain)
	}
	if pr.ProductionBranch != "release" {
		t.Errorf("ProductionBranch = %q, want release", pr.ProductionBranch)
	}
	if len(pr.Domains) != 1 || pr.Domains[0] != "wire.example.com" {
		t.Errorf("Domains = %v, want [wire.example.com]", pr.Domains)
	}
	want := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	if !pr.CreatedOn.Equal(want) {
		t.Errorf("CreatedOn = %v, want %v", pr.CreatedOn, want)
	}
}

func TestPagesProjectFromListItem_FallbackWithoutRawJSON(t *testing.T) {
	created := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	// A literal struct has no raw JSON metadata, exercising the fallback.
	d := pages.Deployment{
		ID:          "dep-id-1",
		ProjectName: "fallback-site",
		CreatedOn:   created,
	}

	pr := pagesProjectFromListItem(d)

	if pr.ID != "dep-id-1" {
		t.Errorf("ID = %q, want dep-id-1", pr.ID)
	}
	if pr.Name != "fallback-site" {
		t.Errorf("Name = %q, want fallback-site", pr.Name)
	}
	if !pr.CreatedOn.Equal(created) {
		t.Errorf("CreatedOn = %v, want %v", pr.CreatedOn, created)
	}
}

func TestPagesProjectFromListItem_EndToEndAsset(t *testing.T) {
	p := &Provider{}
	var d pages.Deployment
	if err := json.Unmarshal([]byte(`{"id":"p1","name":"site","subdomain":"site.pages.dev","domains":["a.com","b.com"],"production_branch":"main"}`), &d); err != nil {
		t.Fatal(err)
	}

	a := p.pagesProjectToAsset("acct-9", pagesProjectFromListItem(d))

	if a.ID != "p1" || a.Name != "site" || a.AccountID != "acct-9" {
		t.Errorf("id/name/account = %q / %q / %q", a.ID, a.Name, a.AccountID)
	}
	if a.Tags["domains"] != "a.com,b.com" {
		t.Errorf("Tags[domains] = %q, want a.com,b.com", a.Tags["domains"])
	}
}
