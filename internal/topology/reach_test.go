package topology

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// reachFixture is a realistic exposure chain plus a decoy that must NOT be
// reported: internet → DNS → OCI LB → K8s Service → Pod, alongside an
// unrelated internal-only service.
func reachFixture() *Topology {
	dns := core.Asset{Provider: "cloudflare", Type: "cloudflare.dns_record", ID: "d1", Name: "api.example.com"}
	lb := core.Asset{Provider: "oci", Type: "oci.load_balancer", ID: "lb1", Name: "prod-lb"}
	svc := core.Asset{Provider: "kubernetes", AccountID: "c1", Type: "v1.Service", ID: "svc1", Name: "api"}
	pod := core.Asset{Provider: "kubernetes", AccountID: "c1", Type: "v1.Pod", ID: "pod1", Name: "api-abc"}
	db := core.Asset{Provider: "kubernetes", AccountID: "c1", Type: "v1.Pod", ID: "db1", Name: "postgres-0"}
	internal := core.Asset{Provider: "kubernetes", AccountID: "c1", Type: "v1.Service", ID: "svc2", Name: "internal-only"}
	rule := core.Asset{Provider: "kubernetes", AccountID: "c1", Type: "networking.k8s.io/v1.NetworkPolicy", ID: "np1", Name: "db-allow"}

	return &Topology{
		Nodes: []core.Asset{dns, lb, svc, pod, db, internal, rule},
		Edges: []core.Edge{
			{From: dns.AsRef(), To: lb.AsRef(), Kind: core.EdgeKindDNS, Confidence: core.ConfidenceHeuristic},
			{From: lb.AsRef(), To: svc.AsRef(), Kind: core.EdgeKindLBBackend, Confidence: core.ConfidenceHeuristic},
			{From: svc.AsRef(), To: pod.AsRef(), Kind: core.EdgeKindServiceBackend, Confidence: core.ConfidenceExact},
			// api pod → policy → db pod (the app reaching its database)
			{From: pod.AsRef(), To: rule.AsRef(), Kind: core.EdgeKindTrafficAllow, Port: 5432, Confidence: core.ConfidenceExact},
			{From: rule.AsRef(), To: db.AsRef(), Kind: core.EdgeKindTrafficAllow, Port: 5432, Confidence: core.ConfidenceExact},
			// internal-only service is connected, but not from any entry point
			{From: internal.AsRef(), To: db.AsRef(), Kind: core.EdgeKindServiceBackend, Confidence: core.ConfidenceExact},
		},
	}
}

func pathEnds(paths []Path) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = p.Nodes[len(p.Nodes)-1].ID
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// --- direction ------------------------------------------------------------

// The single most easily-inverted thing in this file: "what can reach X"
// walks edges backwards, "what can X reach" walks forwards.
func TestReachable_DirectionIsNotInverted(t *testing.T) {
	topo := reachFixture()

	down := pathEnds(topo.Reachable(topo.Select("api.example.com"), Downstream, ReachOptions{}))
	for _, want := range []string{"lb1", "svc1", "pod1", "np1", "db1"} {
		if !contains(down, want) {
			t.Errorf("downstream from the DNS record should reach %s; got %v", want, down)
		}
	}

	up := pathEnds(topo.Reachable(topo.Select("postgres-0"), Upstream, ReachOptions{}))
	for _, want := range []string{"np1", "pod1", "svc1", "lb1", "d1", "svc2"} {
		if !contains(up, want) {
			t.Errorf("upstream from the database should find %s; got %v", want, up)
		}
	}
	// Nothing is downstream of the database — it's a leaf.
	if got := topo.Reachable(topo.Select("postgres-0"), Downstream, ReachOptions{}); len(got) != 0 {
		t.Errorf("database has no downstream, got %v", pathEnds(got))
	}
}

// "What can X reach" must not answer "X".
func TestReachable_ExcludesTheSeedItself(t *testing.T) {
	topo := reachFixture()
	for _, p := range topo.Reachable(topo.Select("api.example.com"), Downstream, ReachOptions{}) {
		if last := p.Nodes[len(p.Nodes)-1].ID; last == "d1" {
			t.Error("the seed appeared in its own reachability result")
		}
	}
}

