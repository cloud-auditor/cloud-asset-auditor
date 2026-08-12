package netbird

import (
	"encoding/json"
	"strings"
	"testing"
)

// withAccount returns a Provider configured for mapping tests: include-raw on,
// a fixed account id so AccountID assertions are stable.
func withAccount() *Provider {
	return &Provider{cfg: Config{IncludeRaw: true}, accountID: "acct1"}
}

func TestPeerToAsset(t *testing.T) {
	p := withAccount()
	a := p.peerToAsset(peer{
		ID: "p1", Name: "stage-host-1", IP: "100.64.0.1", ConnectionIP: "35.0.0.1",
		Connected: true, Hostname: "stage-host-1", DNSLabel: "stage-host-1.netbird.cloud",
		OS: "linux", Groups: []groupRef{{ID: "g1", Name: "all"}, {ID: "g2", Name: "devs"}},
	})
	if a.Type != "netbird.peer" || a.ID != "p1" || a.AccountID != "acct1" {
		t.Fatalf("bad identity: %+v", a)
	}
	if a.Name != "stage-host-1" {
		t.Errorf("Name = %q", a.Name)
	}
	if a.Status != "connected" {
		t.Errorf("Status = %q, want connected", a.Status)
	}
	if a.Tags["ip"] != "100.64.0.1" || a.Tags["connection_ip"] != "35.0.0.1" {
		t.Errorf("missing ip tags: %v", a.Tags)
	}
	if a.Tags["groups"] != "all,devs" {
		t.Errorf("groups tag = %q, want all,devs", a.Tags["groups"])
	}
	if len(a.Raw) == 0 {
		t.Errorf("Raw should be populated when IncludeRaw is set")
	}
}

func TestPeerToAsset_DisconnectedAndNameFallback(t *testing.T) {
	p := withAccount()
	a := p.peerToAsset(peer{ID: "p2", Name: "", Hostname: "raw-host", Connected: false})
	if a.Status != "disconnected" {
		t.Errorf("Status = %q, want disconnected", a.Status)
	}
	if a.Name != "raw-host" {
		t.Errorf("Name = %q, want fallback to hostname", a.Name)
	}
}

func TestRouteToAsset(t *testing.T) {
	p := withAccount()
	a := p.routeToAsset(route{
		ID: "r1", NetworkID: "Route 1", Network: "10.64.0.0/24", NetworkType: "IPv4",
		Enabled: true, Peer: "p1", Groups: []string{"g1", "g2"}, Metric: 9999,
	})
	if a.Type != "netbird.route" || a.Name != "Route 1" || a.Status != "enabled" {
		t.Fatalf("bad route asset: %+v", a)
	}
	if a.Tags["network"] != "10.64.0.0/24" || a.Tags["groups"] != "g1,g2" || a.Tags["metric"] != "9999" {
		t.Errorf("missing route tags: %v", a.Tags)
	}
}

func TestPolicyToAsset_DisabledStatus(t *testing.T) {
	p := withAccount()
	a := p.policyToAsset(policy{ID: "pol1", Name: "deny-all", Enabled: false, Rules: []policyRule{{ID: "ru1"}}})
	if a.Status != "disabled" {
		t.Errorf("Status = %q, want disabled", a.Status)
	}
	if a.Tags["rules_count"] != "1" {
		t.Errorf("rules_count = %q, want 1", a.Tags["rules_count"])
	}
}

func TestAccountToAsset(t *testing.T) {
	p := withAccount()
	var ac account
	_ = json.Unmarshal([]byte(`{"id":"acct1","domain":"example.io","settings":{"network_range":"100.64.0.0/10"}}`), &ac)
	a := p.accountToAsset(ac)
	if a.Type != "netbird.account" || a.Name != "example.io" {
		t.Fatalf("bad account asset: %+v", a)
	}
	if a.Tags["network_range"] != "100.64.0.0/10" {
		t.Errorf("network_range tag = %q", a.Tags["network_range"])
	}
}

// The setup-key secret and the user password must never reach Asset.Raw or
// tags — the structs don't declare those fields, so JSON-decoding a payload
// that contains them drops them entirely.
func TestSecretsNeverLeak(t *testing.T) {
	p := withAccount()

	var k setupKey
	if err := json.Unmarshal([]byte(`{"id":"k1","name":"default","key":"nbp_SUPERSECRET","state":"valid","type":"reusable"}`), &k); err != nil {
		t.Fatal(err)
	}
	ka := p.setupKeyToAsset(k)
	if strings.Contains(string(ka.Raw), "nbp_SUPERSECRET") {
		t.Errorf("setup-key secret leaked into Raw: %s", ka.Raw)
	}
	for key, v := range ka.Tags {
		if strings.Contains(v, "nbp_SUPERSECRET") {
			t.Errorf("setup-key secret leaked into tag %q", key)
		}
	}
	if ka.Status != "valid" {
		t.Errorf("setup-key Status = %q, want valid", ka.Status)
	}

	var u user
	if err := json.Unmarshal([]byte(`{"id":"u1","name":"Tom","email":"t@x.io","status":"active","password":"hunter2"}`), &u); err != nil {
		t.Fatal(err)
	}
	ua := p.userToAsset(u)
	if strings.Contains(string(ua.Raw), "hunter2") {
		t.Errorf("user password leaked into Raw: %s", ua.Raw)
	}
}

func TestRawOmittedWhenIncludeRawOff(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: false}, accountID: "acct1"}
	a := p.peerToAsset(peer{ID: "p1", Name: "h"})
	if a.Raw != nil {
		t.Errorf("Raw must be nil when IncludeRaw is off, got %s", a.Raw)
	}
}

func TestTagsOf_DropsEmpty(t *testing.T) {
	got := tagsOf("a", "1", "b", "", "c", "3")
	if _, ok := got["b"]; ok {
		t.Errorf("empty-valued tag should be dropped: %v", got)
	}
	if got["a"] != "1" || got["c"] != "3" {
		t.Errorf("unexpected tags: %v", got)
	}
	if tagsOf("only") != nil || tagsOf() != nil {
		t.Errorf("tagsOf with no surviving pairs should return nil")
	}
}
