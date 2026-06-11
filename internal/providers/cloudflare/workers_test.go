package cloudflare

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v4/workers"
)

func TestWorkerScriptToAsset_BasicMapping(t *testing.T) {
	p := &Provider{}
	created := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	s := workers.Script{
		ID:         "my-worker",
		CreatedOn:  created,
		Logpush:    true,
		UsageModel: workers.ScriptUsageModelStandard,
		Placement: workers.ScriptPlacement{
			Mode: workers.ScriptPlacementModeSmart,
		},
	}

	a := p.workerScriptToAsset("acct-w-1", s)

	if a.Provider != "cloudflare" {
		t.Errorf("Provider = %q, want cloudflare", a.Provider)
	}
	if a.Type != "cloudflare.worker_script" {
		t.Errorf("Type = %q, want cloudflare.worker_script", a.Type)
	}
	if a.AccountID != "acct-w-1" {
		t.Errorf("AccountID = %q, want acct-w-1", a.AccountID)
	}
	if a.ID != "my-worker" || a.Name != "my-worker" {
		t.Errorf("ID/Name = %q / %q, want my-worker for both (script names are the ID)", a.ID, a.Name)
	}
	if a.Status != "" {
		t.Errorf("Status = %q, want empty (worker scripts have no natural status)", a.Status)
	}
	if a.CreatedAt == nil || !a.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt, created)
	}
	if a.Tags["usage_model"] != "standard" {
		t.Errorf("Tags[usage_model] = %q, want standard", a.Tags["usage_model"])
	}
	if a.Tags["logpush"] != "true" {
		t.Errorf("Tags[logpush] = %q, want true", a.Tags["logpush"])
	}
	if a.Tags["placement_mode"] != "smart" {
		t.Errorf("Tags[placement_mode] = %q, want smart", a.Tags["placement_mode"])
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestWorkerScriptToAsset_DeprecatedPlacementModeFallback(t *testing.T) {
	p := &Provider{}
	s := workers.Script{
		ID:            "legacy-worker",
		PlacementMode: workers.ScriptPlacementModeSmart, // nested Placement.Mode unset
	}

	a := p.workerScriptToAsset("acct-w-1", s)

	if a.Tags["placement_mode"] != "smart" {
		t.Errorf("Tags[placement_mode] = %q, want smart (deprecated field fallback)", a.Tags["placement_mode"])
	}
}

func TestWorkerScriptToAsset_NoPlacementOmitsTag(t *testing.T) {
	p := &Provider{}
	s := workers.Script{ID: "plain-worker", Logpush: false}

	a := p.workerScriptToAsset("acct-w-1", s)

	if _, ok := a.Tags["placement_mode"]; ok {
		t.Errorf("Tags[placement_mode] should be absent when unset, got %q", a.Tags["placement_mode"])
	}
	if a.Tags["logpush"] != "false" {
		t.Errorf("Tags[logpush] = %q, want false", a.Tags["logpush"])
	}
}

func TestWorkerScriptToAsset_ZeroCreatedOnOmitted(t *testing.T) {
	p := &Provider{}
	a := p.workerScriptToAsset("acct-w-1", workers.Script{ID: "w"})
	if a.CreatedAt != nil {
		t.Errorf("CreatedAt should be nil for zero time, got %v", a.CreatedAt)
	}
}

func TestWorkerScriptToAsset_IncludeRawRoundTrip(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	s := workers.Script{
		ID:         "raw-worker",
		Logpush:    true,
		UsageModel: workers.ScriptUsageModelStandard,
	}

	a := p.workerScriptToAsset("acct-w-1", s)
	if a.Raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(a.Raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if back["id"] != "raw-worker" {
		t.Errorf("Raw.id = %v, want raw-worker", back["id"])
	}
}