func TestReachable_RespectsMaxHops(t *testing.T) {
	topo := reachFixture()
	got := topo.Reachable(topo.Select("api.example.com"), Downstream, ReachOptions{MaxHops: 2})
	for _, p := range got {
		if p.Hops() > 2 {
			t.Errorf("path of %d hops exceeds MaxHops=2: %v", p.Hops(), pathEnds([]Path{p}))
		}
	}
	// lb1 is 1 hop, svc1 is 2 — pod1 at 3 must be excluded.
	ends := pathEnds(got)
	if !contains(ends, "svc1") || contains(ends, "pod1") {
		t.Errorf("MaxHops=2 should reach svc1 but not pod1; got %v", ends)
	}
}

// --- deny semantics -------------------------------------------------------

// A deny edge says traffic does NOT flow. Traversing it while computing
// reachability would manufacture routes that policy forbids, so it must be
// excluded unless explicitly asked for.
func TestReachable_DenyEdgesAreNotTraversedByDefault(t *testing.T) {
	a := core.Asset{Provider: "tailscale", Type: "tailscale.device", ID: "n1", Name: "laptop"}
	rule := core.Asset{Provider: "tailscale", Type: "tailscale.acl_rule", ID: "r1", Name: "block"}
	b := core.Asset{Provider: "tailscale", Type: "tailscale.device", ID: "n2", Name: "db"}
	topo := &Topology{
		Nodes: []core.Asset{a, rule, b},
		Edges: []core.Edge{
			{From: a.AsRef(), To: rule.AsRef(), Kind: core.EdgeKindTrafficDeny, Confidence: core.ConfidenceExact},
			{From: rule.AsRef(), To: b.AsRef(), Kind: core.EdgeKindTrafficDeny, Confidence: core.ConfidenceExact},
		},
	}

	if got := topo.Reachable(topo.Select("laptop"), Downstream, ReachOptions{}); len(got) != 0 {
		t.Errorf("deny-only graph should yield no reachability, got %v", pathEnds(got))
	}
	got := topo.Reachable(topo.Select("laptop"), Downstream, ReachOptions{IncludeDeny: true})
	if !contains(pathEnds(got), "n2") {
		t.Errorf("--include-deny should surface the denied route for auditing; got %v", pathEnds(got))
	}
	for _, p := range got {
		if len(p.Edges) > 0 && !p.CrossesDeny() {
			t.Error("CrossesDeny should flag a path built from deny edges")
		}
	}
}

func TestReachOptions_KindsFilter(t *testing.T) {
	topo := reachFixture()
	got := topo.Reachable(topo.Select("api-abc"), Downstream, ReachOptions{Kinds: []string{core.EdgeKindTrafficAllow}})
	ends := pathEnds(got)
	if !contains(ends, "np1") || !contains(ends, "db1") {
		t.Errorf("traffic-allow-only traversal should reach the policy and the db; got %v", ends)
	}

	// Restricting to a kind that isn't on any outgoing edge yields nothing.
	if got := topo.Reachable(topo.Select("api-abc"), Downstream, ReachOptions{Kinds: []string{core.EdgeKindDNS}}); len(got) != 0 {
		t.Errorf("dns-only traversal from a pod should find nothing, got %v", pathEnds(got))
	}
}

// --- path enumeration -----------------------------------------------------

func TestPaths_TracesTheFullChain(t *testing.T) {
	topo := reachFixture()
	got := topo.Paths(topo.Select("api.example.com"), topo.Select("postgres-0"), ReachOptions{})
	if len(got) == 0 {
		t.Fatal("expected at least one path from the DNS record to the database")
	}
	p := got[0]
	var ids []string
	for _, n := range p.Nodes {
		ids = append(ids, n.ID)
	}
	want := "d1 lb1 svc1 pod1 np1 db1"
	if strings.Join(ids, " ") != want {
		t.Errorf("path = %q, want %q", strings.Join(ids, " "), want)
	}
	if p.Hops() != 5 {
		t.Errorf("Hops() = %d, want 5", p.Hops())
	}
	if len(p.Edges) != len(p.Nodes)-1 {
		t.Errorf("len(Edges)=%d must be len(Nodes)-1=%d", len(p.Edges), len(p.Nodes)-1)
	}
}

