package topology

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Renderer writes a Topology to w in one of the supported formats. The CLI
// selects by --output={json|dot|mermaid|d2|graphml|excalidraw|drawio|html};
// the server serves json directly and routes the rest through here as
// downloads.
type Renderer interface {
	Render(t *Topology, w io.Writer) error
}

// Option customizes a renderer returned by New.
type Option func(*renderOptions)

// renderOptions configures the group-aware renderers.
type renderOptions struct {
	groupBy string // "", "provider", "account", or "region"
}

// WithGroupBy clusters nodes by a dimension (provider|account|region) in the
// renderers that support visual grouping (dot, mermaid, d2, drawio). Empty,
// "none", or any unrecognized value leaves the graph flat. Validation lives
// here so callers can pass a raw flag value straight through.
func WithGroupBy(dim string) Option {
	return func(o *renderOptions) {
		switch d := strings.ToLower(strings.TrimSpace(dim)); d {
		case "provider", "account", "region":
			o.groupBy = d
		default:
			o.groupBy = ""
		}
	}
}

// New returns a renderer for the given format. Unknown formats produce an
// error rather than a default — the CLI surfaces it as a clear message.
func New(format string, opts ...Option) (Renderer, error) {
	var o renderOptions
	for _, fn := range opts {
		fn(&o)
	}
	switch strings.ToLower(format) {
	case "json":
		return &jsonRenderer{}, nil
	case "dot", "graphviz":
		return &dotRenderer{groupBy: o.groupBy}, nil
	case "mermaid":
		return &mermaidRenderer{groupBy: o.groupBy}, nil
	case "d2":
		return &d2Renderer{groupBy: o.groupBy}, nil
	case "graphml":
		return &graphmlRenderer{}, nil
	case "excalidraw":
		return &excalidrawRenderer{}, nil
	case "drawio":
		return &drawioRenderer{groupBy: o.groupBy}, nil
	case "html":
		return &htmlRenderer{}, nil
	default:
		return nil, fmt.Errorf("unknown topology format %q (want json|dot|mermaid|d2|graphml|excalidraw|drawio|html)", format)
	}
}

// ----------------------------------------------------------------------
// grouping (shared by the dot / mermaid / d2 / drawio renderers)
// ----------------------------------------------------------------------

// nodeGroup is a named cluster of nodes; Name == "" is the flat/ungrouped
// pseudo-group rendered without a surrounding container.
type nodeGroup struct {
	Name  string
	Nodes []core.Asset
}

// groupOf returns the cluster a node belongs to under dim. Account/region
// fall back so assets missing that field still land in a sensible bucket
// rather than an empty-named one.
func groupOf(a core.Asset, dim string) string {
	switch dim {
	case "provider":
		return a.Provider
	case "account":
		if a.AccountID != "" {
			return a.AccountID
		}
		return a.Provider
	case "region":
		if a.Region != "" {
			return a.Region
		}
		return "(no region)"
	default:
		return ""
	}
}

