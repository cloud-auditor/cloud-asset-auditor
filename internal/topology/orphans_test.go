package topology

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// orphanFixture is a graph with one of each interesting case:
//
//	dns   → lb        connected pair (so cloudflare.dns_record and
//	                  oci.load_balancer are both observed as connectable)
//	dns2              a second DNS record with no target — the outlier the
//	                  report exists to surface
//	cm, cm2           v1.ConfigMap: no resolver relates it, ever
//	pod               v1.Pod: never connected here, but a declared resolver
//	                  input, so it must NOT be filed as unmodelled
func orphanFixture() *Topology {
	dns := core.Asset{Provider: "cloudflare", Type: "cloudflare.dns_record", ID: "d1", Name: "api.example.com"}
	dns2 := core.Asset{Provider: "cloudflare", Type: "cloudflare.dns_record", ID: "d2", Name: "old.example.com"}
	lb := core.Asset{Provider: "oci", Type: "oci.load_balancer", ID: "lb1", Name: "prod-lb"}
	cm := core.Asset{Provider: "kubernetes", Type: "v1.ConfigMap", ID: "c1", Name: "app-config"}
	cm2 := core.Asset{Provider: "kubernetes", Type: "v1.ConfigMap", ID: "c2", Name: "tls-config"}
	pod := core.Asset{Provider: "kubernetes", Type: "v1.Pod", ID: "p1", Name: "api-pod"}

	return &Topology{
		Nodes: []core.Asset{dns, dns2, lb, cm, cm2, pod},
		Edges: []core.Edge{
			{From: dns.AsRef(), To: lb.AsRef(), Kind: core.EdgeKindDNS, Confidence: core.ConfidenceHeuristic},
		},
	}
}

// The orphan set must be exactly the complement of what DropOrphans keeps.
// They are two views of one fact, and the day they disagree the report is
// naming assets that the rendered diagram is happily drawing.
func TestOrphans_AreTheExactComplementOfDropOrphans(t *testing.T) {
	topo := orphanFixture()
	kept := map[string]bool{}
	for _, n := range topo.DropOrphans().Nodes {
		kept[n.ID] = true
	}

	report := topo.Orphans("provider")
	if got, want := report.TotalOrphans, len(topo.Nodes)-len(kept); got != want {
		t.Fatalf("TotalOrphans = %d, want %d", got, want)
	}
	if report.TotalNodes != len(topo.Nodes) {
		t.Errorf("TotalNodes = %d, want %d", report.TotalNodes, len(topo.Nodes))
	}

	for _, g := range append(report.Unconnected, report.Unmodelled...) {
		for _, name := range g.Examples {
			for _, n := range topo.Nodes {
				if n.Name == name && kept[n.ID] {
					t.Errorf("%q is reported as an orphan but DropOrphans kept it", name)
				}
			}
		}
	}
}

// The split is the whole point: a type nothing models is noise, a type the
// graph does relate is a lead. Filing them together buries the lead.
func TestOrphans_SeparatesUnmodelledTypesFromUnconnectedOnes(t *testing.T) {
	report := orphanFixture().Orphans("provider")

	unconnected := map[string]int{}
	for _, g := range report.Unconnected {
		unconnected[g.Type] = g.Count
	}
	unmodelled := map[string]int{}
	for _, g := range report.Unmodelled {
		unmodelled[g.Type] = g.Count
	}

	// Observed signal: a sibling cloudflare.dns_record has an edge in this very
	// graph, so the unconnected one is a genuine outlier.
	if unconnected["cloudflare.dns_record"] != 1 {
		t.Errorf("the DNS record with no target must be reported as connectable-but-unconnected, got %v", report.Unconnected)
	}
	// Declared signal: no v1.Pod is connected anywhere in this graph, so only
	// the resolver-input table can rescue it from the noise bucket.
	if unconnected["v1.Pod"] != 1 {
		t.Errorf("v1.Pod is a declared resolver input and must not be filed as unmodelled, got %v", report.Unmodelled)
	}
	if unmodelled["v1.ConfigMap"] != 2 {
		t.Errorf("v1.ConfigMap has no resolver and must be filed as unmodelled, got %v", report.Unmodelled)
	}
	if _, ok := unconnected["v1.ConfigMap"]; ok {
		t.Error("v1.ConfigMap must not appear in the connectable section")
	}

	if got := report.UnconnectedCount() + report.UnmodelledCount(); got != report.TotalOrphans {
		t.Errorf("the two sections sum to %d but TotalOrphans is %d — the totals must reconcile", got, report.TotalOrphans)
	}
}

