package cloudflare

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v4/load_balancers"
	"github.com/cloudflare/cloudflare-go/v4/zones"
)

func loadBalancerTestZone() zones.Zone {
	return zones.Zone{
		ID:      "zone-id-lb",
		Name:    "example.com",
		Account: zones.ZoneAccount{ID: "acct-id-lb", Name: "LB Account"},
	}
}

func TestLoadBalancerToAsset_BasicMapping(t *testing.T) {
	p := &Provider{}
	z := loadBalancerTestZone()
	lb := load_balancers.LoadBalancer{
		ID:              "lb-1",
		Name:            "lb.example.com",
		Enabled:         true,
		Proxied:         true,
		CreatedOn:       "2025-01-02T03:04:05Z",
		SteeringPolicy:  load_balancers.SteeringPolicyGeo,
		SessionAffinity: load_balancers.SessionAffinityCookie,
		FallbackPool:    "pool-fallback",
		DefaultPools:    []load_balancers.DefaultPools{"pool-a", "pool-b"},
	}

	a := p.loadBalancerToAsset(z, lb)

	if a.Provider != "cloudflare" {
		t.Errorf("Provider = %q, want cloudflare", a.Provider)
	}
	if a.Type != "cloudflare.load_balancer" {
		t.Errorf("Type = %q, want cloudflare.load_balancer", a.Type)
	}
	if a.AccountID != "acct-id-lb" {
		t.Errorf("AccountID = %q, want acct-id-lb (inherited from zone)", a.AccountID)
	}
	if a.ID != "lb-1" {
		t.Errorf("ID = %q, want lb-1", a.ID)
	}
	if a.Name != "lb.example.com" {
		t.Errorf("Name = %q, want lb.example.com (the LB's DNS hostname, verbatim)", a.Name)
	}
	if a.Status != "enabled" {
		t.Errorf("Status = %q, want enabled", a.Status)
	}
	wantCreated := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	if a.CreatedAt == nil || !a.CreatedAt.Equal(wantCreated) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt, wantCreated)
	}

	wantTags := map[string]string{
		"zone_id":          "zone-id-lb",
		"zone_name":        "example.com",
		"proxied":          "true",
		"steering_policy":  "geo",
		"session_affinity": "cookie",
		"fallback_pool":    "pool-fallback",
		"default_pools":    "pool-a,pool-b",
	}
	for k, want := range wantTags {
		if got := a.Tags[k]; got != want {
			t.Errorf("Tags[%q] = %q, want %q", k, got, want)
		}
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestLoadBalancerToAsset_DisabledStatus(t *testing.T) {
	p := &Provider{}
	a := p.loadBalancerToAsset(loadBalancerTestZone(), load_balancers.LoadBalancer{
		ID:      "lb-2",
		Name:    "down.example.com",
		Enabled: false,
		Proxied: false,
	})
	if a.Status != "disabled" {
		t.Errorf("Status = %q, want disabled", a.Status)
	}
	if a.Tags["proxied"] != "false" {
		t.Errorf("Tags[proxied] = %q, want false", a.Tags["proxied"])
	}
	if a.Tags["default_pools"] != "" {
		t.Errorf("Tags[default_pools] = %q, want empty for nil DefaultPools", a.Tags["default_pools"])
	}
}

func TestLoadBalancerToAsset_IncludeRawRoundTrip(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	a := p.loadBalancerToAsset(loadBalancerTestZone(), load_balancers.LoadBalancer{
		ID:   "lb-raw",
		Name: "raw.example.com",
	})
	if a.Raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(a.Raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if back["id"] != "lb-raw" {
		t.Errorf("Raw.id = %v, want lb-raw", back["id"])
	}
}

func TestLoadBalancerCreatedOn_Parsing(t *testing.T) {
	if got := loadBalancerCreatedOn(""); got != nil {
		t.Errorf("empty string should map to nil, got %v", got)
	}
	if got := loadBalancerCreatedOn("not-a-timestamp"); got != nil {
		t.Errorf("garbage should map to nil, got %v", got)
	}
	got := loadBalancerCreatedOn("2014-01-01T05:20:00.12345Z")
	want := time.Date(2014, 1, 1, 5, 20, 0, 123450000, time.UTC)
	if got == nil || !got.Equal(want) {
		t.Errorf("fractional-second timestamp: got %v, want %v", got, want)
	}
}