// A path that reaches the target and keeps going answers a different question
// ("what's downstream of the target"), not "another way in".
func TestPaths_DoesNotExtendPastTheTarget(t *testing.T) {
	topo := reachFixture()
	got := topo.Paths(topo.Select("api.example.com"), topo.Select("api"), ReachOptions{})
	for _, p := range got {
		if last := p.Nodes[len(p.Nodes)-1].ID; last != "svc1" {
			t.Errorf("path ended at %s, want the target svc1", last)
		}
	}
}

// A cycle must not produce infinitely many routes that differ only by how many
// times they went round.
func TestPaths_TerminatesOnACycle(t *testing.T) {
	a := core.Asset{Provider: "p", Type: "t", ID: "a", Name: "a"}
	b := core.Asset{Provider: "p", Type: "t", ID: "b", Name: "b"}
	c := core.Asset{Provider: "p", Type: "t", ID: "c", Name: "c"}
	topo := &Topology{
		Nodes: []core.Asset{a, b, c},
		Edges: []core.Edge{
			{From: a.AsRef(), To: b.AsRef(), Kind: core.EdgeKindDNS, Confidence: core.ConfidenceExact},
			{From: b.AsRef(), To: a.AsRef(), Kind: core.EdgeKindDNS, Confidence: core.ConfidenceExact},
			{From: b.AsRef(), To: c.AsRef(), Kind: core.EdgeKindDNS, Confidence: core.ConfidenceExact},
		},
	}
	got := topo.Paths(topo.Select("a"), topo.Select("c"), ReachOptions{})
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 simple path a→c, got %d: %v", len(got), pathEnds(got))
	}
	if got[0].Hops() != 2 {
		t.Errorf("Hops() = %d, want 2", got[0].Hops())
	}
}

func TestPaths_ShortestFirst(t *testing.T) {
	a := core.Asset{Provider: "p", Type: "t", ID: "a", Name: "a"}
	mid := core.Asset{Provider: "p", Type: "t", ID: "m", Name: "m"}
	z := core.Asset{Provider: "p", Type: "t", ID: "z", Name: "z"}
	topo := &Topology{
		Nodes: []core.Asset{a, mid, z},
		Edges: []core.Edge{
			{From: a.AsRef(), To: z.AsRef(), Kind: core.EdgeKindDNS, Confidence: core.ConfidenceExact},
			{From: a.AsRef(), To: mid.AsRef(), Kind: core.EdgeKindDNS, Confidence: core.ConfidenceExact},
			{From: mid.AsRef(), To: z.AsRef(), Kind: core.EdgeKindDNS, Confidence: core.ConfidenceExact},
		},
	}
	got := topo.Paths(topo.Select("a"), topo.Select("z"), ReachOptions{})
	if len(got) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(got))
	}
	if got[0].Hops() > got[1].Hops() {
		t.Errorf("paths not shortest-first: %d then %d", got[0].Hops(), got[1].Hops())
	}
}

// Enumeration order must be stable, or the same question asked twice returns
// the same routes in a different order.
func TestPaths_Deterministic(t *testing.T) {
	topo := reachFixture()
	first := pathEnds(topo.Paths(topo.Select("api.example.com"), topo.Select("*"), ReachOptions{}))
	for i := range 20 {
		if got := pathEnds(topo.Paths(topo.Select("api.example.com"), topo.Select("*"), ReachOptions{})); strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d differed:\n got %v\nwant %v", i, got, first)
		}
	}
}

// --- exposure -------------------------------------------------------------

func TestEntryPoints_PicksPublicSurfaces(t *testing.T) {
	topo := reachFixture()
	got := topo.EntryPoints()
	ids := make([]string, len(got))
	for i, a := range got {
		ids[i] = a.ID
	}
	for _, want := range []string{"d1", "lb1"} {
		if !contains(ids, want) {
			t.Errorf("entry points should include %s; got %v", want, ids)
		}
	}
	// An internal Kubernetes Service with no published address is not an entry point.
	if contains(ids, "svc2") {
		t.Errorf("internal-only service must not be an entry point; got %v", ids)
	}
}

