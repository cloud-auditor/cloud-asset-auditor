package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

// handleTopology runs an audit (forcing --include-raw so the K8s
// resolvers can parse Ingress/HTTPRoute payloads), builds the graph,
// and renders in one of the supported formats.
//
// Synchronous: no SSE because topology.Build needs every asset before it
// can index. Long audits make this slow; tune --timeout accordingly.
//
// Query params:
//
//	providers=cloudflare,kubernetes   subset; default = all registered
//	hostname=api.example.com          repeatable; filters to connected component
//	include-orphans=true              keep nodes with no edges
//	timeout=10m                       audit timeout
//	format=json|dot|mermaid|excalidraw|html   default json
//
// dot / mermaid / excalidraw / html responses come back as attachments
// with a sensible filename so dragging the URL into a file manager (or
// letting curl follow Content-Disposition) saves something openable.
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	providers := parseProvidersParam(q.Get("providers"))
	hostnames := q["hostname"]
	includeOrphans := q.Get("include-orphans") == "true"
	timeout := parseTimeoutParam(q.Get("timeout"))
	format := strings.ToLower(q.Get("format"))
	if format == "" {
		format = "json"
	}

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
	s.renderTopology(w, topo, topologyRenderOpts{
		format:         format,
		hostnames:      hostnames,
		includeOrphans: includeOrphans,
		initErrs:       initErrs,
		collectErrs:    collectErrs,
	})
}

// maxTopologyBodyBytes caps POST /api/v1/topology request bodies. 128 MiB
// comfortably fits raw-bearing audits of large clusters while bounding what
// an unauthenticated-adjacent client can make the server buffer.
const maxTopologyBodyBytes = 128 << 20

// handleTopologyBuild builds a graph from assets supplied by the client
// instead of running an audit. The web UI uses this: the Assets tab already
// holds every streamed asset in memory, so building the diagram from them is
// instant and costs zero provider API calls. Accepts the same hostname /
// include-orphans / format query params as the GET handler.
func (s *Server) handleTopologyBuild(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	format := strings.ToLower(q.Get("format"))
	if format == "" {
		format = "json"
	}

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

	topo := topology.Build(assets)
	s.renderTopology(w, topo, topologyRenderOpts{
		format:         format,
		hostnames:      q["hostname"],
		includeOrphans: parseBoolParam(q.Get("include-orphans")),
	})
}

// decodeAssetsBody accepts either a bare JSON array of assets (what
// `auditor audit -o json` emits) or an {"assets": [...]} envelope (what a
// JS client naturally builds). The first non-whitespace byte disambiguates.
func decodeAssetsBody(r io.Reader) ([]core.Asset, error) {
	br := bufio.NewReader(r)
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("empty body (want a JSON array of assets or {\"assets\": [...]}): %w", err)
		}
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		if err := br.UnreadByte(); err != nil {
			return nil, err
		}
		break
	}

	first, _ := br.Peek(1)
	dec := json.NewDecoder(br)
	if len(first) == 1 && first[0] == '[' {
		var assets []core.Asset
		if err := dec.Decode(&assets); err != nil {
			return nil, err
		}
		return assets, nil
	}
	var envelope struct {
		Assets []core.Asset `json:"assets"`
	}
	if err := dec.Decode(&envelope); err != nil {
		return nil, err
	}
	return envelope.Assets, nil
}

// topologyRenderOpts carries everything renderTopology needs that isn't the
// graph itself — kept as a struct so the GET (audit-backed) and POST
// (client-supplied assets) handlers share one rendering tail.
type topologyRenderOpts struct {
	format         string
	hostnames      []string
	includeOrphans bool
	initErrs       []string
	collectErrs    []string
}

func (s *Server) renderTopology(w http.ResponseWriter, topo *topology.Topology, opts topologyRenderOpts) {
	if len(opts.hostnames) > 0 {
		topo = topo.FilterByHostname(opts.hostnames)
	}
	if !opts.includeOrphans {
		topo = topo.DropOrphans()
	}

	// JSON keeps the historical envelope (nodes + edges + init_errors +
	// errors) so existing clients aren't broken. Every other format goes
	// straight through topology.New and lands as a download.
	if opts.format == "json" {
		resp := struct {
			Nodes      []core.Asset `json:"nodes"`
			Edges      []core.Edge  `json:"edges"`
			InitErrors []string     `json:"init_errors,omitempty"`
			Errors     []string     `json:"errors,omitempty"`
		}{
			// Strip Raw from the response — clients don't need the full
			// SDK payload (resolvers already consumed it), and shipping
			// it bloats the JSON considerably.
			Nodes:      stripRaw(topo.Nodes),
			Edges:      topo.Edges,
			InitErrors: opts.initErrs,
			Errors:     opts.collectErrs,
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	renderer, err := topology.New(opts.format)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	contentType, filename := topologyContentType(opts.format)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	if len(opts.initErrs) > 0 {
		w.Header().Set("X-Auditor-Init-Errors", strings.Join(opts.initErrs, "; "))
	}
	if len(opts.collectErrs) > 0 {
		w.Header().Set("X-Auditor-Errors", strings.Join(opts.collectErrs, "; "))
	}
	w.WriteHeader(http.StatusOK)
	_ = renderer.Render(topo, w)
}

// topologyContentType picks the right MIME + filename suffix for each
// non-JSON renderer. excalidraw is JSON in disguise, so the type stays
// application/json — the .excalidraw extension is what makes browsers /
// the OS recognize the download as openable in Excalidraw.
func topologyContentType(format string) (contentType, filename string) {
	switch format {
	case "dot", "graphviz":
		return "text/vnd.graphviz", "topology.dot"
	case "mermaid":
		return "text/plain; charset=utf-8", "topology.mmd"
	case "excalidraw":
		return "application/json", "topology.excalidraw"
	case "html":
		return "text/html; charset=utf-8", "topology.html"
	default:
		return "application/json", "topology.json"
	}
}

func stripRaw(in []core.Asset) []core.Asset {
	out := make([]core.Asset, len(in))
	for i, a := range in {
		a.Raw = nil
		out[i] = a
	}
	return out
}
