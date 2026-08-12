package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

const testToken = "tskey-api-testtoken"

// fakeAPI is a minimal Tailscale v2 API: it checks the Bearer auth header and
// serves canned JSON for every endpoint the provider hits. Paths are the
// default-tailnet ("-") forms the provider builds.
func fakeAPI(t *testing.T) *httptest.Server {
	t.Helper()
	bodies := map[string]string{
		"/api/v2/tailnet/-/devices": `{"devices":[{
			"id":"92960230385","nodeId":"n292kg92CNTRL","name":"web.tailfe8c.ts.net",
			"hostname":"web","user":"amelie@example.com","os":"linux",
			"addresses":["100.87.74.78","fd7a:115c:a1e0::1"],
			"connectedToControl":true,"authorized":true,"clientVersion":"v1.36.0",
			"created":"2022-12-01T05:23:30Z","advertisedRoutes":["10.0.0.0/16"],
			"enabledRoutes":["10.0.0.0/16"],"tags":["tag:prod"]}]}`,
		"/api/v2/tailnet/-/users": `{"users":[{
			"id":"123456","displayName":"Some User","loginName":"someuser@example.com",
			"role":"member","status":"active","deviceCount":4}]}`,
		"/api/v2/tailnet/-/keys": `{"keys":[{
			"id":"k123456CNTRL","keyType":"auth","description":"dev access",
			"created":"2021-12-09T23:22:39Z","expires":"2999-03-09T23:22:39Z",
			"capabilities":{"devices":{"create":{"reusable":true,"tags":["tag:prod"]}}}}]}`,
		"/api/v2/tailnet/-/dns/nameservers": `{"dns":["8.8.8.8","1.1.1.1"]}`,
		"/api/v2/tailnet/-/dns/searchpaths": `{"searchPaths":["example.com"]}`,
		"/api/v2/tailnet/-/dns/preferences": `{"magicDNS":true}`,
		"/api/v2/tailnet/-/acl": `{
			"acls":[{"action":"accept","src":["group:eng"],"dst":["tag:prod:22"],"proto":"tcp"}],
			"grants":[{"src":["autogroup:member"],"dst":["tag:db"],"ip":["5432"]}],
			"ssh":[{"action":"check","src":["autogroup:member"],"dst":["autogroup:self"],"users":["root"]}],
			"groups":{"group:eng":["a@example.com","b@example.com"]},
			"tagOwners":{"tag:prod":["group:eng"]},
			"hosts":{"bastion":"100.64.0.9"}}`,
	}
	mux := http.NewServeMux()
	for path, body := range bodies {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testToken {
				http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func collectAll(t *testing.T, p *Provider) ([]core.Asset, []error) {
	t.Helper()
	assets, errs := p.Collect(context.Background())
	var (
		got  []core.Asset
		bad  []error
		done = make(chan struct{})
	)
	go func() {
		for e := range errs {
			bad = append(bad, e)
		}
		close(done)
	}()
	for a := range assets {
		got = append(got, a)
	}
	<-done
	return got, bad
}

func TestCollect_EndToEnd(t *testing.T) {
	srv := fakeAPI(t)
	p, err := New(Config{APIKey: testToken, BaseURL: srv.URL, IncludeRaw: true})
	if err != nil {
		t.Fatal(err)
	}

	got, bad := collectAll(t, p)
	for _, e := range bad {
		t.Errorf("unexpected collect error: %v", e)
	}

	byType := map[string][]core.Asset{}
	for _, a := range got {
		byType[a.Type] = append(byType[a.Type], a)
	}
	for _, want := range []string{
		"tailscale.device", "tailscale.user", "tailscale.key", "tailscale.dns",
		"tailscale.acl", "tailscale.acl_rule", "tailscale.acl_group",
		"tailscale.acl_tag", "tailscale.acl_host",
	} {
		if len(byType[want]) == 0 {
			t.Errorf("missing asset type %q (got types %v)", want, keysOf(byType))
		}
	}

	dev := byType["tailscale.device"][0]
	// nodeId is preferred over the legacy numeric id.
	if dev.ID != "n292kg92CNTRL" {
		t.Errorf("device ID = %q, want the nodeId n292kg92CNTRL", dev.ID)
	}
	if dev.Status != "connected" {
		t.Errorf("device Status = %q, want connected", dev.Status)
	}
	// v4 and v6 must be split, not just "first address wins".
	if got, want := dev.Tags["ip"], "100.87.74.78"; got != want {
		t.Errorf("device ip tag = %q, want %q", got, want)
	}
	if got, want := dev.Tags["ipv6"], "fd7a:115c:a1e0::1"; got != want {
		t.Errorf("device ipv6 tag = %q, want %q", got, want)
	}
	if got, want := dev.Tags["advertised_routes"], "10.0.0.0/16"; got != want {
		t.Errorf("device advertised_routes = %q, want %q", got, want)
	}

	// acls + grants + ssh = 3 rules, each its own asset.
	if n := len(byType["tailscale.acl_rule"]); n != 3 {
		t.Errorf("got %d acl_rule assets, want 3 (1 acl + 1 grant + 1 ssh)", n)
	}
	if got := byType["tailscale.dns"][0].Tags["nameservers"]; got != "8.8.8.8,1.1.1.1" {
		t.Errorf("dns nameservers tag = %q", got)
	}
}

// A grant has no `action` field — it is an allow by definition — so the
// mapper must not leave the status empty.
func TestRuleToAsset_GrantDefaultsToAccept(t *testing.T) {
	p := mustProvider(t)
	a := p.ruleToAsset("grant", 0, aclRule{Src: []string{"x"}, Dst: []string{"y"}})
	if a.Status != "accept" {
		t.Errorf("grant Status = %q, want accept", a.Status)
	}
	if a.Tags["rule_kind"] != "grant" {
		t.Errorf("rule_kind = %q, want grant", a.Tags["rule_kind"])
	}
}

// Policy assets derive from Go maps, whose iteration order is randomised.
// Two runs over the same policy must produce the same asset order or every
// downstream diff reports phantom drift.
func TestPolicyAssets_Deterministic(t *testing.T) {
	p := mustProvider(t)
	pol := aclPolicy{
		Groups:    map[string][]string{"group:z": {"z@x"}, "group:a": {"a@x"}, "group:m": {"m@x"}},
		TagOwners: map[string][]string{"tag:z": {"o"}, "tag:a": {"o"}},
		Hosts:     map[string]string{"zed": "100.0.0.2", "alpha": "100.0.0.1"},
	}
	first := idsOf(p.policyAssets(pol))
	for i := range 20 {
		if got := idsOf(p.policyAssets(pol)); !equal(got, first) {
			t.Fatalf("run %d produced a different asset order:\n got %v\nwant %v", i, got, first)
		}
	}
}

// The ACL endpoint 404s on a tailnet with no custom policy. That's "nothing
// configured", not a failure — it must not surface as a collect error.
func TestCollectACL_NotFoundIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"tailnet not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	p, err := New(Config{APIKey: testToken, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan core.Asset, 8)
	if err := p.collectACL(context.Background(), out); err != nil {
		t.Errorf("collectACL on 404 = %v, want nil", err)
	}
}

// Two of the three DNS endpoints failing still yields a usable asset; the
// failures ride along as errors (invariant 5: partial failure is normal).
func TestCollectDNS_PartialFailureStillEmits(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/tailnet/-/dns/nameservers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"dns":["9.9.9.9"]}`)
	})
	mux.HandleFunc("/api/v2/tailnet/-/dns/searchpaths", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	mux.HandleFunc("/api/v2/tailnet/-/dns/preferences", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, err := New(Config{APIKey: testToken, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan core.Asset, 8)
	err = p.collectDNS(context.Background(), out)
	if err == nil {
		t.Error("collectDNS should report the two failed sub-reads")
	}
	select {
	case a := <-out:
		if a.Tags["nameservers"] != "9.9.9.9" {
			t.Errorf("partial DNS asset lost the nameserver it did read: %v", a.Tags)
		}
	default:
		t.Error("collectDNS dropped the asset entirely on a partial failure")
	}
}

func TestKeyStatus(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		key  authKey
		want string
	}{
		{"revoked wins", authKey{Revoked: "2023-01-01T00:00:00Z", Expires: "2023-06-01T00:00:00Z"}, "revoked"},
		{"expired", authKey{Expires: "2023-06-01T00:00:00Z"}, "expired"},
		{"invalid", authKey{Invalid: true}, "invalid"},
		{"active", authKey{Expires: "2030-01-01T00:00:00Z"}, "active"},
		// An unparseable expiry must not be reported as expired — that would
		// call a live key dead.
		{"unparseable expiry is not expired", authKey{Expires: "not-a-date"}, "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keyStatus(tc.key, now); got != tc.want {
				t.Errorf("keyStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

// Node secrets must be structurally unreachable: the device/key structs have
// no field for them, so --include-raw cannot round-trip them into Asset.Raw
// even when the API returns them.
func TestRaw_OmitsNodeSecrets(t *testing.T) {
	p := mustProvider(t)
	p.cfg.IncludeRaw = true

	var d device
	if err := json.Unmarshal([]byte(`{"nodeId":"n1","machineKey":"mkey:SECRET","nodeKey":"nodekey:SECRET"}`), &d); err != nil {
		t.Fatal(err)
	}
	if raw := string(p.deviceToAsset(d).Raw); strings.Contains(raw, "SECRET") {
		t.Errorf("device Raw leaked a node key: %s", raw)
	}

	var k authKey
	if err := json.Unmarshal([]byte(`{"id":"k1","key":"tskey-auth-SECRET"}`), &k); err != nil {
		t.Fatal(err)
	}
	if raw := string(p.keyToAsset(k, time.Now()).Raw); strings.Contains(raw, "SECRET") {
		t.Errorf("key Raw leaked the secret material: %s", raw)
	}
}

func TestValidate_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"requires authentication"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	p, err := New(Config{APIKey: "wrong", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	err = p.Validate(context.Background())
	if err == nil {
		t.Fatal("Validate should fail on 401")
	}
	if !isAuthError(err) {
		t.Errorf("expected the wrapped error to still classify as an auth error, got %v", err)
	}
}

func TestNew_RequiresAPIKey(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New must reject an empty APIKey")
	}
}

// The default tailnet sentinel "-" doubles as the AccountID, so assets from a
// token-default tailnet still carry a stable, predictable account label.
func TestNew_DefaultsTailnetToSentinel(t *testing.T) {
	p := mustProvider(t)
	if p.cfg.Tailnet != defaultTailnet {
		t.Errorf("Tailnet = %q, want the %q sentinel", p.cfg.Tailnet, defaultTailnet)
	}
	if got := p.tailnetPath("/devices"); got != "/api/v2/tailnet/-/devices" {
		t.Errorf("tailnetPath = %q", got)
	}
}

// The tailnet rides in a path segment, so it's escaped. "@" is a legal
// sub-delim in a segment and survives verbatim (legacy org names are
// email-shaped, and escaping it would break them); a "/" would forge an extra
// segment and must not.
func TestTailnetPath_EscapesName(t *testing.T) {
	for _, tc := range []struct{ tailnet, want string }{
		{"me@example.com", "/api/v2/tailnet/me@example.com/devices"},
		{"a/b", "/api/v2/tailnet/a%2Fb/devices"},
	} {
		p := mustProvider(t)
		p.SetTailnet(tc.tailnet)
		if got := p.tailnetPath("/devices"); got != tc.want {
			t.Errorf("tailnetPath(%q) = %q, want %q", tc.tailnet, got, tc.want)
		}
	}
}

// Empty setter values must be no-ops, or an unset flag would clobber the
// env-derived configuration (applyProviderOptions calls the setters
// unconditionally).
func TestSetters_EmptyValuesAreNoOps(t *testing.T) {
	p := mustProvider(t)
	p.SetTailnet("acme.com")
	p.SetAPIBaseURL("https://headscale.internal")
	before := *p.client

	p.SetTailnet("")
	p.SetAPIBaseURL("")
	p.SetMaxConcurrency(0)

	if p.cfg.Tailnet != "acme.com" {
		t.Errorf("empty SetTailnet clobbered the tailnet: %q", p.cfg.Tailnet)
	}
	if p.client.baseURL != before.baseURL {
		t.Errorf("empty SetAPIBaseURL rebuilt the client: %q", p.client.baseURL)
	}
	if p.cfg.MaxConcurrency != defaultMaxConcurrency {
		t.Errorf("SetMaxConcurrency(0) clobbered concurrency: %d", p.cfg.MaxConcurrency)
	}
}

func TestClient_RedactsToken(t *testing.T) {
	c := newClient("https://api.tailscale.com", "tskey-api-SECRET")
	redacted := c.redact(errors.New("dial https://api.tailscale.com with tskey-api-SECRET failed"))
	if strings.Contains(redacted.Error(), "tskey-api-SECRET") {
		t.Errorf("redact left the token in: %q", redacted.Error())
	}
	if !strings.Contains(redacted.Error(), "***") {
		t.Errorf("redact should mask the token with ***: %q", redacted.Error())
	}
}

func TestNewClient_DefaultsBaseURLAndTrimsSlash(t *testing.T) {
	if got := newClient("", "t").baseURL; got != defaultBaseURL {
		t.Errorf("empty base URL = %q, want %q", got, defaultBaseURL)
	}
	if got := newClient("https://hs.example.com/", "t").baseURL; got != "https://hs.example.com" {
		t.Errorf("trailing slash not trimmed: %q", got)
	}
}

// --- helpers ---

func mustProvider(t *testing.T) *Provider {
	t.Helper()
	p, err := New(Config{APIKey: testToken})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func idsOf(assets []core.Asset) []string {
	out := make([]string, len(assets))
	for i, a := range assets {
		out[i] = a.ID
	}
	return out
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
