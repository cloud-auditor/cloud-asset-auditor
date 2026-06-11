// Package server is the Phase 5 web UI. It serves an embedded single-page
// app plus a versioned JSON/SSE API for running audits from a browser.
//
// Deviation from init-plan.md §3 Phase 5: the frontend is plain JS rather
// than Alpine.js. Keeps the binary fully self-contained with no third-party
// vendored JS and a smaller payload.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/metrics"
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
}

// Server bundles the HTTP server with its parsed config so handlers can
// reach the auth / audit settings without globals.
type Server struct {
	cfg Config
	mux *http.ServeMux
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

func (s *Server) routes() {
	// Static frontend (embedded). fs.Sub strips the web/ prefix so /
	// resolves to web/index.html, /app.js → web/app.js, etc.
	staticFS, err := fs.Sub(WebFS, "web")
	if err != nil {
		// embed.go declares the FS at package init; sub of "web/" cannot
		// fail unless the directory was renamed in the source tree.
		panic(fmt.Sprintf("server: web/ subtree missing from embed.FS: %v", err))
	}
	s.mux.Handle("/", http.FileServer(http.FS(staticFS)))

	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	// /metrics is always open (matches /healthz semantics: scrapers
	// shouldn't need credentials) and exempted from the auth middleware
	// by needsAuth's "/api/" check.
	s.mux.Handle("GET /metrics", metrics.Handler())
	s.mux.HandleFunc("GET /api/v1/openapi.yaml", s.handleOpenAPI)
	s.mux.HandleFunc("GET /api/v1/providers", s.handleProviders)
	s.mux.HandleFunc("GET /api/v1/audit", s.handleAuditSSE)
	s.mux.HandleFunc("GET /api/v1/audit/export", s.handleAuditExport)
	s.mux.HandleFunc("GET /api/v1/topology", s.handleTopology)
	s.mux.HandleFunc("POST /api/v1/topology", s.handleTopologyBuild)
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
