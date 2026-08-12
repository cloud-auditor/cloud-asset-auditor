package server

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The frontend is a *generated* tree: `just web` runs the Next.js export and
// copies it into internal/server/webui/, and the result is committed so
// `go build` needs no Node toolchain. The failure mode of that arrangement is
// someone changing web/ and forgetting to regenerate — or the embed directive
// losing files. These tests are the guard.

// Every exported route must be present as <route>/index.html. next.config.mjs
// sets trailingSlash:true precisely so the export takes this shape; if that
// flag is ever flipped, the export becomes assets.html and Go's FileServer
// 404s every route but "/".
func TestWebUI_ExportedRoutesArePresent(t *testing.T) {
	static, err := fs.Sub(WebFS, "webui")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		"index.html", "assets/index.html", "topology/index.html",
		"exposure/index.html", "404.html",
	} {
		if _, err := fs.Stat(static, f); err != nil {
			t.Errorf("missing %s from the embedded export — run `just web`: %v", f, err)
		}
	}
}

// `//go:embed all:webui` (not plain `embed webui`) is what pulls in the
// _next/ directory: embed skips underscore-prefixed names without `all:`.
// Losing it ships an HTML shell with no JavaScript, which looks like a blank
// page rather than a build error — hence an explicit test.
func TestWebUI_NextChunksAreEmbedded(t *testing.T) {
	static, err := fs.Sub(WebFS, "webui")
	if err != nil {
		t.Fatal(err)
	}
	var js, css int
	err = fs.WalkDir(static, "_next", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case strings.HasSuffix(p, ".js"):
			js++
		case strings.HasSuffix(p, ".css"):
			css++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("_next/ is not embedded — is the embed directive still `all:webui`? %v", err)
	}
	if js == 0 {
		t.Error("no .js chunks under _next/ — the export is an empty shell")
	}
	if css == 0 {
		t.Error("no .css under _next/ — styles would be missing")
	}
}

// Every script/stylesheet the HTML references must actually be servable. This
// catches a partial copy (an interrupted `just web`) that the file-presence
// test above would pass.
func TestWebUI_ReferencedAssetsResolve(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	for _, page := range []string{"/", "/assets/", "/topology/", "/exposure/"} {
		body := get(t, srv.URL+page, http.StatusOK)
		refs := extractNextRefs(body)
		if len(refs) == 0 {
			t.Errorf("%s references no /_next/static assets — the export looks empty", page)
			continue
		}
		for _, ref := range refs {
			get(t, srv.URL+ref, http.StatusOK)
		}
	}
}

// An unknown path gets the exported 404 page, not Go's plain-text default —
// and, importantly, still a 404 status, so a broken link is not reported to
// crawlers or health checks as success.
func TestWebUI_UnknownPathServesExported404(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	body := get(t, srv.URL+"/does-not-exist", http.StatusNotFound)
	if !strings.Contains(strings.ToLower(body), "<html") {
		t.Errorf("404 body is not the exported HTML page: %.120s", body)
	}
}

// Hashed chunks are immutable (their name is a content hash); HTML is not
// (a redeployed binary must not keep serving the previous build's markup).
func TestWebUI_CacheHeaders(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	refs := extractNextRefs(get(t, srv.URL+"/", http.StatusOK))
	if len(refs) == 0 {
		t.Fatal("no hashed assets to check")
	}

	res, err := http.Get(srv.URL + refs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if got := res.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("hashed asset Cache-Control = %q, want immutable", got)
	}

	htmlRes, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = htmlRes.Body.Close() }()
	if got := htmlRes.Header.Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Errorf("index.html Cache-Control = %q, want no-cache", got)
	}
}

// --- helpers ---

func mustServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Config{AuthMode: "none"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(t *testing.T, url string, wantStatus int) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != wantStatus {
		t.Errorf("GET %s → %d, want %d", url, res.StatusCode, wantStatus)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(body)
}

// extractNextRefs pulls /_next/static/... URLs out of the served HTML. It
// stops at a backslash as well as a quote: the page embeds a JSON payload in
// which those paths appear escaped, and those are data, not fetchable URLs.
func extractNextRefs(html string) []string {
	const marker = "/_next/static/"
	seen := map[string]bool{}
	var out []string
	for i := 0; ; {
		j := strings.Index(html[i:], marker)
		if j < 0 {
			break
		}
		start := i + j
		end := start
		for end < len(html) && !strings.ContainsRune("\"'\\ >)", rune(html[end])) {
			end++
		}
		ref := html[start:end]
		if !seen[ref] && (strings.HasSuffix(ref, ".js") || strings.HasSuffix(ref, ".css")) {
			seen[ref] = true
			out = append(out, ref)
		}
		i = end
	}
	return out
}
