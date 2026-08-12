package topology

import (
	"io"
	"sort"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// d2Renderer emits Terrastruct D2 (https://d2lang.com) — a modern
// diagram-as-code language with first-class containers, so --group-by maps
// directly onto D2 containers. Output is deterministic (sorted groups /
// nodes / edges) and self-contained text; render it with the D2 CLI:
//
//	auditor topology -o d2 > topology.d2 && d2 topology.d2 topology.svg
//
// D2's elk/dagre layouts handle dense graphs better than Graphviz for the
// kind of fan-out a cloud inventory produces.
type d2Renderer struct{ groupBy string }

func (r d2Renderer) Render(t *Topology, w io.Writer) error {
	bw := newWriter(w)
	bw.line("# auditor topology — D2 (https://d2lang.com)")
	bw.line("direction: right")
	bw.line("")

	// path maps each node's refKey to the reference used in edge endpoints:
	// "container.node" when grouped, bare "node" when flat.
	path := make(map[string]string)

	for _, g := range groupedNodes(t.Nodes, r.groupBy) {
		var prefix, indent string
		if g.Name != "" {
			cid := sanitizeID("g_" + g.Name)
			bw.linef("%s: %q {", cid, g.Name)
			prefix = cid + "."
			indent = "  "
		}
		for _, n := range g.Nodes {
			nid := d2NodeID(n.AsRef())
			bw.linef("%s%s: %q", indent, nid, d2Label(n))
			path[refKey(n.AsRef())] = prefix + nid
		}
		if g.Name != "" {
			bw.line("}")
		}
	}
	bw.line("")

	edges := append([]core.Edge(nil), t.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		ki := refKey(edges[i].From) + refKey(edges[i].To) + edges[i].Kind
		kj := refKey(edges[j].From) + refKey(edges[j].To) + edges[j].Kind
		return ki < kj
	})
	for _, e := range edges {
		from := path[refKey(e.From)]
		if from == "" {
			from = d2NodeID(e.From)
		}
		to := path[refKey(e.To)]
		if to == "" {
			to = d2NodeID(e.To)
		}
		if e.Confidence == core.ConfidenceHeuristic {
			// Dashed stroke marks a cross-cloud heuristic join, mirroring the
			// dashed edges the dot/mermaid renderers use.
			bw.linef("%s -> %s: %q {", from, to, edgeLabel(e))
			bw.line("  style.stroke-dash: 4")
			bw.line("}")
			continue
		}
		bw.linef("%s -> %s: %q", from, to, edgeLabel(e))
	}
	return bw.err
}

func d2NodeID(r core.AssetRef) string {
	return graphNodeID(r)
}

// d2Label keeps the node label on one line — D2 quoted strings don't carry
// the dot/mermaid two-line "Type\nName" cleanly, so collapse to "name (type)".
func d2Label(a core.Asset) string {
	name := a.Name
	if name == "" {
		name = a.ID
	}
	if t := DisplayType(a); t != "" {
		return name + " (" + t + ")"
	}
	return name
}