func TestOrphans_GroupsAndSortsByCount(t *testing.T) {
	var nodes []core.Asset
	for i := range 5 {
		nodes = append(nodes, core.Asset{
			Provider: "kubernetes", Type: "v1.ConfigMap",
			ID: "c" + strconv.Itoa(i), Name: "cm" + strconv.Itoa(i),
		})
	}
	nodes = append(nodes,
		core.Asset{Provider: "kubernetes", Type: "v1.Secret", ID: "s1", Name: "sec"},
		core.Asset{Provider: "oci", Type: "oci.bucket", ID: "b1", Name: "bucket"},
	)

	report := (&Topology{Nodes: nodes}).Orphans("provider")
	if len(report.Unmodelled) != 3 {
		t.Fatalf("want 3 (provider,type) buckets, got %d: %v", len(report.Unmodelled), report.Unmodelled)
	}
	if report.Unmodelled[0].Type != "v1.ConfigMap" || report.Unmodelled[0].Count != 5 {
		t.Errorf("biggest bucket must sort first, got %+v", report.Unmodelled[0])
	}
	if len(report.Unmodelled[0].Examples) != maxExamples {
		t.Errorf("examples must be capped at %d, got %v", maxExamples, report.Unmodelled[0].Examples)
	}
	// Ties break deterministically on group then type.
	if report.Unmodelled[1].Group != "kubernetes" || report.Unmodelled[2].Group != "oci" {
		t.Errorf("tied buckets must break on group then type, got %+v", report.Unmodelled[1:])
	}
}

func TestOrphans_GroupByDimension(t *testing.T) {
	nodes := []core.Asset{
		{Provider: "oci", Region: "eu-frankfurt-1", AccountID: "tenancy", Type: "oci.bucket", ID: "b1", Name: "b1"},
		{Provider: "oci", Region: "uk-london-1", AccountID: "tenancy", Type: "oci.bucket", ID: "b2", Name: "b2"},
	}
	topo := &Topology{Nodes: nodes}

	if got := topo.Orphans("region"); len(got.Unmodelled) != 2 || got.GroupBy != "region" {
		t.Errorf("--group-by region must split the two buckets, got %+v", got.Unmodelled)
	}
	if got := topo.Orphans("account"); len(got.Unmodelled) != 1 || got.GroupBy != "account" {
		t.Errorf("--group-by account must merge them, got %+v", got.Unmodelled)
	}
	// An unusable dimension falls back to the one every asset always has.
	if got := topo.Orphans("nonsense"); got.GroupBy != "provider" {
		t.Errorf("unknown dimension must fall back to provider, got %q", got.GroupBy)
	}
}

// ----------------------------------------------------------------------
// the caveat — the part of this feature that can do harm if it goes missing
// ----------------------------------------------------------------------

// Every report carries the caveat, including an empty one. A count shipped
// without it reads as a delete list.
func TestOrphans_AlwaysCarriesTheCaveat(t *testing.T) {
	for name, topo := range map[string]*Topology{
		"populated": orphanFixture(),
		"empty":     {},
	} {
		t.Run(name, func(t *testing.T) {
			caveat := strings.ToLower(strings.Join(topo.Orphans("provider").Caveat, " "))
			for _, want := range []string{
				"degree 0",
				"not mean",
				"safe to delete",
				"include-raw",
				"outage",
			} {
				if !strings.Contains(caveat, want) {
					t.Errorf("caveat must say %q; got:\n%s", want, caveat)
				}
			}
		})
	}
}

