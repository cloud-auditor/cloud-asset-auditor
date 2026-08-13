// Package server is the Phase 5 web UI. It serves an embedded single-page
// app plus a versioned JSON/SSE API for running audits from a browser.
//
// The frontend is a Next.js app (source in web/ at the repo root) built in
// static-export mode and embedded from internal/server/webui/ — see embed.go.
// The binary stays fully self-contained: no Node runtime, no CDN, no sidecar.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/cost"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/metrics"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/pricing"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/telemetry"
)

// Config controls server behavior.
type Config struct {
	Addr           string
	AuthMode       string // "none" | "basic" | "token"
	BasicUser      string // populated from AUDITOR_BASIC_USER when AuthMode == "basic"
	BasicPass      string // populated from AUDITOR_BASIC_PASS when AuthMode == "basic"
	APIToken       string // populated from AUDITOR_API_TOKEN when AuthMode == "token"
	MaxConcurrency int
	IncludeRaw     bool
	ShutdownGrace  time.Duration

	// Cost turns on per-asset cost annotation for every audit the server
	// serves: the SSE stream, the exports, and anything downstream of them
	// then carry the cost.* tags. Off by default, so a plain server is
	// byte-identical to one built before the feature existed.
	//
	// Only the streaming half is available here. Kubernetes pod attribution
	// and per-seat rollups need the whole set at once, which is why they live
	// in `auditor cost` — see the internal/cost package doc.
	Cost bool

	// PriceBook is an optional path to a price book that overrides the
	// embedded default. Empty means "the embedded book".
	PriceBook string

	// Providers scopes what a request that names no providers will run.
	// Empty means "every registered provider", which is the historical
	// behaviour and the right default for an operator who configured
	// credentials and wants everything.
	//
	// It exists because `serve --demo` must not fall back to "everything":
	// on a host that also has real credentials, a browser request omitting
	// the parameter would blend fabricated demo assets into a real
	// inventory, and nothing downstream distinguishes them.
	Providers []string
}

// Server bundles the HTTP server with its parsed config so handlers can
// reach the auth / audit settings without globals.
type Server struct {
	cfg Config
	mux *http.ServeMux

	// estimator is non-nil only when Config.Cost is set. Built once at
	// startup rather than per request: loading and validating the price book
	// on every audit would repeat the same disk read and the same failure.
	estimator *cost.Estimator

	// patterns records every route passed to handle/handleFunc, in
	// registration order. Used by the openapi sync test; not part of the
	// public surface.
	patterns []string
}

// New constructs a Server with handlers registered. It does not bind any
// port — call Run for that.
func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.AuthMode == "" {
		cfg.AuthMode = "none"
	}
	if cfg.ShutdownGrace <= 0 {
		cfg.ShutdownGrace = 10 * time.Second
	}
	if err := validateAuth(cfg); err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	if cfg.Cost {
		// Fail at startup, not per request. A bad --price-book should stop the
		// server coming up, not produce audits that are silently uncosted.
		book, err := loadPriceBook(cfg.PriceBook)
		if err != nil {
			return nil, err
		}
		s.estimator = cost.New(book)
	}
	s.routes()
	return s, nil
}

// Handler returns the underlying http.Handler (mux), wrapped in:
//  1. otelhttp middleware (request spans, filtered to skip /healthz noise)
//  2. auth middleware (basic/token gate on /api/*)
//  3. the mux itself
//
// Useful for tests that want to wrap the result in httptest.NewServer
// without binding a real port.
func (s *Server) Handler() http.Handler {
	return otelhttp.NewHandler(
		s.authMiddleware(s.mux),
		telemetry.ServiceName,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
		otelhttp.WithFilter(func(r *http.Request) bool {
			// /healthz is hit by k8s probes every few seconds — emitting
			// a span per probe drowns the actual interesting requests.
			return r.URL.Path != "/healthz"
		}),
	)
}

// Run binds the listener and blocks until ctx is cancelled, then performs a
// graceful shutdown (waits up to ShutdownGrace for in-flight requests).
func (s *Server) Run(ctx context.Context) error {
	httpServer := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Note: no WriteTimeout — SSE responses can stream for the full
		// audit duration. Read timeout is the security-critical one.
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownGrace)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-serveErr:
		return err
	}
}

// handle registers a pattern and records it, so a test can assert that every
// route the server actually serves is described in openapi.yaml. The spec is
// hand-maintained; without the recorded list, the sync check can only run in
// the documented→handler direction and a new undocumented handler passes CI
// while leaving the spec quietly lying.
func (s *Server) handle(pattern string, h http.Handler) {
	s.patterns = append(s.patterns, pattern)
	s.mux.Handle(pattern, h)
}

