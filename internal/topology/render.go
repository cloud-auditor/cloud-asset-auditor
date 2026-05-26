package topology

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Renderer writes a Topology to w in one of the three supported formats.
// The CLI selects by --output={json|dot|mermaid}; the server only emits json.
type Renderer interface {
	Render(t *Topology, w io.Writer) error
}

// New returns a renderer for the given format. Unknown formats produce an
// error rather than a default — the CLI surfaces it as a clear message.
func New(format string) (Renderer, error) {
	switch strings.ToLower(format) {
	case "json":
		return &jsonRenderer{}, nil
	case "dot", "graphviz":
		return &dotRenderer{}, nil
	case "mermaid":
		return &mermaidRenderer{}, nil
	default:
		return nil, fmt.Errorf("unknown topology format %q (want json|dot|mermaid)", format)
	}
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

type dotRenderer struct{}

func (dotRenderer) Render(t *Topology, w io.Writer) error {
	bw := newWriter(w)

	bw.line("digraph topology {")
	bw.line(`  rankdir=LR;`)
	bw.line(`  node [shape=box, style="rounded,filled", fillcolor="#f6f8fa", fontname="Helvetica"];`)
	bw.line(`  edge [fontname="Helvetica", fontsize=10];`)
	bw.line("")

	// Stable node ordering for deterministic output (helps diffs + tests).
	nodes := append([]core.Asset(nil), t.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		return refKey(nodes[i].AsRef()) < refKey(nodes[j].AsRef())
	})
	for _, n := range nodes {
		bw.linef(`  %q [label=%q, tooltip=%q];`, dotID(n.AsRef()), dotLabel(n), n.ID)
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
			style = `, style=dashed, color="#8b949e"`
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
	return strings.Join(parts, " ")
}

// ----------------------------------------------------------------------
// Mermaid
// ----------------------------------------------------------------------

type mermaidRenderer struct{}

func (mermaidRenderer) Render(t *Topology, w io.Writer) error {
	bw := newWriter(w)
	bw.line("flowchart LR")

	nodes := append([]core.Asset(nil), t.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		return refKey(nodes[i].AsRef()) < refKey(nodes[j].AsRef())
	})
	for _, n := range nodes {
		label := strings.ReplaceAll(dotLabel(n), "\n", "<br/>")
		bw.linef(`  %s["%s"]`, mermaidID(n.AsRef()), label)
	}

	edges := append([]core.Edge(nil), t.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		ki := refKey(edges[i].From) + refKey(edges[i].To) + edges[i].Kind
		kj := refKey(edges[j].From) + refKey(edges[j].To) + edges[j].Kind
		return ki < kj
	})
	for _, e := range edges {
		arrow := "-->"
		if e.Confidence == core.ConfidenceHeuristic {
			arrow = "-.->" // dashed in Mermaid for heuristic matches
		}
		label := edgeLabel(e)
		bw.linef(`  %s %s|%s| %s`,
			mermaidID(e.From), arrow, label, mermaidID(e.To))
	}
	return bw.err
}

// mermaidID returns a Mermaid-compatible node identifier (alphanumerics +
// underscore only). Mermaid is picky about node IDs containing slashes
// or dots.
func mermaidID(r core.AssetRef) string {
	id := r.Provider + "_" + r.ID
	var b strings.Builder
	b.Grow(len(id))
	for _, ch := range id {
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
