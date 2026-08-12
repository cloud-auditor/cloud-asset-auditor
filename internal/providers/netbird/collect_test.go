package netbird

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

const testToken = "nbp_testtoken"

// fakeAPI is a minimal NetBird Management API: it checks the Token auth header
// and serves canned JSON arrays for every endpoint the provider hits.
func fakeAPI(t *testing.T) *httptest.Server {
	t.Helper()
	bodies := map[string]string{
		"/api/accounts":        `[{"id":"acct1","domain":"acme.io","settings":{"network_range":"100.64.0.0/10"}}]`,
		"/api/peers":           `[{"id":"p1","name":"host1","ip":"100.64.0.1","connected":true,"groups":[{"id":"g1","name":"all"}]}]`,
		"/api/groups":          `[{"id":"g1","name":"all","peers_count":1}]`,
		"/api/policies":        `[{"id":"pol1","name":"default","enabled":true,"rules":[{"id":"ru1"}]}]`,
		"/api/routes":          `[{"id":"r1","network_id":"Route 1","network":"10.0.0.0/24","enabled":true}]`,
		"/api/networks":        `[]`,
		"/api/dns/nameservers": `[{"id":"ns1","name":"Google","enabled":true,"nameservers":[{"ip":"8.8.8.8","port":53,"ns_type":"udp"}]}]`,
		"/api/setup-keys":      `[]`,
		"/api/users":           `[{"id":"u1","name":"Tom","email":"t@acme.io","status":"active"}]`,
		"/api/posture-checks":  `[]`,
	}
	mux := http.NewServeMux()
	for path, body := range bodies {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Token "+testToken {
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

func TestCollect_EndToEnd(t *testing.T) {
	srv := fakeAPI(t)
	p, err := New(Config{APIToken: testToken, ManagementURL: srv.URL, IncludeRaw: true})
	if err != nil {
		t.Fatal(err)
	}

	assets, errs := p.Collect(context.Background())
	byType := map[string]core.Asset{}
	for a := range assets {
		byType[a.Type] = a
	}
	for e := range errs {
		t.Errorf("unexpected collect error: %v", e)
	}

	for _, want := range []string{
		"netbird.account", "netbird.peer", "netbird.group",
		"netbird.policy", "netbird.route", "netbird.nameserver", "netbird.user",
	} {
		if _, ok := byType[want]; !ok {
			t.Errorf("missing asset type %q (got %d types)", want, len(byType))
		}
	}

	// Every asset must carry the resolved account id.
	if got := byType["netbird.peer"].AccountID; got != "acct1" {
		t.Errorf("peer AccountID = %q, want acct1 (resolved from /api/accounts)", got)
	}
	if got := byType["netbird.peer"].Status; got != "connected" {
		t.Errorf("peer Status = %q, want connected", got)
	}
	if got := byType["netbird.nameserver"].Tags["nameservers"]; got != "8.8.8.8" {
		t.Errorf("nameserver IPs tag = %q, want 8.8.8.8", got)
	}
}

// The account list must be fetched exactly once per audit (resolveAccounts'
// sync.Once), shared by the id warm-up and the account collector — not fetched
// twice as it was before.
func TestCollect_FetchesAccountsOnce(t *testing.T) {
	var accountsHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts", func(w http.ResponseWriter, _ *http.Request) {
		accountsHits.Add(1)
		_, _ = io.WriteString(w, `[{"id":"acct1","domain":"acme.io"}]`)
	})
	for _, path := range []string{
		"/api/peers", "/api/groups", "/api/policies", "/api/routes", "/api/networks",
		"/api/dns/nameservers", "/api/setup-keys", "/api/users", "/api/posture-checks",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `[]`) })
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, err := New(Config{APIToken: testToken, ManagementURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	assets, errs := p.Collect(context.Background())
	for range assets {
	}
	for range errs {
	}
	if got := accountsHits.Load(); got != 1 {
		t.Errorf("GET /api/accounts hit %d times, want exactly 1", got)
	}
}

func TestValidate_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"requires authentication"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	p, err := New(Config{APIToken: "wrong", ManagementURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	err = p.Validate(context.Background())
	if err == nil {
		t.Fatal("Validate should fail on 401")
	}
	if !isAuthError(err) {
		// the wrapped error must still classify as an auth error
		t.Errorf("expected an auth error, got %v", err)
	}
}

func TestNew_RequiresToken(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New must reject an empty APIToken")
	}
}

func TestClient_RedactsToken(t *testing.T) {
	c := newClient("https://api.netbird.io", "nbp_SECRET")
	redacted := c.redact(errors.New("dial https://api.netbird.io with nbp_SECRET failed"))
	if strings.Contains(redacted.Error(), "nbp_SECRET") {
		t.Errorf("redact left the token in: %q", redacted.Error())
	}
	if !strings.Contains(redacted.Error(), "***") {
		t.Errorf("redact should mask the token with ***: %q", redacted.Error())
	}
}