// A Service that a controller has actually published is exposed regardless of
// its type — that's stronger evidence than the declared spec.type.
func TestEntryPoints_PublishedKubeServiceCountsWhenRawPresent(t *testing.T) {
	published := core.Asset{
		Provider: "kubernetes", Type: "v1.Service", ID: "pub", Name: "public-svc",
		Raw: json.RawMessage(`{"status":{"loadBalancer":{"ingress":[{"ip":"203.0.113.5"}]}}}`),
	}
	unpublished := core.Asset{
		Provider: "kubernetes", Type: "v1.Service", ID: "priv", Name: "private-svc",
		Raw: json.RawMessage(`{"spec":{"type":"ClusterIP"}}`),
	}
	topo := &Topology{Nodes: []core.Asset{published, unpublished}}
	got := topo.EntryPoints()
	if len(got) != 1 || got[0].ID != "pub" {
		t.Errorf("EntryPoints() = %v, want just the published service", pathEnds([]Path{{Nodes: got}}))
	}
}

func TestExposed_ReportsTheChainButNotTheIsolatedService(t *testing.T) {
	topo := reachFixture()
	exp := topo.Exposed(ReachOptions{})

	reached := map[string]bool{}
	for _, a := range exp.Reached() {
		reached[a.ID] = true
	}
	for _, want := range []string{"svc1", "pod1", "db1"} {
		if !reached[want] {
			t.Errorf("%s should be reported internet-exposed; got %v", want, exp.Reached())
		}
	}
	// internal-only is upstream of the db, never downstream of an entry point.
	if reached["svc2"] {
		t.Error("internal-only service must not be reported internet-exposed")
	}
	if len(exp.Entries) == 0 {
		t.Error("Exposure should carry the entry points it started from")
	}
}

// --- rendering ------------------------------------------------------------

// "No path" from an inferred graph is much weaker than "not reachable"; the
// report must say so, or a reader treats it as proof of isolation.
func TestRenderReach_EmptyResultCarriesTheCaveat(t *testing.T) {
	var sb strings.Builder
	err := RenderReach(ReachResult{Question: "What can reach X?"}, "table", &sb)
	if err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "No paths found") {
		t.Errorf("missing the no-paths line:\n%s", out)
	}
	if !strings.Contains(out, "not proof of isolation") {
		t.Errorf("empty result must warn that absence of a path is not proof of isolation:\n%s", out)
	}
}

func TestRenderReach_TableShowsHopsAndHeuristicMarker(t *testing.T) {
	topo := reachFixture()
	res := ReachResult{
		Question: "q",
		Paths:    topo.Paths(topo.Select("api.example.com"), topo.Select("postgres-0"), ReachOptions{}),
	}
	var sb strings.Builder
	if err := RenderReach(res, "table", &sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{"api.example.com", "postgres-0", "5 hops", ":5432", "~"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
}

// A truncated security result read as "these are all the ways in" is the
// dangerous kind of wrong, so it must be stated.
func TestRenderReach_TruncationIsStated(t *testing.T) {
	topo := reachFixture()
	res := ReachResult{Question: "q", Paths: topo.Reachable(topo.Select("api.example.com"), Downstream, ReachOptions{}), Truncated: true}
	var sb strings.Builder
	if err := RenderReach(res, "table", &sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "truncated") {
		t.Errorf("truncation not reported:\n%s", sb.String())
	}
}

// Raw would otherwise put a full Pod spec on every hop of every path.
func TestRenderReach_JSONStripsRaw(t *testing.T) {
	res := ReachResult{
		Question: "q",
		Paths: []Path{{Nodes: []core.Asset{
			{Provider: "kubernetes", Type: "v1.Pod", ID: "p", Name: "p", Raw: json.RawMessage(`{"huge":"payload"}`)},
		}}},
	}
	var sb strings.Builder
	if err := RenderReach(res, "json", &sb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "huge") {
		t.Errorf("JSON output leaked Asset.Raw:\n%s", sb.String())
	}
}

// A traced route must export to the same formats as a full topology.
func TestRenderReach_GraphFormats(t *testing.T) {
	topo := reachFixture()
	res := ReachResult{
		Question: "q",
		Paths:    topo.Paths(topo.Select("api.example.com"), topo.Select("postgres-0"), ReachOptions{}),
	}
	for _, format := range []string{"dot", "mermaid", "d2", "graphml", "excalidraw", "drawio", "html"} {
		var sb strings.Builder
		if err := RenderReach(res, format, &sb); err != nil {
			t.Errorf("RenderReach(%s) = %v", format, err)
			continue
		}
		if sb.Len() == 0 {
			t.Errorf("RenderReach(%s) produced no output", format)
		}
	}
	if err := RenderReach(res, "nonsense", &strings.Builder{}); err == nil {
		t.Error("an unknown format must be an error, not a silent default")
	}
}

// The sub-topology is the union of the paths' nodes and edges, deduplicated —
// the two paths in the fixture share the d1→lb1→svc1 prefix.
func TestReachResult_TopologyDedupes(t *testing.T) {
	topo := reachFixture()
	res := ReachResult{Paths: topo.Reachable(topo.Select("api.example.com"), Downstream, ReachOptions{})}
	sub := res.Topology()

	seen := map[string]bool{}
	for _, n := range sub.Nodes {
		k := refKey(n.AsRef())
		if seen[k] {
			t.Errorf("duplicate node %s in the sub-topology", k)
		}
		seen[k] = true
	}
	if len(sub.Nodes) == 0 || len(sub.Edges) == 0 {
		t.Error("sub-topology should carry the traced nodes and edges")
	}
}

func TestSelect_GlobsOverIDAndName(t *testing.T) {
	topo := reachFixture()
	for _, tc := range []struct {
		selector string
		wantID   string
	}{
		{"api.example.com", "d1"}, // by name
		{"lb1", "lb1"},            // by id
		{"postgres*", "db1"},      // glob on name
		{"POD1", "pod1"},          // case-insensitive
	} {
		got := topo.Select(tc.selector)
		if len(got) == 0 {
			t.Errorf("Select(%q) matched nothing", tc.selector)
			continue
		}
		if !contains(idsOfAssets(got), tc.wantID) {
			t.Errorf("Select(%q) = %v, want it to include %s", tc.selector, idsOfAssets(got), tc.wantID)
		}
	}
	if got := topo.Select(""); got != nil {
		t.Errorf("Select(\"\") should match nothing, got %v", idsOfAssets(got))
	}
}

func idsOfAssets(as []core.Asset) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.ID
	}
	return out
}

