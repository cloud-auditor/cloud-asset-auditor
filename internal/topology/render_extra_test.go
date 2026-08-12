package topology_test

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

// renderChain renders the shared canonicalChain() fixture through a format.
// Orphans are kept (Build doesn't drop them) so grouping covers every node.
func renderChain(t *testing.T, format string, opts ...topology.Option) string {
	t.Helper()
	topo := topology.Build(canonicalChain())
	r, err := topology.New(format, opts...)
	if err != nil {
		t.Fatalf("New(%q): %v", format, err)
	}
	var buf bytes.Buffer
	if err := r.Render(topo, &buf); err != nil {
		t.Fatalf("Render(%q): %v", format, err)
	}
	return buf.String()
}

// ----------------------------------------------------------------------
// D2
// ----------------------------------------------------------------------

func TestRenderer_D2_FlatHasNodesAndEdges(t *testing.T) {
	out := renderChain(t, "d2")
	for _, want := range []string{
		"direction: right",
		"cloudflare.zone",   // a node label fragment
		" -> ",              // at least one edge
		"style.stroke-dash", // heuristic DNS edge marked dashed
	} {
		if !strings.Contains(out, want) {
			t.Errorf("d2 output missing %q\n---\n%s", want, out)
		}
	}
	// No container syntax when flat.
	if strings.Contains(out, `: "cloudflare" {`) {
		t.Errorf("flat d2 should not emit containers:\n%s", out)
	}
}

