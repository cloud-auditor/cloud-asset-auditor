package topology

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// sampleTopology is a three-provider graph: two Cloudflare DNS records both
// pointing at one OCI load balancer, which fronts two Kubernetes Services.
// One Cloudflare-internal edge exists so internal_edges has something to
// count.
func sampleTopology() *Topology {
	cf1 := core.Asset{Provider: "cloudflare", Type: "cloudflare.dns_record", ID: "d1", Name: "a.example.com"}
	cf2 := core.Asset{Provider: "cloudflare", Type: "cloudflare.dns_record", ID: "d2", Name: "b.example.com"}
	cfz := core.Asset{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1", Name: "example.com"}
	lb := core.Asset{Provider: "oci", Type: "oci.load_balancer", ID: "lb1", Name: "prod-lb", Region: "eu-frankfurt-1"}
	s1 := core.Asset{Provider: "kubernetes", Type: "v1.Service", ID: "s1", Name: "api", AccountID: "c1"}
	s2 := core.Asset{Provider: "kubernetes", Type: "v1.Service", ID: "s2", Name: "web", AccountID: "c1"}

	return &Topology{
		Nodes: []core.Asset{cf1, cf2, cfz, lb, s1, s2},
		Edges: []core.Edge{
			{From: cf1.AsRef(), To: lb.AsRef(), Kind: core.EdgeKindDNS, Confidence: core.ConfidenceHeuristic},
			{From: cf2.AsRef(), To: lb.AsRef(), Kind: core.EdgeKindDNS, Confidence: core.ConfidenceExact},
			{From: cf1.AsRef(), To: cfz.AsRef(), Kind: core.EdgeKindWAF, Confidence: core.ConfidenceExact},
			{From: lb.AsRef(), To: s1.AsRef(), Kind: core.EdgeKindLBBackend, Confidence: core.ConfidenceExact},
			{From: lb.AsRef(), To: s2.AsRef(), Kind: core.EdgeKindLBBackend, Confidence: core.ConfidenceExact},
		},
	}
}

func TestCollapse_LowIsIdentity(t *testing.T) {
	in := sampleTopology()
	if got := in.Collapse(DetailLow, "provider"); got != in {
		t.Error("DetailLow must return the receiver untouched")
	}
	if got := in.Collapse("", "provider"); got != in {
		t.Error("an empty detail level must return the receiver untouched")
	}
}

func TestCollapse_HighOneNodePerProvider(t *testing.T) {
	got := sampleTopology().Collapse(DetailHigh, "provider")

	if len(got.Nodes) != 3 {
		t.Fatalf("got %d nodes, want 3 (one per provider): %v", len(got.Nodes), nodeNames(got.Nodes))
	}
	byID := map[string]core.Asset{}
	for _, n := range got.Nodes {
		byID[n.ID] = n
		if n.Type != CollapsedNodeType {
			t.Errorf("node %q Type = %q, want %q", n.ID, n.Type, CollapsedNodeType)
		}
	}

	cf, ok := byID["group:provider:cloudflare"]
	if !ok {
		t.Fatalf("missing the cloudflare group node; got %v", nodeNames(got.Nodes))
	}
	if cf.Tags["member_count"] != "3" {
		t.Errorf("cloudflare member_count = %q, want 3", cf.Tags["member_count"])
	}
	// The DNS-record → zone edge is entirely inside Cloudflare. It must not
	// survive as a self-loop, but its count must not be silently lost either.
	if cf.Tags["internal_edges"] != "1" {
		t.Errorf("cloudflare internal_edges = %q, want 1", cf.Tags["internal_edges"])
	}
	if got, want := cf.Tags["member_types"], "cloudflare.dns_record,cloudflare.zone"; got != want {
		t.Errorf("cloudflare member_types = %q, want %q", got, want)
	}

	// Two DNS edges collapse into one weighted arrow; two LB edges likewise.
	edges := edgeSet(got.Edges)
	dns, ok := edges["group:provider:cloudflare→group:provider:oci:dns"]
	if !ok {
		t.Fatalf("missing the collapsed cloudflare→oci edge; got %v", edgeKeys(got.Edges))
	}
	if dns.Count != 2 {
		t.Errorf("collapsed dns edge Count = %d, want 2", dns.Count)
	}
	// One heuristic constituent makes the whole aggregate heuristic — a
	// summary arrow is only as trustworthy as its weakest member.
	if dns.Confidence != core.ConfidenceHeuristic {
		t.Errorf("collapsed dns edge Confidence = %q, want heuristic", dns.Confidence)
	}

	lb := edges["group:provider:oci→group:provider:kubernetes:lb-backend"]
	if lb.Count != 2 {
		t.Errorf("collapsed lb-backend Count = %d, want 2", lb.Count)
	}
	if lb.Confidence != core.ConfidenceExact {
		t.Errorf("all-exact aggregate Confidence = %q, want exact", lb.Confidence)
	}

	// No self-loops in the output.
	for _, e := range got.Edges {
		if e.From.ID == e.To.ID {
			t.Errorf("collapse left a self-loop: %+v", e)
		}
	}
}

func TestCollapse_MediumSplitsByType(t *testing.T) {
	got := sampleTopology().Collapse(DetailMedium, "provider")

	// cloudflare{dns_record, zone} + oci{load_balancer} + kubernetes{Service}
	if len(got.Nodes) != 4 {
		t.Fatalf("got %d nodes, want 4: %v", len(got.Nodes), nodeNames(got.Nodes))
	}
	byID := map[string]core.Asset{}
	for _, n := range got.Nodes {
		byID[n.ID] = n
	}
	rec, ok := byID["group:provider:cloudflare/cloudflare.dns_record"]
	if !ok {
		t.Fatalf("missing the per-type node; got %v", nodeNames(got.Nodes))
	}
	if rec.Tags["member_count"] != "2" {
		t.Errorf("dns_record member_count = %q, want 2", rec.Tags["member_count"])
	}
	if rec.Tags["member_type"] != "cloudflare.dns_record" {
		t.Errorf("member_type = %q", rec.Tags["member_type"])
	}
	// At medium detail the record→zone edge crosses two buckets, so it
	// survives as a real edge rather than becoming an internal count.
	if _, ok := edgeSet(got.Edges)["group:provider:cloudflare/cloudflare.dns_record→group:provider:cloudflare/cloudflare.zone:waf"]; !ok {
		t.Errorf("intra-provider cross-type edge was dropped: %v", edgeKeys(got.Edges))
	}
}

func TestCollapse_GroupByAccountAndRegion(t *testing.T) {
	got := sampleTopology().Collapse(DetailHigh, "account")
	for _, n := range got.Nodes {
		if n.Tags["group_by"] != "account" {
			t.Errorf("node %q group_by = %q, want account", n.ID, n.Tags["group_by"])
		}
	}
	// Assets with no AccountID fall back to their provider name (groupOf).
	if len(got.Nodes) != 3 {
		t.Errorf("account grouping produced %d nodes, want 3: %v", len(got.Nodes), nodeNames(got.Nodes))
	}

	byRegion := sampleTopology().Collapse(DetailHigh, "region")
	var multi bool
	for _, n := range byRegion.Nodes {
		// The "(no region)" bucket spans cloudflare + kubernetes, so it must
		// not claim either provider as its own.
		if n.Provider == "multi" {
			multi = true
		}
	}
	if !multi {
		t.Errorf("a region bucket spanning providers should report Provider=multi; got %v", nodeProviders(byRegion.Nodes))
	}
}

// An empty dim must not produce a single unnamed mega-bucket — provider is
// the fallback because every asset has one.
func TestCollapse_EmptyDimDefaultsToProvider(t *testing.T) {
	got := sampleTopology().Collapse(DetailHigh, "")
	if len(got.Nodes) != 3 {
		t.Errorf("empty dim produced %d nodes, want 3 (provider fallback): %v", len(got.Nodes), nodeNames(got.Nodes))
	}
	for _, n := range got.Nodes {
		if n.Tags["group_by"] != "provider" {
			t.Errorf("group_by = %q, want provider", n.Tags["group_by"])
		}
	}
}

// Two collapses of the same input must be byte-identical once rendered, or
// the diagram churns between runs.
func TestCollapse_Deterministic(t *testing.T) {
	r, err := New("dot")
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for i := range 20 {
		var buf bytes.Buffer
		if err := r.Render(sampleTopology().Collapse(DetailHigh, "provider"), &buf); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = buf.String()
			continue
		}
		if buf.String() != first {
			t.Fatalf("run %d rendered differently:\n%s", i, buf.String())
		}
	}
}

