package cloudflare

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// fakeAPI is a stand-in for the Cloudflare v4 REST API.
//
// Tests built on it drive the *real* SDK — the seam is the CLOUDFLARE_BASE_URL
// environment variable that cf.DefaultClientOptions() reads (client.go:224).
// New() calls cf.NewClient with no explicit base URL, so the env var wins and
// no production hook is needed. That matters: every assertion below therefore
// exercises the SDK's own pagination loops, envelope decoding and error
// construction, so a v4 upgrade that changes any of those fails here instead of
// silently in production.
//
// Nothing here touches the network: httptest binds loopback only.
type fakeAPI struct {
	srv *httptest.Server

	// mu serializes handler dispatch as well as guarding the fields below.
	// Collectors fan out concurrently under an errgroup, so without it the
	// per-route hit counters and the call log would race.
	mu     sync.Mutex
	seen   []apiCall
	routes []*apiRoute
}

type apiCall struct {
	Method string
	Path   string
	Query  url.Values
}

type apiRoute struct {
	suffix string
	hits   int
	fn     func(w http.ResponseWriter, r *http.Request, nth int)
}

// newFakeAPI starts the fake API and registers it for cleanup.
func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.start(t)
	return f
}

func (f *fakeAPI) start(t *testing.T) {
	t.Helper()
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	// looksLikeAuthzError substring-matches "403" against the WHOLE error
	// string, and the SDK embeds the request URL in that string. An ephemeral
	// port such as :14036 would therefore make a plain 500 look like a
	// permission denial and flake the negative cases. Re-roll the listener
	// rather than weaken the assertion. (The same substring match can misfire
	// in production on a resource id containing "403" — harmless, since the
	// hint is advisory, but worth knowing.)
	for i := 0; i < 16 && strings.Contains(f.srv.URL, "403"); i++ {
		f.srv.Close()
		f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	}
	t.Cleanup(f.srv.Close)
}

// provider points a real *Provider — real SDK client, real transport — at the
// fake API. The env var must be set before New(), because cf.NewClient
// snapshots the environment at construction time.
func (f *fakeAPI) provider(t *testing.T, cfg Config) *Provider {
	t.Helper()
	if cfg.APIToken == "" {
		cfg.APIToken = "fake-token"
	}
	t.Setenv("CLOUDFLARE_BASE_URL", f.srv.URL)
	t.Setenv("CLOUDFLARE_API_TOKEN", cfg.APIToken)
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%+v) = %v, want a provider", cfg, err)
	}
	return p
}

// route registers a handler for every request whose path ends with suffix; nth
// is the zero-based hit count for this route, which is how paging fixtures
// vary their reply. Routes are tried in registration order, first match wins.
//
// Suffix (not substring) matching is deliberate: every Cloudflare list endpoint
// has a unique path suffix, whereas a substring match on "/accounts" would also
// swallow "/accounts/{id}/r2/buckets".
func (f *fakeAPI) route(suffix string, fn func(w http.ResponseWriter, r *http.Request, nth int)) *fakeAPI {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = append(f.routes, &apiRoute{suffix: suffix, fn: fn})
	return f
}

// static answers with body — except for requests carrying page=2 or higher,
// which get an empty result. That second half is load-bearing: the SDK's
// V4PagePaginationArray auto-pager (accounts, zones, dns_records,
// custom_certificates, kv namespaces) stops only when a page comes back EMPTY,
// so a naively static fixture would page forever.
func (f *fakeAPI) static(suffix, body string) *fakeAPI {
	return f.route(suffix, func(w http.ResponseWriter, r *http.Request, _ int) {
		if pg := r.URL.Query().Get("page"); pg != "" && pg != "1" {
			fmt.Fprint(w, emptyResultFor(r.URL.Path))
			return
		}
		fmt.Fprint(w, body)
	})
}

// fail answers with a Cloudflare error envelope at the given HTTP status.
func (f *fakeAPI) fail(suffix string, status, code int, msg string) *fakeAPI {
	return f.route(suffix, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(status)
		fmt.Fprint(w, cfErrorBody(code, msg))
	})
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.seen = append(f.seen, apiCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query()})
	w.Header().Set("Content-Type", "application/json")
	// Never let a fixture trip the SDK's retry loop: shouldRetry() honours this
	// header over the status code, so a 429/5xx fixture answers once instead of
	// sleeping 0.5s + 1s and re-hitting the route (which would break every
	// call-count assertion).
	w.Header().Set("x-should-retry", "false")

	for _, rt := range f.routes {
		if strings.HasSuffix(r.URL.Path, rt.suffix) {
			nth := rt.hits
			rt.hits++
			rt.fn(w, r, nth)
			return
		}
	}
	// Unrouted endpoints answer "nothing here" so a run() test can exercise the
	// whole collector fan-out while stubbing only the endpoints it cares about.
	fmt.Fprint(w, emptyResultFor(r.URL.Path))
}

// hits counts requests whose path ends with suffix.
func (f *fakeAPI) hits(suffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.seen {
		if strings.HasSuffix(c.Path, suffix) {
			n++
		}
	}
	return n
}