func TestRenderer_D2_GroupByProviderEmitsContainers(t *testing.T) {
	out := renderChain(t, "d2", topology.WithGroupBy("provider"))
	for _, want := range []string{
		`: "cloudflare" {`,
		`: "oci" {`,
		`: "kubernetes" {`,
		"g_cloudflare.", // edges reference container-qualified paths
	} {
		if !strings.Contains(out, want) {
			t.Errorf("grouped d2 missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderer_D2_Deterministic(t *testing.T) {
	a := renderChain(t, "d2", topology.WithGroupBy("provider"))
	b := renderChain(t, "d2", topology.WithGroupBy("provider"))
	if a != b {
		t.Errorf("d2 output not deterministic across two renders")
	}
}

// ----------------------------------------------------------------------
// GraphML
// ----------------------------------------------------------------------

func TestRenderer_GraphML_WellFormedAndComplete(t *testing.T) {
	out := renderChain(t, "graphml")

	if !strings.HasPrefix(out, "<?xml") {
		t.Errorf("graphml output missing XML declaration:\n%s", out[:min(80, len(out))])
	}
	if !strings.Contains(out, "graphml.graphdrawing.org") {
		t.Errorf("graphml output missing namespace")
	}

	// Parse it back to prove it's well-formed and count nodes/edges.
	var doc struct {
		Keys []struct {
			ID  string `xml:"id,attr"`
			For string `xml:"for,attr"`
		} `xml:"key"`
		Graph struct {
			Nodes []struct {
				ID string `xml:"id,attr"`
			} `xml:"node"`
			Edges []struct {
				Source string `xml:"source,attr"`
				Target string `xml:"target,attr"`
			} `xml:"edge"`
		} `xml:"graph"`
	}
	if err := xml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("graphml is not valid XML: %v", err)
	}

	topo := topology.Build(canonicalChain())
	if len(doc.Graph.Nodes) != len(topo.Nodes) {
		t.Errorf("graphml node count = %d, want %d", len(doc.Graph.Nodes), len(topo.Nodes))
	}
	if len(doc.Graph.Edges) != len(topo.Edges) {
		t.Errorf("graphml edge count = %d, want %d", len(doc.Graph.Edges), len(topo.Edges))
	}
	if len(doc.Keys) == 0 {
		t.Errorf("graphml declares no <key> attributes")
	}
	// Every edge endpoint must reference a declared node id.
	ids := map[string]bool{}
	for _, n := range doc.Graph.Nodes {
		ids[n.ID] = true
	}
	for _, e := range doc.Graph.Edges {
		if !ids[e.Source] || !ids[e.Target] {
			t.Errorf("edge %s->%s references an undeclared node", e.Source, e.Target)
		}
	}
}

func TestRenderer_GraphML_Deterministic(t *testing.T) {
	a := renderChain(t, "graphml")
	b := renderChain(t, "graphml")
	if a != b {
		t.Errorf("graphml output not deterministic across two renders")
	}
}

// ----------------------------------------------------------------------
// grouping in dot + mermaid
// ----------------------------------------------------------------------

func TestRenderer_DOT_GroupByProviderEmitsClusters(t *testing.T) {
	out := renderChain(t, "dot", topology.WithGroupBy("provider"))
	for _, want := range []string{
		`subgraph "cluster_cloudflare"`,
		`label="cloudflare"`,
		`subgraph "cluster_oci"`,
		`subgraph "cluster_kubernetes"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("grouped dot missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderer_Mermaid_GroupByProviderEmitsSubgraphs(t *testing.T) {
	out := renderChain(t, "mermaid", topology.WithGroupBy("provider"))
	if !strings.Contains(out, `subgraph grp_cloudflare["cloudflare"]`) {
		t.Errorf("grouped mermaid missing cloudflare subgraph\n---\n%s", out)
	}
	if strings.Count(out, "  end") < 3 {
		t.Errorf("grouped mermaid should close 3 subgraphs (cloudflare/oci/kubernetes)\n---\n%s", out)
	}
}

func TestWithGroupBy_UnknownDimensionIsFlat(t *testing.T) {
	flat := renderChain(t, "dot")
	for _, dim := range []string{"bogus", "none", "", "  "} {
		if got := renderChain(t, "dot", topology.WithGroupBy(dim)); got != flat {
			t.Errorf("WithGroupBy(%q) should render flat (== no grouping)", dim)
		}
	}
}

func renderAssets(t *testing.T, assets []core.Asset, format string) string {
	t.Helper()
	r, err := topology.New(format)
	if err != nil {
		t.Fatalf("New(%q): %v", format, err)
	}
	var buf bytes.Buffer
	if err := r.Render(topology.Build(assets), &buf); err != nil {
		t.Fatalf("Render(%q): %v", format, err)
	}
	return buf.String()
}

// Two assets whose provider+id differ only in separator characters used to
// collapse to the SAME sanitized node id (svc-a-1 vs svc.a.1 → svc_a_1),
// merging distinct nodes. graphNodeID's hash suffix must keep them distinct.
func TestRenderer_NodeIDs_Injective(t *testing.T) {
	assets := []core.Asset{
		{Provider: "kubernetes", AccountID: "c", Type: "v1.Service", ID: "svc-a-1", Name: "dash"},
		{Provider: "kubernetes", AccountID: "c", Type: "v1.ConfigMap", ID: "svc.a.1", Name: "dot"},
	}
	// Mermaid: node lines look like `id["label"]`.
	mIDs := map[string]bool{}
	for _, line := range strings.Split(renderAssets(t, assets, "mermaid"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "subgraph") {
			continue
		}
		if i := strings.Index(line, `["`); i > 0 {
			mIDs[line[:i]] = true
		}
	}
	if len(mIDs) != 2 {
		t.Errorf("mermaid produced %d distinct node ids, want 2: %v", len(mIDs), mIDs)
	}
	// D2: node lines look like `id: "label"`.
	dIDs := map[string]bool{}
	for _, line := range strings.Split(renderAssets(t, assets, "d2"), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, `: "`); i > 0 && !strings.Contains(line, " -> ") {
			dIDs[line[:i]] = true
		}
	}
	if len(dIDs) != 2 {
		t.Errorf("d2 produced %d distinct node ids, want 2: %v", len(dIDs), dIDs)
	}
	// draw.io: one vertex mxCell per node, keyed by id.
	xIDs := map[string]bool{}
	for _, c := range parseDrawioCells(t, renderAssets(t, assets, "drawio")) {
		if c.Vertex == "1" {
			xIDs[c.ID] = true
		}
	}
	if len(xIDs) != 2 {
		t.Errorf("drawio produced %d distinct node ids, want 2: %v", len(xIDs), xIDs)
	}
}

// ----------------------------------------------------------------------
// draw.io (mxGraph XML)
// ----------------------------------------------------------------------

// drawioCell mirrors the attributes the drawio renderer emits per mxCell —
// enough to prove the output round-trips through an XML parser.
type drawioCell struct {
	ID          string `xml:"id,attr"`
	Value       string `xml:"value,attr"`
	Style       string `xml:"style,attr"`
	Parent      string `xml:"parent,attr"`
	Source      string `xml:"source,attr"`
	Target      string `xml:"target,attr"`
	Vertex      string `xml:"vertex,attr"`
	Edge        string `xml:"edge,attr"`
	Connectable string `xml:"connectable,attr"`
}

func parseDrawioCells(t *testing.T, out string) []drawioCell {
	t.Helper()
	var doc struct {
		Diagram struct {
			Model struct {
				Root struct {
					Cells []drawioCell `xml:"mxCell"`
				} `xml:"root"`
			} `xml:"mxGraphModel"`
		} `xml:"diagram"`
	}
	if err := xml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("drawio output is not valid XML: %v\n%s", err, out)
	}
	return doc.Diagram.Model.Root.Cells
}

func TestRenderer_Drawio_Structure(t *testing.T) {
	out := renderChain(t, "drawio")
	topo := topology.Build(canonicalChain())

	for _, want := range []string{
		"<mxfile",
		"<mxGraphModel",
		`<mxCell id="0">`,            // model root
		`<mxCell id="1" parent="0">`, // default layer
		`source="`,
		`target="`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drawio output missing %q\n---\n%s", want, out)
		}
	}
	if got := strings.Count(out, `vertex="1"`); got != len(topo.Nodes) {
		t.Errorf("drawio vertex count = %d, want %d", got, len(topo.Nodes))
	}
	if got := strings.Count(out, `edge="1"`); got != len(topo.Edges) {
		t.Errorf("drawio edge count = %d, want %d", got, len(topo.Edges))
	}
}