// A collapsed edge must show its multiplier — otherwise one arrow between two
// platforms gives no sense of whether it stands for 1 relationship or 4,000.
func TestRender_CollapsedEdgeShowsCount(t *testing.T) {
	r, err := New("dot")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := r.Render(sampleTopology().Collapse(DetailHigh, "provider"), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "×2") {
		t.Errorf("collapsed dot output has no edge multiplier:\n%s", buf.String())
	}
}

// A denial drawn like a grant reads as reachability — the exact opposite of
// what the rule says. Verify the two verdicts render in different colours.
func TestRender_TrafficEdgesAreColouredByVerdict(t *testing.T) {
	from := core.Asset{Provider: "tailscale", Type: "tailscale.device", ID: "n1", Name: "a"}
	to := core.Asset{Provider: "tailscale", Type: "tailscale.device", ID: "n2", Name: "b"}
	topo := &Topology{
		Nodes: []core.Asset{from, to},
		Edges: []core.Edge{
			{From: from.AsRef(), To: to.AsRef(), Kind: core.EdgeKindTrafficAllow, Confidence: core.ConfidenceExact},
			{From: to.AsRef(), To: from.AsRef(), Kind: core.EdgeKindTrafficDeny, Confidence: core.ConfidenceExact},
		},
	}
	for _, format := range []string{"dot", "mermaid"} {
		r, err := New(format)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := r.Render(topo, &buf); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if !strings.Contains(out, "#1a7f37") || !strings.Contains(out, "#cf222e") {
			t.Errorf("%s output does not colour allow and deny differently:\n%s", format, out)
		}
	}
}

