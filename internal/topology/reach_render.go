package topology

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Renderers for reachability results. Separate from render.go because they
// consume []Path rather than a Topology — a path list has an order that
// carries meaning (shortest first), which the graph renderers deliberately
// discard in favour of deterministic sorting.

// ReachResult is the full answer to a reachability question, and the shape the
// JSON renderer and the HTTP API both serialise.
type ReachResult struct {
	// Question restates what was asked, so a saved JSON file is
	// self-describing months later.
	Question string `json:"question"`

	Sources []core.Asset `json:"sources,omitempty"`
	Targets []core.Asset `json:"targets,omitempty"`
	Paths   []Path       `json:"paths"`

	// Truncated reports that the result hit MaxPaths and more routes exist.
	// Silently capping would read as "these are all the ways in", which for a
	// security question is the dangerous kind of wrong.
	Truncated bool `json:"truncated"`
}

// RenderReach writes a reachability result in the given format.
func RenderReach(res ReachResult, format string, w io.Writer) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "table":
		return renderReachTable(res, w)
	case "json":
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(stripReachRaw(res))
	case "dot", "mermaid", "d2", "graphml", "excalidraw", "drawio", "html":
		// Reuse the graph renderers by turning the paths back into a
		// sub-topology: the union of every node and edge on a path. That
		// keeps one code path for "draw a graph" and means a traced route
		// exports to the same seven formats as a full topology.
		r, err := New(format)
		if err != nil {
			return err
		}
		return r.Render(res.Topology(), w)
	default:
		return fmt.Errorf("unknown reach format %q (want table|json|dot|mermaid|d2|graphml|excalidraw|drawio|html)", format)
	}
}

// Topology collapses the result's paths back into a graph — the union of all
// nodes and edges that appear on any path, deduplicated and deterministically
// ordered.
func (r ReachResult) Topology() *Topology {
	nodeSeen := map[string]bool{}
	edgeSeen := map[string]bool{}
	out := &Topology{}

	for _, p := range r.Paths {
		for _, n := range p.Nodes {
			k := refKey(n.AsRef())
			if !nodeSeen[k] {
				nodeSeen[k] = true
				out.Nodes = append(out.Nodes, n)
			}
		}
		for _, e := range p.Edges {
			k := refKey(e.From) + "\x00" + refKey(e.To) + "\x00" + e.Kind
			if !edgeSeen[k] {
				edgeSeen[k] = true
				out.Edges = append(out.Edges, e)
			}
		}
	}

	sort.SliceStable(out.Nodes, func(i, j int) bool {
		return refKey(out.Nodes[i].AsRef()) < refKey(out.Nodes[j].AsRef())
	})
	sort.SliceStable(out.Edges, func(i, j int) bool {
		a := refKey(out.Edges[i].From) + refKey(out.Edges[i].To) + out.Edges[i].Kind
		b := refKey(out.Edges[j].From) + refKey(out.Edges[j].To) + out.Edges[j].Kind
		return a < b
	})
	return out
}

// stripReachRaw drops Asset.Raw from every node before serialising. The
// resolvers already consumed it, and a path through a Kubernetes cluster would
// otherwise carry a full Pod spec per hop.
func stripReachRaw(r ReachResult) ReachResult {
	clean := func(in []core.Asset) []core.Asset {
		if in == nil {
			return nil
		}
		out := make([]core.Asset, len(in))
		for i, a := range in {
			a.Raw = nil
			out[i] = a
		}
		return out
	}
	r.Sources = clean(r.Sources)
	r.Targets = clean(r.Targets)
	paths := make([]Path, len(r.Paths))
	for i, p := range r.Paths {
		paths[i] = Path{Nodes: clean(p.Nodes), Edges: p.Edges}
	}
	r.Paths = paths
	return r
}

func renderReachTable(res ReachResult, w io.Writer) error {
	bw := newWriter(w)
	bw.line(res.Question)
	bw.line("")

	if len(res.Paths) == 0 {
		bw.line("No paths found.")
		bw.line("")
		// This caveat is the whole reason the renderer is chatty: "no path"
		// from an inferred graph is much weaker than "not reachable", and a
		// reader acting on it as proof of isolation would be misled.
		bw.line("Note: absence of a path is not proof of isolation — it can also mean")
		bw.line("no collected relationship joins the two, e.g. a resolver that needs")
		bw.line("--include-raw, or a provider that wasn't included in the audit.")
		return bw.err
	}

	// Group by destination so "what can reach the database" reads as one
	// block per source rather than an undifferentiated list of routes.
	for i, p := range res.Paths {
		last := p.Nodes[len(p.Nodes)-1]
		marker := ""
		if p.CrossesDeny() {
			marker = "  [crosses a deny rule]"
		}
		bw.linef("%d. %s  (%d hop%s)%s", i+1, describe(last), p.Hops(), plural(p.Hops()), marker)
		for j, e := range p.Edges {
			bw.linef("     %s%s", indentArrow(j), hopLine(p.Nodes[j], e, p.Nodes[j+1]))
		}
		bw.line("")
	}

	bw.linef("%d path%s.", len(res.Paths), plural(len(res.Paths)))
	if res.Truncated {
		bw.line("Result truncated — more paths exist. Raise --max-paths to see them.")
	}
	return bw.err
}

// hopLine renders one hop, annotating heuristic joins so a reader can tell an
// inferred hop from an authoritative one.
//
// The arrow always points the way traffic actually flows, which is not always
// the way the traversal walked. An upstream query ("what can reach X") walks
// edges backwards, so for those hops the node the walk arrived *from* is the
// edge's destination — drawing `X --> Y` there would state the reverse of what
// the edge says. When the walk ran against the edge, the arrow is flipped so
// the line still reads source-to-destination.
func hopLine(walkedFrom core.Asset, e core.Edge, walkedTo core.Asset) string {
	label := e.Kind
	if e.Port != 0 {
		label += fmt.Sprintf(":%d", e.Port)
	}
	if e.Hostname != "" {
		label += " " + e.Hostname
	}
	if e.Confidence == core.ConfidenceHeuristic {
		label += " ~"
	}

	if refKey(e.From) == refKey(walkedFrom.AsRef()) {
		return fmt.Sprintf("%s --[%s]--> %s", describe(walkedFrom), label, describe(walkedTo))
	}
	// Walked against the edge: keep the reading order of the traversal but
	// point the arrow at the true destination.
	return fmt.Sprintf("%s <--[%s]-- %s", describe(walkedFrom), label, describe(walkedTo))
}

func indentArrow(depth int) string { return strings.Repeat("  ", depth) }

func describe(a core.Asset) string {
	name := a.Name
	if name == "" {
		name = a.ID
	}
	return fmt.Sprintf("%s %s", name, dimType(a))
}

func dimType(a core.Asset) string { return "(" + a.Type + ")" }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
