package cloudflare

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v4/d1"
)

func TestD1DatabaseToAsset_BasicMapping(t *testing.T) {
	p := &Provider{}
	created := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	db := d1.DatabaseListResponse{
		UUID:      "d1-uuid-1",
		Name:      "prod-db",
		Version:   "production",
		CreatedAt: created,
	}

	a := p.d1DatabaseToAsset("acct-1", db)

	if a.Provider != "cloudflare" {
		t.Errorf("Provider = %q, want cloudflare", a.Provider)
	}
	if a.Type != "cloudflare.d1_database" {
		t.Errorf("Type = %q, want cloudflare.d1_database", a.Type)
	}
	if a.AccountID != "acct-1" {
		t.Errorf("AccountID = %q, want acct-1", a.AccountID)
	}
	if a.ID != "d1-uuid-1" {
		t.Errorf("ID = %q, want d1-uuid-1", a.ID)
	}
	if a.Name != "prod-db" {
		t.Errorf("Name = %q, want prod-db", a.Name)
	}
	if a.Status != "" {
		t.Errorf("Status = %q, want empty (D1 has no natural status)", a.Status)
	}
	if a.CreatedAt == nil || !a.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt, created)
	}
	if a.Tags["version"] != "production" {
		t.Errorf("Tags[version] = %q, want production", a.Tags["version"])
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestD1DatabaseToAsset_ZeroCreatedAtOmitted(t *testing.T) {
	p := &Provider{}
	a := p.d1DatabaseToAsset("acct-1", d1.DatabaseListResponse{UUID: "u", Name: "n"})
	if a.CreatedAt != nil {
		t.Errorf("CreatedAt should be nil for zero time, got %v", a.CreatedAt)
	}
}

func TestD1DatabaseToAsset_EmptyVersionLeavesTagsNil(t *testing.T) {
	p := &Provider{}
	a := p.d1DatabaseToAsset("acct-1", d1.DatabaseListResponse{UUID: "u", Name: "n"})
	if a.Tags != nil {
		t.Errorf("Tags should be nil when version is empty, got %v", a.Tags)
	}
}

func TestD1DatabaseToAsset_IncludeRawRoundTrip(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	db := d1.DatabaseListResponse{
		UUID:    "d1-uuid-raw",
		Name:    "raw-db",
		Version: "beta",
	}

	a := p.d1DatabaseToAsset("acct-raw", db)
	if a.Raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(a.Raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if back["uuid"] != "d1-uuid-raw" {
		t.Errorf("Raw.uuid = %v, want d1-uuid-raw", back["uuid"])
	}
	if back["name"] != "raw-db" {
		t.Errorf("Raw.name = %v, want raw-db", back["name"])
	}
}
