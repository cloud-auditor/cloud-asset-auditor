package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

// Reachability endpoints. Same two-verb shape as /api/v1/topology, and for the
// same reason: GET runs a fresh raw-bearing audit (slow, complete), POST
// builds from assets the client already holds (instant, free of provider API
// calls). The UI uses POST for interactive exploration.
//
// Query params (both verbs):
//
//	from=<glob>        what can it reach?      (matched on asset id and name)
//	to=<glob>          what can reach it?
//	from + to          how does one get to the other?
//	exposed=true       what can the internet reach?
//	max_hops=6         path length bound
//	max_paths=25       result bound; the response reports when it truncated
//	kinds=a,b          restrict traversal to these edge kinds
//	include_deny=true  follow traffic-deny edges as well
//	format=json|dot|…  json (default) or any topology renderer, as a download
//
// At least one of from / to / exposed is required; anything else is a 400,
// because an unconstrained "reachability" query has no meaningful answer.

func (s *Server) handleReach(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Force include-raw for the duration of this handler so the Kubernetes
	// resolvers see Ingress / Service / NetworkPolicy payloads. Same
	// save-and-restore dance as handleTopology — s.cfg is shared, and a
	// concurrent handler must not observe the toggle.
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

	s.respondReach(w, q, collected, initErrs, collectErrs)
}

// handleReachBuild answers the same questions from assets supplied in the
// request body — the path the UI takes, since the Assets view already holds
// the full stream and re-running every provider to answer "what can reach X"
// would waste the operator's API quota.
func (s *Server) handleReachBuild(w http.ResponseWriter, r *http.Request) {
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
	s.respondReach(w, r.URL.Query(), assets, nil, nil)
}

// respondReach is the shared tail: build the graph, run the query, render.
func (s *Server) respondReach(
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

	from := get("from")
	to := get("to")
	exposed := parseBoolParam(get("exposed"))
	if from == "" && to == "" && !exposed {
		http.Error(w, `one of "from", "to", or "exposed=true" is required`, http.StatusBadRequest)
		return
	}

	opts := topology.ReachOptions{
		MaxHops:     atoiOr(get("max_hops"), 0),
		MaxPaths:    atoiOr(get("max_paths"), 0),
		IncludeDeny: parseBoolParam(get("include_deny")),
	}
	if kinds := get("kinds"); kinds != "" {
		for _, k := range strings.Split(kinds, ",") {
			if k = strings.TrimSpace(k); k != "" {
				opts.Kinds = append(opts.Kinds, k)
			}
		}
	}

	topo := topology.Build(assets)
	res, err := buildReachResult(topo, from, to, exposed, opts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	format := strings.ToLower(get("format"))
	if format == "" || format == "json" {
		writeJSON(w, http.StatusOK, reachResponse{
			ReachResult: res,
			InitErrors:  initErrs,
			Errors:      collectErrs,
		})
		return
	}

	contentType, filename := topologyContentType(format)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="reach-`+filename+`"`)
	if len(initErrs) > 0 {
		w.Header().Set("X-Auditor-Init-Errors", strings.Join(initErrs, "; "))
	}
	if len(collectErrs) > 0 {
		w.Header().Set("X-Auditor-Errors", strings.Join(collectErrs, "; "))
	}
	w.WriteHeader(http.StatusOK)
	_ = topology.RenderReach(res, format, w)
}

// reachResponse embeds the result so the JSON keeps one flat shape, and adds
// the provider-error fields every other endpoint carries.
type reachResponse struct {
	topology.ReachResult
	InitErrors []string `json:"init_errors,omitempty"`
	Errors     []string `json:"errors,omitempty"`
}

// buildReachResult mirrors the CLI's runReach. Kept as a small duplicate
// rather than exported from internal/cli, because importing the CLI package
// from the server would pull cobra and every provider registration into the
// server's dependency graph.
func buildReachResult(
	topo *topology.Topology,
	from, to string,
	exposed bool,
	opts topology.ReachOptions,
) (topology.ReachResult, error) {
	var res topology.ReachResult

	switch {
	case exposed:
		exp := topo.Exposed(opts)
		res.Question = fmt.Sprintf("What can the internet reach? (from %d public entry points)", len(exp.Entries))
		res.Sources = exp.Entries
		res.Paths = exp.Paths

	case from != "" && to != "":
		sources, targets := topo.Select(from), topo.Select(to)
		if err := requireSelectorMatch(from, sources); err != nil {
			return res, err
		}
		if err := requireSelectorMatch(to, targets); err != nil {
			return res, err
		}
		res.Question = fmt.Sprintf("How can %q reach %q?", from, to)
		res.Sources, res.Targets = sources, targets
		res.Paths = topo.Paths(sources, targets, opts)

	case from != "":
		sources := topo.Select(from)
		if err := requireSelectorMatch(from, sources); err != nil {
			return res, err
		}
		res.Question = fmt.Sprintf("What can %q reach?", from)
		res.Sources = sources
		res.Paths = topo.Reachable(sources, topology.Downstream, opts)

	default:
		targets := topo.Select(to)
		if err := requireSelectorMatch(to, targets); err != nil {
			return res, err
		}
		res.Question = fmt.Sprintf("What can reach %q?", to)
		res.Targets = targets
		res.Paths = topo.Reachable(targets, topology.Upstream, opts)
	}

	if max := opts.MaxPaths; max > 0 && len(res.Paths) >= max {
		res.Truncated = true
	}
	return res, nil
}

// requireSelectorMatch turns "matched nothing" into a 400 with a usable
// message rather than an empty result, which would read as "nothing can reach
// it" — the opposite of "your selector was wrong".
func requireSelectorMatch(selector string, got []core.Asset) error {
	if len(got) > 0 {
		return nil
	}
	return fmt.Errorf("selector %q matched no assets (selectors are globs over asset id and name)", selector)
}

func atoiOr(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}
