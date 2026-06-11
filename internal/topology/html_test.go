package topology_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

func TestNew_HTMLResolves(t *testing.T) {
	r, err := topology.New("html")
	if err != nil {
		t.Fatalf("New(html) errored: %v", err)
	}
	if r == nil {
		t.Fatal("New(html) returned a nil renderer")
	}
}

// extractDataIsland pulls the JSON payload out of the rendered page's
// <script id="topology-data" type="application/json"> tag.
func extractDataIsland(t *testing.T, page string) string {
	t.Helper()
	const open = `<script id="topology-data" type="application/json">`
	start := strings.Index(page, open)
	if start == -1 {
		t.Fatalf("output has no %q data island", open)
	}
	rest := page[start+len(open):]
	end := strings.Index(rest, "</script>")
	if end == -1 {
		t.Fatal("data island is not closed by </script>")
	}
	return rest[:end]
}

func TestRenderer_HTML_DataIslandRoundTrips(t *testing.T) {
	topo := topology.Build(canonicalChain()).DropOrphans()
	r, err := topology.New("html")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := r.Render(topo, &buf); err != nil {
		t.Fatal(err)
	}

	var graph struct {
		Nodes []core.Asset `json:"nodes"`
		Edges []core.Edge  `json:"edges"`
	}
	if err := json.Unmarshal([]byte(extractDataIsland(t, buf.String())), &graph); err != nil {
		t.Fatalf("data island is not valid JSON: %v", err)
	}
	if len(graph.Nodes) != len(topo.Nodes) {
		t.Errorf("data island nodes = %d, want %d", len(graph.Nodes), len(topo.Nodes))
	}
	if len(graph.Edges) != len(topo.Edges) {
		t.Errorf("data island edges = %d, want %d", len(graph.Edges), len(topo.Edges))
	}

	// The static header counts must match the payload — they're the page's
	// "node/edge counts" bar and Go renders them independently of the blob.
	if want := "4 nodes"; !strings.Contains(buf.String(), want) {
		t.Errorf("output missing header count %q", want)
	}
}

func TestRenderer_HTML_OmitsRaw(t *testing.T) {
	// The canonical chain carries Raw on the K8s Service and Ingress; the
	// page must not — same contract as the JSON renderer and the server
	// envelope (a shareable file shouldn't leak provider payloads).
	topo := topology.Build(canonicalChain()).DropOrphans()
	r, _ := topology.New("html")
	var buf bytes.Buffer
	if err := r.Render(topo, &buf); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"raw"`)) {
		t.Errorf("HTML output contains a `raw` key; renderer should strip Asset.Raw")
	}
}

func TestRenderer_HTML_Deterministic(t *testing.T) {
	// Same input → byte-identical output: no timestamps, no randomness, and
	// json.Marshal sorts Tags keys. Mirrors the Excalidraw determinism test.
	topo := topology.Build(canonicalChain()).DropOrphans()
	r, _ := topology.New("html")

	var a, b bytes.Buffer
	if err := r.Render(topo, &a); err != nil {
		t.Fatal(err)
	}
	if err := r.Render(topo, &b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("two renders of the same topology differ — output must be deterministic")
	}
}

func TestRenderer_HTML_EmptyTopology(t *testing.T) {
	// Zero nodes still renders a complete page (the viewer shows its empty
	// state); the data island must hold empty arrays, not null.
	r, _ := topology.New("html")
	var buf bytes.Buffer
	if err := r.Render(&topology.Topology{}, &buf); err != nil {
		t.Fatal(err)
	}
	island := extractDataIsland(t, buf.String())
	if island != `{"nodes":[],"edges":[]}` {
		t.Errorf("empty-topology data island = %q, want empty arrays (not null)", island)
	}
	if want := "0 nodes"; !strings.Contains(buf.String(), want) {
		t.Errorf("output missing header count %q", want)
	}
}
