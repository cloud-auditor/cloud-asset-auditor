package cloudflare

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v4/zero_trust"
)

func tunnelTestFixture(created time.Time) zero_trust.TunnelCloudflaredListResponse {
	return zero_trust.TunnelCloudflaredListResponse{
		ID:           "f70ff985-a4ef-4643-bbbc-4a0ed4fc8415",
		AccountTag:   "acct-tunnel-1",
		Name:         "blog-tunnel",
		Status:       zero_trust.TunnelCloudflaredListResponseStatusHealthy,
		TunType:      zero_trust.TunnelCloudflaredListResponseTunTypeCfdTunnel,
		RemoteConfig: true,
		CreatedAt:    created,
	}
}

func TestTunnelToAsset_BasicMapping(t *testing.T) {
	p := &Provider{}
	created := time.Date(2025, 3, 14, 9, 26, 53, 0, time.UTC)
	tun := tunnelTestFixture(created)

	a := p.tunnelToAsset("acct-tunnel-1", tun)

	if a.Provider != "cloudflare" {
		t.Errorf("Provider = %q, want cloudflare", a.Provider)
	}
	if a.Type != "cloudflare.tunnel" {
		t.Errorf("Type = %q, want cloudflare.tunnel", a.Type)
	}
	if a.ID != "f70ff985-a4ef-4643-bbbc-4a0ed4fc8415" {
		t.Errorf("ID = %q, want tunnel UUID", a.ID)
	}
	if a.Name != "blog-tunnel" {
		t.Errorf("Name = %q, want blog-tunnel", a.Name)
	}
	if a.AccountID != "acct-tunnel-1" {
		t.Errorf("AccountID = %q, want acct-tunnel-1", a.AccountID)
	}
	if a.Status != "healthy" {
		t.Errorf("Status = %q, want healthy", a.Status)
	}
	if a.CreatedAt == nil || !a.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt, created)
	}
	if a.Tags["tun_type"] != "cfd_tunnel" {
		t.Errorf("Tags[tun_type] = %q, want cfd_tunnel", a.Tags["tun_type"])
	}
	if a.Tags["remote_config"] != "true" {
		t.Errorf("Tags[remote_config] = %q, want true", a.Tags["remote_config"])
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestTunnelToAsset_ZeroCreatedAtOmitted(t *testing.T) {
	p := &Provider{}
	tun := zero_trust.TunnelCloudflaredListResponse{
		ID:   "uuid-1",
		Name: "no-created",
	}
	a := p.tunnelToAsset("acct-1", tun)
	if a.CreatedAt != nil {
		t.Errorf("CreatedAt should be nil for zero time, got %v", a.CreatedAt)
	}
}

func TestTunnelToAsset_LocalConfigWARPConnector(t *testing.T) {
	p := &Provider{}
	tun := zero_trust.TunnelCloudflaredListResponse{
		ID:           "uuid-2",
		Name:         "warp-tun",
		Status:       zero_trust.TunnelCloudflaredListResponseStatusDown,
		TunType:      zero_trust.TunnelCloudflaredListResponseTunTypeWARPConnector,
		RemoteConfig: false,
	}
	a := p.tunnelToAsset("acct-2", tun)
	if a.Status != "down" {
		t.Errorf("Status = %q, want down", a.Status)
	}
	if a.Tags["tun_type"] != "warp_connector" {
		t.Errorf("Tags[tun_type] = %q, want warp_connector", a.Tags["tun_type"])
	}
	if a.Tags["remote_config"] != "false" {
		t.Errorf("Tags[remote_config] = %q, want false", a.Tags["remote_config"])
	}
}

func TestTunnelToAsset_IncludeRawRoundTrip(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	created := time.Date(2025, 3, 14, 9, 26, 53, 0, time.UTC)
	tun := tunnelTestFixture(created)

	a := p.tunnelToAsset("acct-tunnel-1", tun)
	if a.Raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(a.Raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if back["id"] != "f70ff985-a4ef-4643-bbbc-4a0ed4fc8415" {
		t.Errorf("Raw.id = %v, want tunnel UUID", back["id"])
	}
	if back["name"] != "blog-tunnel" {
		t.Errorf("Raw.name = %v, want blog-tunnel", back["name"])
	}
}
