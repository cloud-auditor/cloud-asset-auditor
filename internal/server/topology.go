package server

import (
	"context"
	"net/http"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

// handleTopology runs an audit (forcing --include-raw so the K8s
// resolvers can parse Ingress/HTTPRoute payloads), builds the graph,
// and returns JSON.
//
// Synchronous: no SSE because topology.Build needs every asset before it
// can index. Long audits make this slow; tune --timeout accordingly.
//
// Query params:
//   providers=cloudflare,kubernetes   subset; default = all registered
//   hostname=api.example.com          repeatable; filters to connected component
//   include-orphans=true              keep nodes with no edges
//   timeout=10m                       audit timeout
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	providers := parseProvidersParam(q.Get("providers"))
	hostnames := q["hostname"]
	includeOrphans := q.Get("include-orphans") == "true"
	timeout := parseTimeoutParam(q.Get("timeout"))

	// Snapshot the relevant server config, then locally force include-raw
	// for the duration of this handler. We don't mutate s.cfg because
	// other concurrent handlers shouldn't see the toggle.
	prev := s.cfg.IncludeRaw
	s.cfg.IncludeRaw = true
	defer func() { s.cfg.IncludeRaw = prev }()

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	assets, errs, initErrs := s.runProviders(ctx, providers)

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

	topo := topology.Build(collected)
	if len(hostnames) > 0 {
		topo = topo.FilterByHostname(hostnames)
	}
	if !includeOrphans {
		topo = topo.DropOrphans()
	}

	resp := struct {
		Nodes      []core.Asset `json:"nodes"`
		Edges      []core.Edge  `json:"edges"`
		InitErrors []string     `json:"init_errors,omitempty"`
		Errors     []string     `json:"errors,omitempty"`
	}{
		// Strip Raw from the response — clients don't need the full SDK
		// payload (resolvers already consumed it), and shipping it bloats
		// the JSON considerably.
		Nodes:      stripRaw(topo.Nodes),
		Edges:      topo.Edges,
		InitErrors: initErrs,
		Errors:     collectErrs,
	}
	writeJSON(w, http.StatusOK, resp)
}

func stripRaw(in []core.Asset) []core.Asset {
	out := make([]core.Asset, len(in))
	for i, a := range in {
		a.Raw = nil
		out[i] = a
	}
	return out
}