func (s *Server) handleFunc(pattern string, h http.HandlerFunc) {
	s.handle(pattern, h)
}

func (s *Server) routes() {
	// Static frontend (embedded). fs.Sub strips the webui/ prefix so /
	// resolves to webui/index.html, /topology/ → webui/topology/index.html.
	staticFS, err := fs.Sub(WebFS, "webui")
	if err != nil {
		// embed.go declares the FS at package init; sub of "webui/" cannot
		// fail unless the directory was renamed in the source tree.
		panic(fmt.Sprintf("server: webui/ subtree missing from embed.FS: %v", err))
	}
	s.mux.Handle("/", spaHandler(staticFS))

	s.handleFunc("GET /healthz", s.handleHealthz)
	// /metrics is always open (matches /healthz semantics: scrapers
	// shouldn't need credentials) and exempted from the auth middleware
	// by needsAuth's "/api/" check.
	s.handle("GET /metrics", metrics.Handler())
	s.handleFunc("GET /api/v1/openapi.yaml", s.handleOpenAPI)
	s.handleFunc("GET /api/v1/providers", s.handleProviders)
	s.handleFunc("GET /api/v1/kube/contexts", s.handleKubeContexts)
	s.handleFunc("GET /api/v1/audit", s.handleAuditSSE)
	s.handleFunc("GET /api/v1/audit/export", s.handleAuditExport)
	s.handleFunc("GET /api/v1/topology", s.handleTopology)
	s.handleFunc("POST /api/v1/topology", s.handleTopologyBuild)
	s.handleFunc("GET /api/v1/reach", s.handleReach)
	s.handleFunc("POST /api/v1/reach", s.handleReachBuild)
	s.handleFunc("GET /api/v1/insights", s.handleInsights)
	s.handleFunc("POST /api/v1/insights", s.handleInsightsBuild)
}

// handleOpenAPI serves the embedded OpenAPI 3.1 spec verbatim. Spec
// contains no secrets — kept reachable without auth so client
// generators (Swagger UI, oapi-codegen, openapi-typescript) can
// consume the running server's contract without out-of-band downloads.
// The auth middleware doesn't intervene because /api/v1/openapi.yaml
// is the only `/api/*` path needsAuth() must explicitly allow through.
func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(OpenAPISpec)
}

// loadPriceBook returns the embedded book, or the embedded book with the
// operator's overrides merged over it when a path is given.
func loadPriceBook(path string) (*pricing.Book, error) {
	if path == "" {
		return pricing.Default()
	}
	return pricing.LoadFile(path)
}

func validateAuth(cfg Config) error {
	switch cfg.AuthMode {
	case "none":
		return nil
	case "basic":
		if cfg.BasicUser == "" || cfg.BasicPass == "" {
			return errors.New("auth=basic requires AUDITOR_BASIC_USER and AUDITOR_BASIC_PASS")
		}
		return nil
	case "token":
		if cfg.APIToken == "" {
			return errors.New("auth=token requires AUDITOR_API_TOKEN")
		}
		return nil
	default:
		return fmt.Errorf("unknown auth mode %q (want none|basic|token)", cfg.AuthMode)
	}
}

// spaHandler serves the exported Next.js frontend.
//
// It is http.FileServer with two adjustments the export needs:
//
//   - Unknown paths get the exported 404.html instead of Go's plain-text
//     "404 page not found". The export is a fixed set of routes (there is no
//     client-side router to hand an unknown path to), so a miss really is a
//     miss — it should just look like the rest of the app.
//   - Hashed build artefacts under /_next/static/ are marked immutably
//     cacheable. Their names contain a content hash, so a stale copy is
//     impossible by construction, and this turns every reload after the first
//     into zero requests for the bulk of the payload. Everything else stays
//     uncached: index.html must be revalidated or a redeployed binary would
//     keep serving the previous build's HTML.
func spaHandler(static fs.FS) http.Handler {
	files := http.FileServer(http.FS(static))
	notFound, _ := fs.ReadFile(static, "404.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_next/static/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}

		if len(notFound) > 0 && !staticFileExists(static, r.URL.Path) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(notFound)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// staticFileExists reports whether a request path resolves to something in
// the embedded tree — either a file, or a directory holding an index.html
// (which is how every exported route is shaped).
func staticFileExists(static fs.FS, urlPath string) bool {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" {
		name = "."
	}
	info, err := fs.Stat(static, name)
	if err != nil {
		return false
	}
	if !info.IsDir() {
		return true
	}
	_, err = fs.Stat(static, path.Join(name, "index.html"))
	return err == nil
}
