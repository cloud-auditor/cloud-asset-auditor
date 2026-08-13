package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/insight"
)

// Insight endpoints. Same two-verb shape as /api/v1/topology and /api/v1/reach,
// and for the same reason: GET runs a fresh raw-bearing audit (slow, complete),
// POST derives the same findings from assets the client already holds (instant,
// zero provider API calls). The UI uses POST — the Assets view already has the
// streamed inventory in memory, and re-collecting an estate to answer questions
// about the estate it just collected would burn the operator's quota twice.
//
// Query params (both verbs):
//
//	only=exposure,hygiene.*   run only these insights (globs on id AND family)
//	severity=warn             drop findings below this severity
//	max_rows=25               detail rows per finding in the human formats
//	format=json|table|markdown  json (default) inline; the others as a download
//
// GET additionally takes providers / timeout / kube_contexts, like every other
// audit-backed endpoint.
//
// Cost-bearing findings need a price book, which is a startup decision here
// rather than a per-request one (`serve --cost`, see Config.Cost). Without it
// they come back under `skipped` saying so, which is the honest answer: a
// report that priced some things and not others would read as a full
// accounting of the money.

func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Force include-raw for the duration of this handler so the insights that
	// read a resource's own document see one. Same save-and-restore dance as
	// handleTopology — s.cfg is shared, and a concurrent handler must not
	// observe the toggle.
	prev := s.cfg.IncludeRaw
	s.cfg.IncludeRaw = true
	defer func() { s.cfg.IncludeRaw = prev }()

	timeout := parseTimeoutParam(q.Get("timeout"))
	kubeContexts, ctxWarn := validateKubeContexts(q.Get("kube_contexts"))

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	assets, errs, initErrs := s.runProviders(ctx, parseProvidersParam(q.Get("providers")),
		reqOptions{kubeContexts: kubeContexts})
	if ctxWarn != "" {
		initErrs = append(initErrs, ctxWarn)
	}

	var collected []core.Asset
	var collectErrs []string
	errsDone := make(chan struct{})
	go func() {
		for e := range errs {
			if e != nil {
				collectErrs = append(collectErrs, e.Error())
			}
		}
		close(errsDone)
	}()
	for a := range assets {
		collected = append(collected, a)
	}
	<-errsDone

	// r.Context(), not the audit's ctx: the timeout parameter bounds the
	// collection, not the arithmetic done on what it collected. Handing the
	// expired context to Run would turn a timed-out audit into an empty report
	// plus a cancellation note, when the useful answer is the findings derived
	// from the assets that did arrive — with the timeout itself listed among
	// the errors. The CLI draws the line in the same place.
	s.respondInsights(r.Context(), w, q, collected, initErrs, collectErrs)
}

// handleInsightsBuild derives the same findings from assets supplied in the
// request body. Note the body only carries Raw if the *server* was started with
// --include-raw; without it the raw-reading insights report themselves as NOT
// RUN, which is why the GET form forces raw on.
func (s *Server) handleInsightsBuild(w http.ResponseWriter, r *http.Request) {
	assets, err := decodeAssetsBody(http.MaxBytesReader(w, r.Body, maxTopologyBodyBytes))
	if err != nil {
		status := http.StatusBadRequest
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "decode assets: "+err.Error(), status)
		return
	}
	s.respondInsights(r.Context(), w, r.URL.Query(), assets, nil, nil)
}

// respondInsights is the shared tail: build the input, run, render.
func (s *Server) respondInsights(
	ctx context.Context,
	w http.ResponseWriter,
	q map[string][]string,
	assets []core.Asset,
	initErrs, collectErrs []string,
) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}

	format := strings.ToLower(strings.TrimSpace(get("format")))
	if format == "" {
		format = "json"
	}
	contentType, filename, known := insightsContentType(format)
	if !known {
		http.Error(w, "unknown insights format "+format+" (want json|table|markdown)", http.StatusBadRequest)
		return
	}

	var minSeverity insight.Severity
	if raw := get("severity"); strings.TrimSpace(raw) != "" {
		parsed, err := insight.ParseSeverity(raw)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		minSeverity = parsed
	}

	// A typed-nil *cost.Estimator stored in the interface would not be nil, and
	// Input.Priced() would then promise figures the server cannot produce. Guard
	// on the concrete pointer instead of letting WithEstimator's nil check see
	// an interface that is only nominally set.
	var inputOpts []insight.InputOption
	if s.estimator != nil {
		inputOpts = append(inputOpts, insight.WithEstimator(s.estimator))
	}

	in := insight.NewInput(assets, inputOpts...)
	report := insight.Run(ctx, in, insight.Options{
		Only:        splitCommaParams(q["only"]),
		MinSeverity: minSeverity,
	})

	if format == "json" {
		writeJSON(w, http.StatusOK, insightsResponse{
			Report:     report,
			InitErrors: initErrs,
			Errors:     collectErrs,
		})
		return
	}

	// The human formats are a report to keep, not a payload to parse, so they
	// come back as a download with the provider errors in headers — the same
	// shape the topology and reach renderers use.
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	if len(initErrs) > 0 {
		w.Header().Set("X-Auditor-Init-Errors", strings.Join(initErrs, "; "))
	}
	if len(collectErrs) > 0 {
		w.Header().Set("X-Auditor-Errors", strings.Join(collectErrs, "; "))
	}
	w.WriteHeader(http.StatusOK)
	_ = insight.Render(report, format, w, insight.WithMaxRows(atoiOr(get("max_rows"), 0)))
}

// insightsResponse embeds the report so the JSON keeps one flat shape — the
// disclaimer, scope, findings, skipped and suppressed lists stay top-level —
// and adds the provider-error fields every other endpoint carries.
type insightsResponse struct {
	*insight.Report
	InitErrors []string `json:"init_errors,omitempty"`
	Errors     []string `json:"errors,omitempty"`
}

// insightsContentType picks the MIME type and download filename per format,
// and reports whether the format is one this endpoint serves. Validating here
// rather than inside Render means an unknown format is a 400 before an audit
// runs, not a truncated 200 after one.
func insightsContentType(format string) (contentType, filename string, ok bool) {
	switch format {
	case "json":
		return "application/json", "insights.json", true
	case "table":
		return "text/plain; charset=utf-8", "insights.txt", true
	case "markdown", "md":
		return "text/markdown; charset=utf-8", "insights.md", true
	default:
		return "", "", false
	}
}

// splitCommaParams flattens repeated query params that also accept commas, so
// ?only=exposure&only=cost and ?only=exposure,cost mean the same thing. Every
// other list-shaped param on this API takes commas; a client should not have to
// remember which ones also repeat.
func splitCommaParams(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}