// A snapshot with no Raw silently disables three resolvers. Reporting the
// resulting Kubernetes orphans without saying so would blame the cluster for
// the collection flag.
func TestOrphanCaveat_NamesAMissingRawSnapshot(t *testing.T) {
	withRaw := &Topology{Nodes: []core.Asset{
		{Provider: "kubernetes", Type: "v1.Pod", ID: "p1", Raw: json.RawMessage(`{}`)},
		{Provider: "oci", Type: "oci.bucket", ID: "b1"},
	}}
	if joined := strings.Join(withRaw.Orphans("provider").Caveat, " "); strings.Contains(joined, "no Asset.Raw payloads") {
		t.Error("a graph that does carry Raw must not warn about missing Raw")
	}

	without := &Topology{Nodes: []core.Asset{
		{Provider: "kubernetes", Type: "v1.Pod", ID: "p1"},
		{Provider: "oci", Type: "oci.bucket", ID: "b1"},
	}}
	if joined := strings.Join(without.Orphans("provider").Caveat, " "); !strings.Contains(joined, "no Asset.Raw payloads") {
		t.Errorf("a Raw-less graph must say the raw-reading resolvers could not run; got:\n%s", joined)
	}
}

// The graph-wide RawAvailable flag is not enough on its own: every raw-reading
// resolver is Kubernetes-specific, so a snapshot that carries payloads for the
// other providers but not for Kubernetes disconnects every Service, Ingress,
// HTTPRoute and NetworkPolicy while RawAvailable stays true. That combination
// is where an orphan listing is at its most confidently wrong — plainly
// connected workloads land under "the lines to look at" — so it must warn.
func TestOrphanCaveat_NamesRawStarvedTypesWhenOtherProvidersHaveRaw(t *testing.T) {
	mixed := &Topology{Nodes: []core.Asset{
		// Payload present, just not on the types that need it.
		{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1", Raw: json.RawMessage(`{"a":1}`)},
		{Provider: "kubernetes", Type: "v1.Service", ID: "s1"},
		{Provider: "kubernetes", Type: "networking.k8s.io/v1.Ingress", ID: "i1"},
		{Provider: "kubernetes", Type: "v1.Pod", ID: "p1"},
	}}

	report := mixed.Orphans("provider")
	if !report.RawAvailable {
		t.Fatal("precondition: this graph does carry Raw, just not on the Kubernetes types")
	}
	if got, want := report.RawMissingFor, []string{"networking.k8s.io/v1.Ingress", "v1.Service"}; !slices.Equal(got, want) {
		t.Errorf("RawMissingFor = %v, want %v (sorted, and v1.Pod excluded — Pods are matched by Tags)", got, want)
	}

	joined := strings.Join(report.Caveat, " ")
	if !strings.Contains(joined, "not one node of these types does") {
		t.Errorf("a graph whose raw-dependent types have no payload must say so; got:\n%s", joined)
	}
	for _, typ := range report.RawMissingFor {
		if !strings.Contains(joined, typ) {
			t.Errorf("caveat must name the starved type %q so the reader knows which resolver died; got:\n%s", typ, joined)
		}
	}
	// The largest bucket in a real starved snapshot is Pods, which are orphaned
	// only as a knock-on. Naming that link is the difference between the report
	// being read as a symptom and being read as a finding.
	if !strings.Contains(joined, "v1.Pod count below is a symptom") {
		t.Errorf("caveat must explain the Pod knock-on from an unreadable Service selector; got:\n%s", joined)
	}
}

// A type that HAS its payload must never be reported as starved, or the warning
// becomes noise that gets tuned out on healthy snapshots.
func TestOrphanCaveat_NoRawWarningWhenTheRawDependentTypesCarryPayloads(t *testing.T) {
	healthy := &Topology{Nodes: []core.Asset{
		{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1", Raw: json.RawMessage(`{"a":1}`)},
		{Provider: "kubernetes", Type: "v1.Service", ID: "s1", Raw: json.RawMessage(`{"spec":{}}`)},
		{Provider: "kubernetes", Type: "v1.Pod", ID: "p1"},
	}}

	report := healthy.Orphans("provider")
	if len(report.RawMissingFor) != 0 {
		t.Errorf("RawMissingFor = %v, want none", report.RawMissingFor)
	}
	if joined := strings.Join(report.Caveat, " "); strings.Contains(joined, "not one node of these types does") {
		t.Errorf("a graph whose raw-dependent types carry payloads must not warn about them; got:\n%s", joined)
	}
}

// One provider means every cross-provider resolver had nothing to join to,
// which is the single commonest explanation for an alarming orphan count.
func TestOrphanCaveat_NamesASingleProviderGraph(t *testing.T) {
	single := &Topology{Nodes: []core.Asset{{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1"}}}
	if joined := strings.Join(single.Orphans("provider").Caveat, " "); !strings.Contains(joined, "Only one provider (cloudflare)") {
		t.Errorf("a single-provider graph must say so; got:\n%s", joined)
	}
	if joined := strings.Join(orphanFixture().Orphans("provider").Caveat, " "); strings.Contains(joined, "Only one provider") {
		t.Error("a multi-provider graph must not claim to have one")
	}
}

// ----------------------------------------------------------------------
// rendering
// ----------------------------------------------------------------------

// The caveat must precede the numbers. Printed underneath them it reads as a
// footnote, and a footnote is exactly the weight this warning must not have.
func TestRenderOrphans_TableLeadsWithTheCaveat(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderOrphans(orphanFixture().Orphans("provider"), "table", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	caveatAt := strings.Index(out, "safe to delete")
	countsAt := strings.Index(out, "CONNECTABLE, BUT NOT CONNECTED HERE")
	if caveatAt < 0 || countsAt < 0 {
		t.Fatalf("table is missing the caveat or the sections:\n%s", out)
	}
	if caveatAt > countsAt {
		t.Errorf("the caveat must come before the counts, not after:\n%s", out)
	}
	for _, want := range []string{"old.example.com", "v1.ConfigMap", "NO RESOLVER RELATES THESE TYPES"} {
		if !strings.Contains(out, want) {
			t.Errorf("table should mention %q:\n%s", want, out)
		}
	}
	// Prose wrapped to a readable width, not one enormous line.
	for _, line := range strings.Split(out, "\n") {
		if len(line) > 100 {
			t.Errorf("line is %d columns, wrap it: %q", len(line), line)
		}
	}
}

// An empty report still says what it would have meant — "no orphans" is the
// most tempting result to over-read.
func TestRenderOrphans_EmptyReportStillExplainsItself(t *testing.T) {
	var buf bytes.Buffer
	topo := &Topology{
		Nodes: []core.Asset{{Provider: "oci", Type: "oci.vcn", ID: "v1", Name: "vcn"}, {Provider: "oci", Type: "oci.subnet", ID: "s1", Name: "sub"}},
	}
	topo.Edges = []core.Edge{{From: topo.Nodes[1].AsRef(), To: topo.Nodes[0].AsRef(), Kind: core.EdgeKindNetworkContainment}}

	if err := RenderOrphans(topo.Orphans("provider"), "", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Every node in this graph has at least one edge.") {
		t.Errorf("an empty report must say so plainly:\n%s", out)
	}
	if !strings.Contains(out, "safe to delete") {
		t.Errorf("an empty report still carries the caveat:\n%s", out)
	}
}

// The JSON is the machine-readable form, so it must carry the caveat too — a
// saved file read six months later has no terminal output to fall back on.
func TestRenderOrphans_JSONIsSelfDescribing(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderOrphans(orphanFixture().Orphans("provider"), "json", &buf); err != nil {
		t.Fatal(err)
	}
	var got OrphanReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(got.Caveat) < 3 {
		t.Errorf("JSON must carry the caveat, got %v", got.Caveat)
	}
	if got.TotalOrphans != 4 || got.GroupBy != "provider" {
		t.Errorf("round-trip lost fields: %+v", got)
	}
	if len(got.Unconnected) == 0 || len(got.Unmodelled) == 0 {
		t.Errorf("round-trip lost a section: %+v", got)
	}
}

// A graph format is not merely unsupported here, it is incoherent: the subject
// is the nodes with no edges. Say that rather than drawing loose boxes.
func TestRenderOrphans_RejectsGraphFormats(t *testing.T) {
	err := RenderOrphans(orphanFixture().Orphans("provider"), "dot", &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected an error for -o dot")
	}
	if !strings.Contains(err.Error(), "table|json") || !strings.Contains(err.Error(), "no edges") {
		t.Errorf("error should name the valid formats and why graphs do not apply: %v", err)
	}
}

func TestRenderOrphans_IsDeterministic(t *testing.T) {
	topo := orphanFixture()
	var a, b bytes.Buffer
	if err := RenderOrphans(topo.Orphans("provider"), "table", &a); err != nil {
		t.Fatal(err)
	}
	if err := RenderOrphans(topo.Orphans("provider"), "table", &b); err != nil {
		t.Fatal(err)
	}
	if a.String() != b.String() {
		t.Error("two renders of the same topology must be byte-identical")
	}
}

// ----------------------------------------------------------------------
// staleness guard for the declared resolver-input table
// ----------------------------------------------------------------------

// The classification's "declared" signal is a table, and a table drifts. This
// reads the package's own resolver source and fails when a resolver names an
// asset type the table has not been told about — the failure mode being that a
// newly-modelled type keeps getting filed under "no resolver relates this",
// silently, forever.
//
// It checks one direction only. Several table entries are join *targets*
// reached through idx.byID (cloudflare.zone, oci.vcn, netbird.group) and so
// appear as no literal anywhere; a reverse check would demand their deletion.
func TestResolverInputTypes_CoversEveryTypeTheResolversName(t *testing.T) {
	declared := resolverInputTypes()

	found := map[string]string{} // type → the file it was found in
	for _, file := range []string{"resolvers.go", "traffic.go", "index.go"} {
		for _, typ := range assetTypeLiterals(t, file) {
			found[typ] = file
		}
	}
	if len(found) < 10 {
		t.Fatalf("the source scan found only %d type literals — the scan itself has broken, "+
			"which would make this guard silently pass forever", len(found))
	}

	for typ, file := range found {
		if _, ok := declared[typ]; !ok {
			t.Errorf("%s names asset type %q but declaredResolverTypes does not list it. "+
				"Add it, or orphans of that type will be reported as structurally unconnectable forever.", file, typ)
		}
	}
}

// assetTypeLiterals extracts the string literals a resolver file uses as an
// asset type. Four syntactic shapes cover every resolver written so far:
//
//	idx.byType["v1.Pod"]                 a type bucket lookup
//	switch a.Type { case "...": }        a type dispatch
//	a.Type == "v1.Service"               a type guard
//	byNameOfType(idx, "tailscale.…", …)  the Tailscale selector helper
//
// Shapes it deliberately does not chase — a type held in a slice or struct
// table (wafBinding's candidates, ociContainmentRules) — are covered instead
// by reading the table at runtime or by an explicit table entry. A resolver
// that invents a fifth shape simply is not seen, which is why the caller also
// asserts the scan still finds a plausible number of literals.
func assetTypeLiterals(t *testing.T, file string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var out []string
	lit := func(e ast.Expr) {
		if b, ok := e.(*ast.BasicLit); ok && b.Kind == token.STRING {
			if s, err := strconv.Unquote(b.Value); err == nil && s != "" {
				out = append(out, s)
			}
		}
	}
	isSel := func(e ast.Expr, name string) bool {
		s, ok := e.(*ast.SelectorExpr)
		return ok && s.Sel.Name == name
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.IndexExpr:
			if isSel(node.X, "byType") {
				lit(node.Index)
			}
		case *ast.SwitchStmt:
			if !isSel(node.Tag, "Type") {
				return true
			}
			for _, stmt := range node.Body.List {
				clause, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, e := range clause.List {
					lit(e)
				}
			}
		case *ast.BinaryExpr:
			if isSel(node.X, "Type") {
				lit(node.Y)
			}
			if isSel(node.Y, "Type") {
				lit(node.X)
			}
		case *ast.CallExpr:
			id, ok := node.Fun.(*ast.Ident)
			if ok && id.Name == "byNameOfType" && len(node.Args) > 1 {
				lit(node.Args[1])
			}
		}
		return true
	})
	return out
}