// queries returns, in arrival order, the query strings of every request whose
// path ends with suffix.
func (f *fakeAPI) queries(suffix string) []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []url.Values
	for _, c := range f.seen {
		if strings.HasSuffix(c.Path, suffix) {
			out = append(out, c.Query)
		}
	}
	return out
}

// paths returns every request path seen, in arrival order.
func (f *fakeAPI) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.seen))
	for _, c := range f.seen {
		out = append(out, c.Path)
	}
	return out
}

// ---------------------------------------------------------------------------
// response bodies
// ---------------------------------------------------------------------------

// v4List wraps a JSON array (or object) in the standard v4 success envelope.
func v4List(result string) string {
	return `{"success":true,"errors":[],"messages":[],"result":` + result + `}`
}

// v4ListCursor is v4List plus a result_info.cursor, which is how the SDK's
// CursorPagination (rulesets) is told there is another page.
func v4ListCursor(result, cursor string) string {
	return fmt.Sprintf(`{"success":true,"errors":[],"messages":[],"result":%s,"result_info":{"cursor":%q}}`, result, cursor)
}

func cfErrorBody(code int, msg string) string {
	return fmt.Sprintf(`{"success":false,"errors":[{"code":%d,"message":%q}],"messages":[],"result":null}`, code, msg)
}

// emptyResultFor returns the shape of an empty listing for path. R2 is the odd
// one out: its result is an object holding a "buckets" array, not a bare array,
// so a generic `"result":[]` fails to decode.
func emptyResultFor(path string) string {
	if strings.HasSuffix(path, "/r2/buckets") {
		return v4List(`{"buckets":[]}`)
	}
	return v4List(`[]`)
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func accountsJSON(ids ...string) string {
	items := make([]string, 0, len(ids))
	for _, id := range ids {
		items = append(items, fmt.Sprintf(`{"id":%q,"name":%q,"created_on":"2024-01-02T03:04:05Z"}`, id, "acct "+id))
	}
	return v4List("[" + strings.Join(items, ",") + "]")
}

func zonesJSON(id, name, accountID string) string {
	return v4List(fmt.Sprintf(`[{"id":%q,"name":%q,"status":"active","paused":false,"type":"full",
	  "account":{"id":%q,"name":"Acme"},"created_on":"2024-03-04T05:06:07Z"}]`, id, name, accountID))
}

func r2BucketsJSON(names ...string) string {
	items := make([]string, 0, len(names))
	for _, n := range names {
		items = append(items, fmt.Sprintf(
			`{"name":%q,"location":"wnam","storage_class":"Standard","creation_date":"2022-06-24T19:58:49.477Z"}`, n))
	}
	return v4List(`{"buckets":[` + strings.Join(items, ",") + `]}`)
}

// ---------------------------------------------------------------------------
// drivers
// ---------------------------------------------------------------------------

// collectVia runs a single collector to completion against an unbuffered
// channel, draining concurrently so a large result set can't deadlock the test.
func collectVia(t *testing.T, fn func(context.Context, chan<- core.Asset) error) ([]core.Asset, error) {
	t.Helper()
	return collectViaCtx(t, context.Background(), fn)
}

func collectViaCtx(t *testing.T, ctx context.Context, fn func(context.Context, chan<- core.Asset) error) ([]core.Asset, error) {
	t.Helper()
	out := make(chan core.Asset)
	var got []core.Asset
	done := make(chan struct{})
	go func() {
		defer close(done)
		for a := range out {
			got = append(got, a)
		}
	}()
	err := fn(ctx, out)
	close(out)
	<-done
	return got, err
}

// runCollect drives the full Collect() fan-out to completion. Both channels are
// drained concurrently: the asset channel is unbuffered, so a single-threaded
// reader would deadlock the moment the 32-slot error buffer filled.
func runCollect(t *testing.T, ctx context.Context, p *Provider) ([]core.Asset, []error) {
	t.Helper()
	assetCh, errCh := p.Collect(ctx)

	var (
		mu     sync.Mutex
		assets []core.Asset
		errs   []error
		wg     sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for a := range assetCh {
			mu.Lock()
			assets = append(assets, a)
			mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for e := range errCh {
			mu.Lock()
			errs = append(errs, e)
			mu.Unlock()
		}
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Collect() did not finish within 30s — a channel was never closed")
	}
	return assets, errs
}

// ---------------------------------------------------------------------------
// assertion helpers
// ---------------------------------------------------------------------------

func assetsOfType(assets []core.Asset, typ string) []core.Asset {
	var out []core.Asset
	for _, a := range assets {
		if a.Type == typ {
			out = append(out, a)
		}
	}
	return out
}

func assetIDs(assets []core.Asset) []string {
	out := make([]string, 0, len(assets))
	for _, a := range assets {
		out = append(out, a.ID)
	}
	return out
}

// errorContaining returns the first error whose text contains sub, or nil.
func errorContaining(errs []error, sub string) error {
	for _, e := range errs {
		if strings.Contains(e.Error(), sub) {
			return e
		}
	}
	return nil
}

func errorStrings(errs []error) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Error())
	}
	return out
}