func TestRenderer_Drawio_RoundTrip(t *testing.T) {
	cells := parseDrawioCells(t, renderChain(t, "drawio"))
	topo := topology.Build(canonicalChain())

	vertexIDs := map[string]bool{}
	var edges []drawioCell
	for _, c := range cells {
		switch {
		case c.Vertex == "1":
			vertexIDs[c.ID] = true
		case c.Edge == "1":
			edges = append(edges, c)
		}
	}
	if len(vertexIDs) != len(topo.Nodes) {
		t.Errorf("drawio node-cell count = %d, want %d", len(vertexIDs), len(topo.Nodes))
	}
	if len(edges) != len(topo.Edges) {
		t.Errorf("drawio edge-cell count = %d, want %d", len(edges), len(topo.Edges))
	}
	for _, e := range edges {
		if !vertexIDs[e.Source] || !vertexIDs[e.Target] {
			t.Errorf("edge %s: %s->%s references an undeclared vertex", e.ID, e.Source, e.Target)
		}
	}
}

func TestRenderer_Drawio_Deterministic(t *testing.T) {
	flatA := renderChain(t, "drawio")
	flatB := renderChain(t, "drawio")
	if flatA != flatB {
		t.Errorf("flat drawio output not deterministic across two renders")
	}
	a := renderChain(t, "drawio", topology.WithGroupBy("provider"))
	b := renderChain(t, "drawio", topology.WithGroupBy("provider"))
	if a != b {
		t.Errorf("grouped drawio output not deterministic across two renders")
	}
}

func TestRenderer_Drawio_GroupBy(t *testing.T) {
	cells := parseDrawioCells(t, renderChain(t, "drawio", topology.WithGroupBy("provider")))

	// Containers are the non-connectable vertices; one per provider.
	containers := map[string]string{} // id → value (group name)
	for _, c := range cells {
		if c.Vertex == "1" && c.Connectable == "0" {
			containers[c.ID] = c.Value
		}
	}
	seen := map[string]bool{}
	for _, name := range containers {
		seen[name] = true
	}
	for _, want := range []string{"cloudflare", "oci", "kubernetes"} {
		if !seen[want] {
			t.Errorf("grouped drawio missing container for %q (got %v)", want, containers)
		}
	}
	if len(containers) != 3 {
		t.Errorf("grouped drawio container count = %d, want 3: %v", len(containers), containers)
	}

	// Member nodes parent to their container, not the root layer.
	for _, c := range cells {
		if c.Vertex != "1" || c.Connectable == "0" {
			continue
		}
		if c.Parent == "1" {
			t.Errorf("grouped node %s parents to the root layer, want a container", c.ID)
		}
		if _, ok := containers[c.Parent]; !ok {
			t.Errorf("grouped node %s parents to %q, not a container cell", c.ID, c.Parent)
		}
	}

	// Flat render must produce no containers.
	for _, c := range parseDrawioCells(t, renderChain(t, "drawio")) {
		if c.Connectable == "0" {
			t.Errorf("flat drawio should not emit containers, found %s (%q)", c.ID, c.Value)
		}
	}
}

func TestRenderer_Drawio_HeuristicDashed(t *testing.T) {
	var dashed, solid int
	for _, c := range parseDrawioCells(t, renderChain(t, "drawio")) {
		if c.Edge != "1" {
			continue
		}
		if strings.Contains(c.Style, "dashed=1") {
			dashed++
		} else {
			solid++
		}
	}
	// The canonical chain has heuristic DNS/LB edges and exact
	// gateway-route edges, so both styles must appear.
	if dashed == 0 {
		t.Errorf("no dashed edge cells — heuristic edges should carry dashed=1")
	}
	if solid == 0 {
		t.Errorf("no solid edge cells — exact edges should not carry dashed=1")
	}
}

// An asset Name containing Mermaid metacharacters must be escaped, not leaked
// verbatim into the label/edge syntax.
func TestRenderer_Mermaid_EscapesSpecialChars(t *testing.T) {
	assets := []core.Asset{
		{Provider: "oci", AccountID: "t", Type: "oci.load_balancer", ID: "lb1", Name: `my"lb|x]`,
			Tags: map[string]string{"ip_addresses": "1.2.3.4"}},
		{Provider: "cloudflare", AccountID: "a", Type: "cloudflare.dns_record", ID: "r1", Name: "x.example.com",
			Tags: map[string]string{"type": "A", "content": "1.2.3.4"}}, // shares IP → an edge exists
	}
	out := renderAssets(t, assets, "mermaid")
	if strings.Contains(out, `my"lb`) {
		t.Errorf("unescaped double-quote leaked into mermaid label:\n%s", out)
	}
	if !strings.Contains(out, "&quot;") || !strings.Contains(out, "&#124;") {
		t.Errorf("expected escaped entities (&quot; / &#124;) in mermaid output:\n%s", out)
	}
}