func TestParseDetail(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", DetailLow, false},
		{"low", DetailLow, false},
		{"MEDIUM", DetailMedium, false},
		{" high ", DetailHigh, false},
		// An unrecognised value must fail loudly: silently rendering 40,000
		// nodes when a summary was asked for is the worst outcome available.
		{"summary", "", true},
	} {
		got, err := ParseDetail(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseDetail(%q) should have failed", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDetail(%q) = %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseDetail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- helpers ---

func nodeNames(nodes []core.Asset) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}

func nodeProviders(nodes []core.Asset) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID + "=" + n.Provider
	}
	return out
}

// At high detail the collapse already yields one node per group, so also
// clustering by that dimension would box every node on its own.
func TestRenderGroupBy_SuppressedAtHighDetail(t *testing.T) {
	if got := RenderGroupBy(DetailHigh, "provider"); got != "" {
		t.Errorf("RenderGroupBy(high, provider) = %q, want \"\"", got)
	}
	for _, level := range []string{DetailLow, DetailMedium} {
		if got := RenderGroupBy(level, "provider"); got != "provider" {
			t.Errorf("RenderGroupBy(%s, provider) = %q, want provider", level, got)
		}
	}
}

// A collapsed node's Type is the synthetic CollapsedNodeType. Printing it in a
// label says only "this is a group", which the label, the member count and the
// enclosing cluster already say — and repeating it on every box is what turned
// a high-level diagram into a wall of "(topology.group)".
func TestDisplayType_OmitsTheSyntheticGroupType(t *testing.T) {
	if got := DisplayType(core.Asset{Type: CollapsedNodeType}); got != "" {
		t.Errorf("collapsed node display type = %q, want empty", got)
	}
	if got := DisplayType(core.Asset{Type: "v1.Pod"}); got != "v1.Pod" {
		t.Errorf("real type must survive, got %q", got)
	}
}
