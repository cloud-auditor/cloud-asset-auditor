package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/output"
)

// handleHealthz is the liveness probe. Returns 200 unconditionally and never
// touches providers — must work even when credentials are missing.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok\n"))
}

// handleProviders returns the sorted list of registered provider names plus
// the server's configured auth mode. The frontend uses this to populate
// the provider-checkbox list before the user kicks off an audit.
func (s *Server) handleProviders(w http.ResponseWriter, _ *http.Request) {
	resp := struct {
		Providers []string `json:"providers"`
		AuthMode  string   `json:"auth_mode"`
	}{
		Providers: core.Registered(),
		AuthMode:  s.cfg.AuthMode,
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAuditSSE runs the audit and streams each Asset as an SSE event.
// Stream contract:
//
//	event: meta   data: {"started_at": "..."}                    // exactly once, first
//	event: init_error data: {"message": "..."}                   // zero or more, before assets
//	event: asset  data: {<one Asset>}                            // many, in arrival order
//	event: error  data: {"message": "..."}                       // zero or more, interleaved
//	event: done   data: {"count": N, "elapsed_ms": M, "errors": K}  // exactly once, last
func (s *Server) handleAuditSSE(w http.ResponseWriter, r *http.Request) {
	sse, err := newSSEWriter(w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	started := time.Now()
	_ = sse.emit("meta", map[string]any{"started_at": started.UTC().Format(time.RFC3339)})

	providers := parseProvidersParam(r.URL.Query().Get("providers"))
	timeout := parseTimeoutParam(r.URL.Query().Get("timeout"))

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	assets, errs, initErrs := s.runProviders(ctx, providers)
	for _, msg := range initErrs {
		_ = sse.emit("init_error", map[string]string{"message": msg})
	}

	// Drain errors in parallel so the asset channel can keep flowing even
	// when one provider is spewing failures.
	var errCount int
	errsDone := make(chan struct{})
	go func() {
		for e := range errs {
			if e == nil {
				continue
			}
			errCount++
			_ = sse.emit("error", map[string]string{"message": e.Error()})
		}
		close(errsDone)
	}()

	var assetCount int
	for a := range assets {
		if err := sse.emit("asset", a); err != nil {
			// Client disconnected mid-stream — abort cleanly.
			cancel()
			break
		}
		assetCount++
	}
	<-errsDone

	_ = sse.emit("done", map[string]any{
		"count":      assetCount,
		"elapsed_ms": time.Since(started).Milliseconds(),
		"errors":     errCount + len(initErrs),
	})
}

// handleAuditExport runs the audit synchronously and writes the rendered
// output as a downloadable file. Same data the CLI's `auditor audit` would
// emit — just delivered via HTTP.
func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "json"
	}
	renderer, contentType, err := buildExportRenderer(format)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	providers := parseProvidersParam(r.URL.Query().Get("providers"))
	timeout := parseTimeoutParam(r.URL.Query().Get("timeout"))

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	assets, errs, initErrs := s.runProviders(ctx, providers)

	// Drain errors in background — export endpoint discards them. Init
	// errors get logged as a response header so the operator can see
	// them via curl -I or browser dev tools.
	if len(initErrs) > 0 {
		w.Header().Set("X-Auditor-Init-Errors", strings.Join(initErrs, "; "))
	}
	go func() {
		for range errs {
		}
	}()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="assets.`+format+`"`)
	w.WriteHeader(http.StatusOK)

	if err := renderer.Render(ctx, assets, w); err != nil && !errors.Is(err, context.Canceled) {
		// Headers are already written; we can't emit a proper error
		// status. Best we can do is log via the response trailer (most
		// clients ignore it, but it's better than silence).
		w.Header().Set("X-Auditor-Render-Error", err.Error())
	}
}

func buildExportRenderer(format string) (output.Renderer, string, error) {
	switch format {
	case "json":
		return &output.JSON{}, "application/json", nil
	case "ndjson":
		return &output.JSON{Stream: true}, "application/x-ndjson", nil
	case "csv":
		return &output.CSV{}, "text/csv", nil
	default:
		return nil, "", fmt.Errorf("unknown format %q (want json|ndjson|csv)", format)
	}
}

func parseProvidersParam(raw string) []string {
	if raw == "" {
		return nil // signals "all registered"
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseTimeoutParam(raw string) time.Duration {
	const defaultTimeout = 10 * time.Minute
	if raw == "" {
		return defaultTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultTimeout
	}
	return d
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