// groupedNodes partitions nodes into deterministically ordered clusters.
// Nodes are sorted by refKey within each group, and groups by name; dim==""
// returns a single flat group so callers share one code path.
func groupedNodes(nodes []core.Asset, dim string) []nodeGroup {
	sorted := append([]core.Asset(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool {
		return refKey(sorted[i].AsRef()) < refKey(sorted[j].AsRef())
	})
	if dim == "" {
		return []nodeGroup{{Nodes: sorted}}
	}
	byGroup := map[string][]core.Asset{}
	var names []string
	for _, n := range sorted {
		g := groupOf(n, dim)
		if _, seen := byGroup[g]; !seen {
			names = append(names, g)
		}
		byGroup[g] = append(byGroup[g], n)
	}
	sort.Strings(names)
	out := make([]nodeGroup, 0, len(names))
	for _, g := range names {
		out = append(out, nodeGroup{Name: g, Nodes: byGroup[g]})
	}
	return out
}

// sanitizeID maps an arbitrary string to a graph-safe identifier
// (alphanumerics preserved, everything else collapsed to '_'). Shared by the
// mermaid, d2, and cluster-id paths.
func sanitizeID(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, ch := range s {
		switch {
		case ch >= 'a' && ch <= 'z',
			ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9':
			b.WriteRune(ch)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// graphNodeID is the node identifier used by the mermaid and d2 renderers.
// sanitizeID alone is NOT injective — "svc-a-1" and "svc.a.1" both collapse to
// "svc_a_1", which would merge two distinct nodes (and turn an edge between
// them into a self-loop). Appending a short FNV-1a hash of the canonical
// refKey (provider/id, already unique) makes it collision-free while keeping
// the readable sanitized prefix. Deterministic: same asset → same id.
func graphNodeID(r core.AssetRef) string {
	return sanitizeID(r.Provider+"_"+r.ID) + "_" + shortHash(refKey(r))
}

func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return strconv.FormatUint(uint64(h.Sum32()), 16)
}

// mermaidEscape neutralizes the characters that would break a Mermaid label:
// a double-quote closes the node-label string, a pipe closes the edge label,
// and square brackets close the node shape. HTML entities render as the
// literal character in Mermaid. '<'/'>' are left alone so the "\n"→"<br/>"
// substitution callers apply afterward survives.
func mermaidEscape(s string) string {
	return strings.NewReplacer(
		`"`, "&quot;",
		"|", "&#124;",
		"[", "&#91;",
		"]", "&#93;",
	).Replace(s)
}

// ----------------------------------------------------------------------
// JSON
// ----------------------------------------------------------------------

type jsonRenderer struct{}

func (jsonRenderer) Render(t *Topology, w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	// Drop Asset.Raw on the way out — it bloats the response, the
	// resolvers already extracted what they needed, and it's not
	// useful to the topology consumer.
	stripped := make([]core.Asset, len(t.Nodes))
	for i, a := range t.Nodes {
		a.Raw = nil
		stripped[i] = a
	}
	return enc.Encode(struct {
		Nodes []core.Asset `json:"nodes"`
		Edges []core.Edge  `json:"edges"`
	}{stripped, t.Edges})
}

// ----------------------------------------------------------------------
// DOT (Graphviz)
// ----------------------------------------------------------------------

type dotRenderer struct{ groupBy string }

func (r dotRenderer) Render(t *Topology, w io.Writer) error {
	bw := newWriter(w)

	bw.line("digraph topology {")
	bw.line(`  rankdir=LR;`)
	bw.line(`  compound=true;`)
	bw.line(`  node [shape=box, style="rounded,filled", fillcolor="#f6f8fa", fontname="Helvetica"];`)
	bw.line(`  edge [fontname="Helvetica", fontsize=10];`)
	bw.line("")

	// Node declarations, optionally wrapped in subgraph clusters. Edges are
	// always emitted at the top level so they cross cluster boundaries.
	// groupedNodes guarantees deterministic group + node ordering.
	for _, g := range groupedNodes(t.Nodes, r.groupBy) {
		indent := "  "
		if g.Name != "" {
			bw.linef(`  subgraph %q {`, "cluster_"+sanitizeID(g.Name))
			bw.linef(`    label=%q;`, g.Name)
			bw.line(`    style="rounded,dashed";`)
			bw.line(`    color="#d0d7de";`)
			bw.line(`    fontname="Helvetica";`)
			indent = "    "
		}
		for _, n := range g.Nodes {
			bw.linef(`%s%q [label=%q, tooltip=%q];`, indent, dotID(n.AsRef()), dotLabel(n), n.ID)
		}
		if g.Name != "" {
			bw.line("  }")
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
		style := ""
		if e.Confidence == core.ConfidenceHeuristic {
			style = `, style=dashed`
		}
		color := edgeColor(e)
		if color == "" && e.Confidence == core.ConfidenceHeuristic {
			color = "#8b949e"
		}
		if color != "" {
			style += fmt.Sprintf(`, color=%q, fontcolor=%q`, color, color)
		}
		bw.linef(`  %q -> %q [label=%q%s];`, dotID(e.From), dotID(e.To), edgeLabel(e), style)
	}

	bw.line("}")
	return bw.err
}

func dotID(r core.AssetRef) string {
	return r.Provider + ":" + r.ID
}

func dotLabel(a core.Asset) string {
	name := a.Name
	if name == "" {
		name = a.ID
	}
	return a.Type + "\n" + name
}

func edgeLabel(e core.Edge) string {
	parts := []string{e.Kind}
	if e.Hostname != "" {
		parts = append(parts, e.Hostname)
	}
	if e.Port != 0 {
		parts = append(parts, fmt.Sprintf(":%d", e.Port))
	}
	// A collapsed edge stands for many; without the multiplier a summary
	// diagram shows one arrow between two platforms and gives no sense of
	// whether it represents 1 relationship or 4,000.
	if e.Count > 1 {
		parts = append(parts, fmt.Sprintf("×%d", e.Count))
	}
	return strings.Join(parts, " ")
}

// edgeColor is the accent for an edge kind. Traffic-flow edges are coloured
// by verdict — a denial drawn like a grant would read as reachability, which
// is the exact opposite of what the rule says. Everything else keeps the
// default ink.
func edgeColor(e core.Edge) string {
	switch e.Kind {
	case core.EdgeKindTrafficAllow:
		return "#1a7f37" // green: permitted
	case core.EdgeKindTrafficDeny:
		return "#cf222e" // red: blocked
	default:
		return ""
	}
}

// ----------------------------------------------------------------------
// Mermaid
// ----------------------------------------------------------------------

type mermaidRenderer struct{ groupBy string }

func (r mermaidRenderer) Render(t *Topology, w io.Writer) error {
	bw := newWriter(w)
	bw.line("flowchart LR")

	for _, g := range groupedNodes(t.Nodes, r.groupBy) {
		indent := "  "
		if g.Name != "" {
			bw.linef(`  subgraph %s["%s"]`, sanitizeID("grp_"+g.Name), mermaidEscape(g.Name))
			indent = "    "
		}
		for _, n := range g.Nodes {
			label := strings.ReplaceAll(mermaidEscape(dotLabel(n)), "\n", "<br/>")
			bw.linef(`%s%s["%s"]`, indent, mermaidID(n.AsRef()), label)
		}
		if g.Name != "" {
			bw.line("  end")
		}
	}

	edges := append([]core.Edge(nil), t.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		ki := refKey(edges[i].From) + refKey(edges[i].To) + edges[i].Kind
		kj := refKey(edges[j].From) + refKey(edges[j].To) + edges[j].Kind
		return ki < kj
	})
	// Mermaid has no per-edge attribute syntax: colour is applied afterwards
	// by `linkStyle <index>`, indexed by declaration order. Collect the
	// styled indices while emitting so the two stay in lockstep.
	var styles []string
	for i, e := range edges {
		arrow := "-->"
		if e.Confidence == core.ConfidenceHeuristic {
			arrow = "-.->" // dashed in Mermaid for heuristic matches
		}
		label := mermaidEscape(edgeLabel(e))
		bw.linef(`  %s %s|%s| %s`,
			mermaidID(e.From), arrow, label, mermaidID(e.To))
		if c := edgeColor(e); c != "" {
			styles = append(styles, fmt.Sprintf("  linkStyle %d stroke:%s,stroke-width:2px;", i, c))
		}
	}
	for _, s := range styles {
		bw.line(s)
	}
	return bw.err
}

// mermaidID returns a Mermaid-compatible, collision-free node identifier.
// Mermaid is picky about node IDs containing slashes or dots.
func mermaidID(r core.AssetRef) string {
	return graphNodeID(r)
}

// ----------------------------------------------------------------------
// shared
// ----------------------------------------------------------------------

// writer is a tiny helper so renderers don't have to thread an io.Writer
// through every line and check the same `if err != nil { return err }` on
// every call. The first error sticks; everything after is a no-op.
type writer struct {
	w   io.Writer
	err error
}

func newWriter(w io.Writer) *writer { return &writer{w: w} }

func (b *writer) line(s string) {
	if b.err != nil {
		return
	}
	_, b.err = io.WriteString(b.w, s+"\n")
}

func (b *writer) linef(format string, args ...any) {
	b.line(fmt.Sprintf(format, args...))
}
