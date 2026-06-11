package cloudflare

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v4/zero_trust"
)

func TestAccessAppToAsset_BasicMapping(t *testing.T) {
	p := &Provider{}
	created := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	app := zero_trust.AccessApplicationListResponse{
		ID:              "app-uuid-1",
		Name:            "Internal Wiki",
		Domain:          "wiki.example.com",
		Type:            zero_trust.ApplicationTypeSelfHosted,
		AUD:             "aud-tag-123",
		SessionDuration: "24h",
		CreatedAt:       created,
	}

	a := p.accessAppToAsset("acct-1", app)

	if a.Provider != "cloudflare" {
		t.Errorf("Provider = %q, want cloudflare", a.Provider)
	}
	if a.Type != "cloudflare.access_app" {
		t.Errorf("Type = %q, want cloudflare.access_app", a.Type)
	}
	if a.AccountID != "acct-1" {
		t.Errorf("AccountID = %q, want acct-1", a.AccountID)
	}
	if a.ID != "app-uuid-1" {
		t.Errorf("ID = %q, want app-uuid-1", a.ID)
	}
	if a.Name != "Internal Wiki" {
		t.Errorf("Name = %q, want Internal Wiki", a.Name)
	}
	if a.Status != "" {
		t.Errorf("Status = %q, want empty (no natural status)", a.Status)
	}
	if a.CreatedAt == nil || !a.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt, created)
	}
	if a.Tags["domain"] != "wiki.example.com" {
		t.Errorf("Tags[domain] = %q, want wiki.example.com", a.Tags["domain"])
	}
	if a.Tags["type"] != "self_hosted" {
		t.Errorf("Tags[type] = %q, want self_hosted", a.Tags["type"])
	}
	if a.Tags["aud"] != "aud-tag-123" {
		t.Errorf("Tags[aud] = %q, want aud-tag-123", a.Tags["aud"])
	}
	if a.Tags["session_duration"] != "24h" {
		t.Errorf("Tags[session_duration] = %q, want 24h", a.Tags["session_duration"])
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestAccessAppToAsset_OmitsAbsentTagsAndZeroTime(t *testing.T) {
	p := &Provider{}
	app := zero_trust.AccessApplicationListResponse{
		ID:   "app-uuid-2",
		Name: "Bare App",
	}

	a := p.accessAppToAsset("acct-1", app)

	if a.CreatedAt != nil {
		t.Errorf("CreatedAt should be nil for zero time, got %v", a.CreatedAt)
	}
	for _, k := range []string{"domain", "type", "aud", "session_duration"} {
		if _, ok := a.Tags[k]; ok {
			t.Errorf("Tags[%s] should be absent when the field is empty, got %q", k, a.Tags[k])
		}
	}
}

func TestAccessAppToAsset_ComposedIDFallback(t *testing.T) {
	p := &Provider{}
	app := zero_trust.AccessApplicationListResponse{
		Name: "No-ID Bookmark",
		Type: zero_trust.ApplicationTypeBookmark,
	}

	a := p.accessAppToAsset("acct-9", app)

	if a.ID != "acct-9/No-ID Bookmark" {
		t.Errorf("ID = %q, want acct-9/No-ID Bookmark", a.ID)
	}
}

func TestAccessAppToAsset_IncludeRawRoundTrip(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	app := zero_trust.AccessApplicationListResponse{
		ID:     "app-uuid-3",
		Name:   "Raw App",
		Domain: "raw.example.com",
		Type:   zero_trust.ApplicationTypeSaaS,
	}

	a := p.accessAppToAsset("acct-1", app)

	if a.Raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(a.Raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if back["id"] != "app-uuid-3" {
		t.Errorf("Raw.id = %v, want app-uuid-3", back["id"])
	}
	if back["domain"] != "raw.example.com" {
		t.Errorf("Raw.domain = %v, want raw.example.com", back["domain"])
	}
}