// --- regressions ----------------------------------------------------------

// An upstream traversal walks edges backwards, so the node the walk arrives
// *from* is the edge's destination. Rendering those hops with a forward arrow
// stated the exact reverse of what the edge says.
func TestRenderReach_UpstreamArrowPointsTheWayTrafficFlows(t *testing.T) {
	topo := reachFixture()
	res := ReachResult{
		Question: "q",
		Paths:    topo.Reachable(topo.Select("api-abc"), Upstream, ReachOptions{}),
	}
	var sb strings.Builder
	if err := RenderReach(res, "table", &sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "<--[") {
		t.Errorf("upstream hops should render a reversed arrow:\n%s", out)
	}
	// The pod does not serve the Service; the Service selects the pod.
	if strings.Contains(out, "api-abc (v1.Pod) --[service-backend]--> api (v1.Service)") {
		t.Errorf("upstream hop drawn in walk direction, reversing the edge's meaning:\n%s", out)
	}
}

// Two A records sharing an address are siblings, not a resolution chain. The
// index buckets DNS records by their content IP, so a naive byIP lookup pairs
// every such record with every other — inventing a mutual edge (apex + www is
// the common case) and, with it, a cycle that defeats reachability analysis.
func TestDNSToTarget_SiblingARecordsAreNotLinked(t *testing.T) {
	apex := core.Asset{Provider: "cloudflare", Type: "cloudflare.dns_record", ID: "d1", Name: "example.com",
		Tags: map[string]string{"type": "A", "content": "203.0.113.10"}}
	www := core.Asset{Provider: "cloudflare", Type: "cloudflare.dns_record", ID: "d2", Name: "www.example.com",
		Tags: map[string]string{"type": "A", "content": "203.0.113.10"}}
	lb := core.Asset{Provider: "oci", Type: "oci.load_balancer", ID: "lb1", Name: "lb",
		Tags: map[string]string{"ip_addresses": "203.0.113.10"}}

	edges := dnsToTarget(newIndex([]core.Asset{apex, www, lb}))
	for _, e := range edges {
		if e.To.Type == "cloudflare.dns_record" {
			t.Errorf("A record %s linked to sibling record %s — records resolve to targets, not to each other",
				e.From.ID, e.To.ID)
		}
	}
	// The real target must survive the change.
	var toLB int
	for _, e := range edges {
		if e.To.ID == "lb1" {
			toLB++
		}
	}
	if toLB != 2 {
		t.Errorf("both records should still resolve to the load balancer; got %d edges", toLB)
	}
}

