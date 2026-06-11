package cloudflare

import (
	"encoding/json"
	"testing"

	"github.com/cloudflare/cloudflare-go/v4/kv"
)

func TestKVNamespaceToAsset_BasicMapping(t *testing.T) {
	p := &Provider{}
	ns := kv.Namespace{
		ID:                  "kv-ns-1",
		Title:               "session-store",
		Beta:                true,
		SupportsURLEncoding: true,
	}

	a := p.kvNamespaceToAsset("acct-id-1", ns)

	if a.Provider != "cloudflare" {
		t.Errorf("Provider = %q, want cloudflare", a.Provider)
	}
	if a.Type != "cloudflare.kv_namespace" {
		t.Errorf("Type = %q, want cloudflare.kv_namespace", a.Type)
	}
	if a.AccountID != "acct-id-1" {
		t.Errorf("AccountID = %q, want acct-id-1", a.AccountID)
	}
	if a.ID != "kv-ns-1" {
		t.Errorf("ID = %q, want kv-ns-1", a.ID)
	}
	if a.Name != "session-store" {
		t.Errorf("Name = %q, want session-store", a.Name)
	}
	if a.Status != "" {
		t.Errorf("Status = %q, want empty (KV namespaces have no status)", a.Status)
	}
	if a.CreatedAt != nil {
		t.Errorf("CreatedAt = %v, want nil (SDK exposes no creation time)", a.CreatedAt)
	}
	if a.Tags["supports_url_encoding"] != "true" {
		t.Errorf("Tags[supports_url_encoding] = %q, want true", a.Tags["supports_url_encoding"])
	}
	if a.Tags["beta"] != "true" {
		t.Errorf("Tags[beta] = %q, want true", a.Tags["beta"])
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestKVNamespaceToAsset_FalseBooleansStringified(t *testing.T) {
	p := &Provider{}
	ns := kv.Namespace{ID: "kv-ns-2", Title: "plain"}

	a := p.kvNamespaceToAsset("acct-id-2", ns)

	if a.Tags["supports_url_encoding"] != "false" {
		t.Errorf("Tags[supports_url_encoding] = %q, want false", a.Tags["supports_url_encoding"])
	}
	if a.Tags["beta"] != "false" {
		t.Errorf("Tags[beta] = %q, want false", a.Tags["beta"])
	}
}

func TestKVNamespaceToAsset_IncludeRawRoundTrip(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	ns := kv.Namespace{
		ID:                  "kv-ns-3",
		Title:               "config-cache",
		SupportsURLEncoding: true,
	}

	a := p.kvNamespaceToAsset("acct-id-3", ns)
	if a.Raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(a.Raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if back["id"] != "kv-ns-3" {
		t.Errorf("Raw.id = %v, want kv-ns-3", back["id"])
	}
	if back["title"] != "config-cache" {
		t.Errorf("Raw.title = %v, want config-cache", back["title"])
	}
	if back["supports_url_encoding"] != true {
		t.Errorf("Raw.supports_url_encoding = %v, want true", back["supports_url_encoding"])
	}
}