// A CNAME pointing at a name that has its own record IS a genuine resolution
// chain, so those record-to-record edges must be kept.
func TestDNSToTarget_CNAMEChainToAnotherRecordIsKept(t *testing.T) {
	target := core.Asset{Provider: "cloudflare", Type: "cloudflare.dns_record", ID: "d1", Name: "origin.example.com",
		Tags: map[string]string{"type": "A", "content": "203.0.113.10"}}
	alias := core.Asset{Provider: "cloudflare", Type: "cloudflare.dns_record", ID: "d2", Name: "www.example.com",
		Tags: map[string]string{"type": "CNAME", "content": "origin.example.com"}}

	edges := dnsToTarget(newIndex([]core.Asset{target, alias}))
	var found bool
	for _, e := range edges {
		if e.From.ID == "d2" && e.To.ID == "d1" {
			found = true
		}
	}
	if !found {
		t.Errorf("CNAME → the record it names is a real chain and must be kept; got %d edges", len(edges))
	}
}

// A published Kubernetes Service is an entry point by type, but it usually
// also sits behind an LB behind a DNS record. Seeding all three makes BFS mark
// the Service visited at depth 0, so the route to its pods starts mid-chain
// and the actual way in from the internet is never shown.
func TestExposed_PathsStartAtTheOutermostEntryPoint(t *testing.T) {
	topo := reachFixture()
	exp := topo.Exposed(ReachOptions{})
	if len(exp.Paths) == 0 {
		t.Fatal("expected exposure paths")
	}
	for _, p := range exp.Paths {
		if got := p.Nodes[0].Type; got != "cloudflare.dns_record" {
			t.Errorf("exposure path starts at %s (%s); want the outermost public surface, the DNS record",
				p.Nodes[0].ID, got)
		}
	}
	// The intermediate hops are still reported as entry points in their own right.
	var ids []string
	for _, e := range exp.Entries {
		ids = append(ids, e.ID)
	}
	if !contains(ids, "lb1") {
		t.Errorf("the load balancer is still internet-facing and belongs in Entries; got %v", ids)
	}
}

// Every entry point being in a cycle must not empty the report — a slightly
// redundant answer beats no answer for a security question.
func TestEntryRoots_CycleFallsBackToAllEntries(t *testing.T) {
	a := core.Asset{Provider: "oci", Type: "oci.load_balancer", ID: "lb1", Name: "a"}
	b := core.Asset{Provider: "oci", Type: "oci.load_balancer", ID: "lb2", Name: "b"}
	topo := &Topology{
		Nodes: []core.Asset{a, b},
		Edges: []core.Edge{
			{From: a.AsRef(), To: b.AsRef(), Kind: core.EdgeKindLBBackend, Confidence: core.ConfidenceExact},
			{From: b.AsRef(), To: a.AsRef(), Kind: core.EdgeKindLBBackend, Confidence: core.ConfidenceExact},
		},
	}
	if got := entryRoots(topo, topo.EntryPoints(), ReachOptions{}); len(got) != 2 {
		t.Errorf("a cycle among entry points should fall back to all of them, got %d", len(got))
	}
}

// MaxPaths must bound Reachable as well as Paths. It only bounded the latter
// at first, so an exposure query over a large estate came back complete but
// enormous — and was then labelled "truncated" by the caller purely for
// having hit the count, which made the flag lie.
func TestReachable_RespectsMaxPaths(t *testing.T) {
	topo := reachFixture()

	full := topo.Reachable(topo.Select("api.example.com"), Downstream, ReachOptions{})
	if len(full) < 3 {
		t.Fatalf("fixture should yield at least 3 reachable assets, got %d", len(full))
	}

	capped := topo.Reachable(topo.Select("api.example.com"), Downstream, ReachOptions{MaxPaths: 2})
	if len(capped) != 2 {
		t.Errorf("MaxPaths=2 returned %d paths: %v", len(capped), pathEnds(capped))
	}
	// Breadth-first, so a cap keeps the *nearest* assets, not an arbitrary
	// subset — the closest hops are the ones worth reporting first.
	for _, p := range capped {
		if p.Hops() > full[len(capped)-1].Hops()+1 {
			t.Errorf("capped result kept a far path (%d hops) over nearer ones", p.Hops())
		}
	}
}
