package insight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/cost"
)

// The test that matters most in this file is
// TestRegisteredInsights_ProduceValidFindings: it runs every insight anyone
// registers over a synthetic inventory and fails on the first finding that
// does not carry a caveat. Everything else defends the machinery that makes
// that check meaningful — the ordering guarantee, the refusal path, and the
// renderers that have to put the caveat next to the number.

// ----------------------------------------------------------------------
// fixtures
// ----------------------------------------------------------------------

var fixedNow = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

// fixtureAssets is the canonical cross-cloud chain the topology package's own
// tests use (Cloudflare DNS → OCI LB → Kubernetes Service + Ingress), plus a
// pod so there is something with labels and a payload to read. Shared so an
// insight's test can start from an inventory that actually produces edges.
func fixtureAssets() []core.Asset {
	return []core.Asset{
		{
			Provider: "cloudflare", AccountID: "acct-cf",
			Type: "cloudflare.zone", ID: "z1", Name: "example.com",
		},
		{
			Provider: "cloudflare", AccountID: "acct-cf",
			Type: "cloudflare.dns_record", ID: "rec1", Name: "api.example.com",
			Tags: map[string]string{
				"type": "A", "content": "203.0.113.10", "zone_id": "z1", "zone_name": "example.com",
			},
		},
		{
			Provider: "oci", AccountID: "ocid1.tenancy..t", Region: "us-ashburn-1",
			Type: "oci.load_balancer", ID: "ocid1.lb..lb1", Name: "public-lb",
			Tags: map[string]string{"ip_addresses": "203.0.113.10,203.0.113.11"},
		},
		{
			Provider: "kubernetes", AccountID: "prod-cluster",
			Type: "v1.Service", ID: "uid-svc-1", Name: "api-svc",
			Tags: map[string]string{"namespace": "prod"},
			Raw: json.RawMessage(`{
				"spec": {"type": "LoadBalancer", "selector": {"app": "api"}},
				"status": {"loadBalancer": {"ingress": [{"ip": "203.0.113.10"}]}}
			}`),
		},
		{
			Provider: "kubernetes", AccountID: "prod-cluster",
			Type: "v1.Pod", ID: "uid-pod-1", Name: "api-7c9f",
			Tags: map[string]string{"namespace": "prod", "app": "api"},
			Raw: json.RawMessage(`{
				"spec": {"nodeName": "node-1", "containers": [{"name": "api", "image": "api:1.2.3"}]}
			}`),
		},
	}
}

// fixtureInput builds the shared Input every insight test can start from. It
// is deliberately available to the whole package (not just this file) so the
// insights that land alongside this framework test against one inventory.
func fixtureInput(tb testing.TB, opts ...InputOption) *Input {
	tb.Helper()
	return NewInput(fixtureAssets(), append([]InputOption{WithNow(fixedNow)}, opts...)...)
}

// stubEstimator prices one type and leaves everything else unknown, which is
// the realistic shape: a price book never covers a whole estate.
type stubEstimator struct {
	monthly  map[string]float64
	measured bool
}

func (s stubEstimator) Estimate(a core.Asset) cost.Estimate {
	amount, ok := s.monthly[a.Type]
	if !ok {
		return cost.Estimate{Basis: cost.BasisUnknown, Detail: "no rule for " + a.Type}
	}
	basis := cost.BasisInferred
	if s.measured {
		basis = cost.BasisMeasured
	}
	return cost.Estimate{Monthly: amount, Priced: true, Currency: "USD", Basis: basis}
}

// fakeInsight is a scripted insight. Tests that need Requirements wrap it in
// requiringInsight — Go has no conditional interface satisfaction, and the
// optional-interface pattern is exactly what is being tested.
type fakeInsight struct {
	id       string
	title    string
	family   Family
	findings []Finding
	ran      *int
}

func (f fakeInsight) ID() string     { return f.id }
func (f fakeInsight) Title() string  { return f.title }
func (f fakeInsight) Family() Family { return f.family }

func (f fakeInsight) Run(context.Context, *Input) []Finding {
	if f.ran != nil {
		*f.ran++
	}
	return f.findings
}

type requiringInsight struct {
	fakeInsight
	req Requirements
}

func (r requiringInsight) Requires() Requirements { return r.req }

// goodFinding is a finding that meets the contract, for tests about something
// other than validation.
func goodFinding(id string, sev Severity) Finding {
	return Finding{
		ID:       id,
		Title:    "Public endpoints with no policy in front of them",
		Summary:  "2 assets answer on a public address that no collected policy covers.",
		Severity: sev,
		Count:    2,
		Basis:    "the topology graph's entry points, joined to traffic-allow edges by asset id",
		Caveat:   "an inventory records that an address is public, not that anything connects to it",
		Rows: []Row{
			AssetRow(fixtureAssets()[2], "public IP 203.0.113.10, no covering policy"),
		},
	}
}

func newFake(id string, family Family, findings ...Finding) fakeInsight {
	return fakeInsight{id: id, title: "Fake " + id, family: family, findings: findings}
}

// ----------------------------------------------------------------------
// the house rule
// ----------------------------------------------------------------------

// TestRegisteredInsights_ProduceValidFindings is the enforcement point for the
// rule this package exists to keep: every finding names what it cannot know.
//
// It runs the real registry over a synthetic inventory, so an insight that
// ships a bare finding fails CI rather than shipping an accusation.
func TestRegisteredInsights_ProduceValidFindings(t *testing.T) {
	registered := Registered()
	if len(registered) == 0 {
		// Not a failure: this framework is committed before the insights that
		// use it. The subtest below is what keeps the check honest in the
		// meantime — it proves this test can fail.
		t.Log("no insights registered yet; the teeth of this test are asserted below")
	}

	in := fixtureInput(t, WithEstimator(stubEstimator{monthly: map[string]float64{"oci.load_balancer": 18}}))
	for _, ins := range registered {
		t.Run(ins.ID(), func(t *testing.T) {
			if err := Validate(ins); err != nil {
				t.Fatalf("registered insight is invalid: %v", err)
			}
			for i, f := range ins.Run(context.Background(), in) {
				f.Family = ins.Family()
				if err := ValidateFinding(f); err != nil {
					t.Errorf("finding %d (%q) does not meet the contract: %v", i, f.ID, err)
				}
			}
		})
	}

	t.Run("the check has teeth", func(t *testing.T) {
		bare := goodFinding("exposure.bare", SeverityWarn)
		bare.Caveat = ""
		if err := ValidateFinding(bare); !errors.Is(err, ErrNoCaveat) {
			t.Fatalf("a finding with no caveat must be rejected with ErrNoCaveat, got %v", err)
		}
	})
}

func TestValidateFinding_CaveatIsMandatory(t *testing.T) {
	// Each of these is a way of writing nothing. The empty string is the
	// accident; the rest are the stub somebody left in.
	for _, caveat := range []string{"", "   ", "n/a", "N/A.", "none", "TBD", "unknown", "-", "approximate"} {
		f := goodFinding("hygiene.x", SeverityInfo)
		f.Caveat = caveat
		err := ValidateFinding(f)
		if !errors.Is(err, ErrNoCaveat) {
			t.Errorf("caveat %q: want ErrNoCaveat, got %v", caveat, err)
		}
	}

	f := goodFinding("hygiene.x", SeverityInfo)
	f.Caveat = "an inventory cannot see whether this bucket is public on purpose"
	if err := ValidateFinding(f); err != nil {
		t.Errorf("a real caveat was rejected: %v", err)
	}
}

func TestValidateFinding(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Finding)
		wantErr bool
	}{
		{"valid", func(*Finding) {}, false},
		{"no id", func(f *Finding) { f.ID = "" }, true},
		{"id is not a slug", func(f *Finding) { f.ID = "Network.PublicEndpoints" }, true},
		{"id with underscore", func(f *Finding) { f.ID = "network_public" }, true},
		{"no title", func(f *Finding) { f.Title = " " }, true},
		{"no summary", func(f *Finding) { f.Summary = "" }, true},
		{"multi-line summary", func(f *Finding) { f.Summary = "two\nlines" }, true},
		{"bad severity", func(f *Finding) { f.Severity = "critical" }, true},
		{"negative count", func(f *Finding) { f.Count = -1 }, true},
		{"zero count is allowed", func(f *Finding) { f.Count = 0 }, false},
		{"no basis", func(f *Finding) { f.Basis = "" }, true},
		{"row without label", func(f *Finding) { f.Rows = []Row{{Fact: "x"}} }, true},
		{"row asset without id", func(f *Finding) {
			f.Rows = []Row{{Label: "x", Asset: &core.AssetRef{Provider: "oci"}}}
		}, true},
		{"aggregate row without asset is fine", func(f *Finding) {
			f.Rows = []Row{{Label: "namespace prod", Fact: "no NetworkPolicy"}}
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := goodFinding("exposure.public-endpoints", SeverityWarn)
			tc.mutate(&f)
			err := ValidateFinding(f)
			if tc.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestRun_RefusesAFindingWithoutACaveat asserts the runtime half of the
// enforcement: the finding does not reach the report, and the refusal is
// reported loudly instead of swallowed.
func TestRun_RefusesAFindingWithoutACaveat(t *testing.T) {
	bare := goodFinding("exposure.bare", SeverityRisk)
	bare.Caveat = ""
	ok := goodFinding("exposure.ok", SeverityWarn)

	rep := Run(context.Background(), fixtureInput(t), Options{
		Insights: []Insight{newFake("exposure.fake", FamilyExposure, bare, ok)},
	})

	if len(rep.Findings) != 1 || rep.Findings[0].ID != "exposure.ok" {
		t.Fatalf("the bare finding was published: %+v", rep.Findings)
	}
	if len(rep.Suppressed) != 1 {
		t.Fatalf("want 1 suppressed finding, got %d", len(rep.Suppressed))
	}
	if s := rep.Suppressed[0]; s.Insight != "exposure.fake" || !strings.Contains(s.Reason, "caveat") {
		t.Errorf("suppression does not name the insight and the reason: %+v", s)
	}

	var buf bytes.Buffer
	if err := Render(rep, "table", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "REFUSED") {
		t.Error("the table renderer hides a refused finding; it must be as loud as a finding")
	}
}

// ----------------------------------------------------------------------
// determinism and ordering
// ----------------------------------------------------------------------

func TestRun_OrdersByFamilyThenID(t *testing.T) {
	rep := Run(context.Background(), fixtureInput(t), Options{
		Insights: []Insight{
			// Registered in the wrong order on purpose, including a family that
			// is not in familyOrder (it must sort last, not randomly).
			newFake("hygiene.untagged", FamilyHygiene, goodFinding("hygiene.untagged", SeverityInfo)),
			newFake("zzz.custom", "custom", goodFinding("custom.thing", SeverityInfo)),
			newFake("exposure.b", FamilyExposure, goodFinding("exposure.b", SeverityInfo)),
			newFake("exposure.a", FamilyExposure, goodFinding("exposure.a", SeverityRisk)),
			newFake("cost.idle", FamilyCost, goodFinding("cost.idle", SeverityNotable)),
		},
	})

	var got []string
	for _, f := range rep.Findings {
		got = append(got, string(f.Family)+"/"+f.ID)
	}
	want := []string{
		"exposure/exposure.a",
		"exposure/exposure.b",
		"cost/cost.idle",
		"hygiene/hygiene.untagged",
		"custom/custom.thing",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("report order:\n got %v\nwant %v", got, want)
	}
}

// TestRun_IsDeterministic is the guard against an insight sorting one of the
// Input's shared slices in place, which would make one insight's output depend
// on whether another ran first.
func TestRun_IsDeterministic(t *testing.T) {
	insights := []Insight{
		newFake("exposure.a", FamilyExposure, goodFinding("exposure.a", SeverityRisk)),
		newFake("cost.idle", FamilyCost, goodFinding("cost.idle", SeverityNotable)),
	}
	render := func() string {
		rep := Run(context.Background(), fixtureInput(t), Options{Insights: insights})
		var buf bytes.Buffer
		if err := Render(rep, "table", &buf); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if a, b := render(), render(); a != b {
		t.Error("two runs over the same inventory produced different output")
	}
}

func TestRegistry_RegisterAndOrder(t *testing.T) {
	// These stay in the global registry for the rest of the package's tests,
	// so they are well-formed on purpose: the everything-is-valid test above
	// runs over them too.
	a := newFake("test-registry.b", FamilyHygiene, goodFinding("test-registry.b", SeverityInfo))
	b := newFake("test-registry.a", FamilyHygiene, goodFinding("test-registry.a", SeverityInfo))
	Register(a)
	Register(b)

	if _, ok := Lookup("test-registry.a"); !ok {
		t.Fatal("Lookup did not find a registered insight")
	}
	var seen []string
	for _, i := range Registered() {
		if strings.HasPrefix(i.ID(), "test-registry.") {
			seen = append(seen, i.ID())
		}
	}
	if strings.Join(seen, ",") != "test-registry.a,test-registry.b" {
		t.Errorf("Registered() is not sorted by id: %v", seen)
	}

	assertPanics(t, "duplicate id", func() { Register(a) })
	assertPanics(t, "invalid id", func() { Register(newFake("Not A Slug", FamilyHygiene)) })
	assertPanics(t, "no family", func() { Register(newFake("test-registry.c", "")) })
}

func assertPanics(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: want panic, got none", what)
		}
	}()
	fn()
}

// ----------------------------------------------------------------------
// skipping, filtering, cancellation
// ----------------------------------------------------------------------

// TestRun_SkipsRatherThanFallsSilent is the other half of the house rule: an
// insight that could not look must not be indistinguishable from one that
// looked and found nothing.
func TestRun_SkipsRatherThanFallsSilent(t *testing.T) {
	ran := 0
	insights := []Insight{
		requiringInsight{
			fakeInsight: fakeInsight{id: "exposure.needs-cost", title: "t", family: FamilyExposure, ran: &ran},
			req:         Requirements{Cost: true},
		},
		requiringInsight{
			fakeInsight: fakeInsight{id: "exposure.needs-aws", title: "t", family: FamilyExposure, ran: &ran},
			req:         Requirements{Providers: []string{"gcp"}},
		},
		requiringInsight{
			fakeInsight: fakeInsight{id: "exposure.needs-type", title: "t", family: FamilyExposure, ran: &ran},
			req:         Requirements{Types: []string{"tailscale.device"}},
		},
		requiringInsight{
			fakeInsight: fakeInsight{id: "exposure.needs-raw", title: "t", family: FamilyExposure, ran: &ran},
			req:         Requirements{Raw: true, Topology: true, Types: []string{"v1.Service"}},
		},
	}

	rep := Run(context.Background(), fixtureInput(t), Options{Insights: insights})
	if len(rep.Skipped) != 3 {
		t.Fatalf("want 3 skipped insights, got %d: %+v", len(rep.Skipped), rep.Skipped)
	}
	if ran != 1 {
		t.Errorf("want only the satisfiable insight to run, ran %d", ran)
	}
	for _, s := range rep.Skipped {
		if s.Reason == "" {
			t.Errorf("%s was skipped without a reason", s.Insight)
		}
	}

	var buf bytes.Buffer
	if err := Render(rep, "table", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "NOT RUN") {
		t.Error("skipped insights are invisible in the table; an absent section must be explained")
	}
}

func TestRun_RequirementsMetWhenInputHasEverything(t *testing.T) {
	in := fixtureInput(t, WithEstimator(stubEstimator{monthly: map[string]float64{"v1.Pod": 3}}))
	ran := 0
	rep := Run(context.Background(), in, Options{Insights: []Insight{
		requiringInsight{
			fakeInsight: fakeInsight{id: "cost.x", title: "t", family: FamilyCost, ran: &ran},
			req:         Requirements{Raw: true, Topology: true, Cost: true, Providers: []string{"oci"}, Types: []string{"v1.Pod"}},
		},
	}})
	if ran != 1 || len(rep.Skipped) != 0 {
		t.Fatalf("a fully satisfied insight was skipped: %+v", rep.Skipped)
	}
}

func TestRun_OnlySelectsByIDAndFamily(t *testing.T) {
	insights := []Insight{
		newFake("exposure.a", FamilyExposure, goodFinding("exposure.a", SeverityInfo)),
		newFake("cost.idle", FamilyCost, goodFinding("cost.idle", SeverityInfo)),
		newFake("hygiene.tags", FamilyHygiene, goodFinding("hygiene.tags", SeverityInfo)),
	}
	for _, tc := range []struct {
		only []string
		want int
	}{
		{nil, 3},
		{[]string{"cost"}, 1},         // by family
		{[]string{"exposure.a"}, 1},   // by exact id
		{[]string{"*.idle"}, 1},       // by glob
		{[]string{"cost", "*.a"}, 2},  // union
		{[]string{"nothing.here"}, 0}, // no match is empty, not everything
	} {
		rep := Run(context.Background(), fixtureInput(t), Options{Insights: insights, Only: tc.only})
		if len(rep.Findings) != tc.want {
			t.Errorf("--only %v: want %d findings, got %d", tc.only, tc.want, len(rep.Findings))
		}
	}
}

func TestRun_MinSeverityCountsWhatItHides(t *testing.T) {
	rep := Run(context.Background(), fixtureInput(t), Options{
		Insights: []Insight{newFake("exposure.a", FamilyExposure,
			goodFinding("exposure.a", SeverityInfo),
			goodFinding("exposure.b", SeverityRisk),
		)},
		MinSeverity: SeverityWarn,
	})
	if len(rep.Findings) != 1 || rep.Hidden != 1 {
		t.Fatalf("want 1 finding and 1 hidden, got %d and %d", len(rep.Findings), rep.Hidden)
	}
	var buf bytes.Buffer
	if err := Render(rep, "table", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "hidden by the severity filter") {
		t.Error("a filtered report must say it is filtered")
	}
}

func TestRun_CancelledContextIsIncomplete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ran := 0
	rep := Run(ctx, fixtureInput(t), Options{Insights: []Insight{
		fakeInsight{id: "exposure.a", title: "t", family: FamilyExposure, ran: &ran},
	}})
	if ran != 0 {
		t.Error("insights ran after the context was cancelled")
	}
	if rep.Complete {
		t.Error("a cancelled run must not report itself complete")
	}
	if len(rep.Notes) == 0 || !strings.Contains(strings.Join(rep.Notes, " "), "partial") {
		t.Errorf("a cancelled run must say the report is partial: %v", rep.Notes)
	}
}

// ----------------------------------------------------------------------
// scope notes
// ----------------------------------------------------------------------

func TestRun_EmptyReportSaysItIsNotACleanBillOfHealth(t *testing.T) {
	rep := Run(context.Background(), fixtureInput(t), Options{Insights: []Insight{}})
	notes := strings.Join(rep.Notes, " ")
	if !strings.Contains(notes, "not a clean bill of health") {
		t.Errorf("an empty report must qualify its own silence: %v", rep.Notes)
	}
	var buf bytes.Buffer
	if err := Render(rep, "table", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No findings.") {
		t.Error("table renderer swallowed the empty case")
	}
}

func TestRun_NotesNameTheScopeGaps(t *testing.T) {
	// One provider, no payloads, no cost: the three conditions that most often
	// explain a thin report.
	assets := []core.Asset{
		{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1", Name: "example.com"},
	}
	rep := Run(context.Background(), NewInput(assets, WithNow(fixedNow)), Options{Insights: []Insight{}})
	notes := strings.Join(rep.Notes, "\n")
	for _, want := range []string{"Only one provider", "Asset.Raw", "Cost estimation is off"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes do not mention %q:\n%s", want, notes)
		}
	}
}

// ----------------------------------------------------------------------
// input
// ----------------------------------------------------------------------

func TestNewInput_SortsCanonicallyWithoutTouchingTheCaller(t *testing.T) {
	assets := fixtureAssets()
	shuffled := []core.Asset{assets[4], assets[0], assets[3], assets[2], assets[1]}
	before := append([]core.Asset(nil), shuffled...)

	in := NewInput(shuffled, WithNow(fixedNow))

	var got []string
	for _, a := range in.Assets {
		got = append(got, a.Provider+"/"+a.Type+"/"+a.ID)
	}
	want := []string{
		"cloudflare/cloudflare.dns_record/rec1",
		"cloudflare/cloudflare.zone/z1",
		"kubernetes/v1.Pod/uid-pod-1",
		"kubernetes/v1.Service/uid-svc-1",
		"oci/oci.load_balancer/ocid1.lb..lb1",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("assets are not in canonical order:\n got %v\nwant %v", got, want)
	}
	for i := range before {
		if shuffled[i].ID != before[i].ID {
			t.Fatalf("NewInput reordered the caller's slice at %d", i)
		}
	}
}

func TestNewInput_IndexesAndScope(t *testing.T) {
	in := fixtureInput(t)

	if got := len(in.ByType("v1.Pod")); got != 1 {
		t.Errorf("ByType(v1.Pod) = %d, want 1", got)
	}
	if got := len(in.ByType("v1.Pod", "v1.Service")); got != 2 {
		t.Errorf("ByType with two types = %d, want 2", got)
	}
	if got := len(in.ByProvider("cloudflare")); got != 2 {
		t.Errorf("ByProvider(cloudflare) = %d, want 2", got)
	}
	if !in.HasProvider("oci") || in.HasProvider("gcp") {
		t.Error("HasProvider is wrong about which providers contributed")
	}
	if a, ok := in.Asset(core.AssetRef{Provider: "oci", ID: "ocid1.lb..lb1"}); !ok || a.Name != "public-lb" {
		t.Error("Asset() did not resolve a (provider, id) reference")
	}
	if a, ok := in.AssetByID("z1"); !ok || a.Name != "example.com" {
		t.Error("AssetByID() did not resolve a bare id")
	}

	s := in.Scope
	if s.Assets != 5 || s.Types != 5 || len(s.Providers) != 3 {
		t.Errorf("scope counts are wrong: %+v", s)
	}
	if s.RawAssets != 2 || !s.RawAvailable() {
		t.Errorf("scope should see 2 raw payloads, saw %d", s.RawAssets)
	}
	if s.Priced {
		t.Error("scope reports cost on when no estimator was supplied")
	}
	if s.Edges == 0 || s.Edges != len(in.Graph.Edges) {
		t.Errorf("scope edge count (%d) disagrees with the graph (%d)", s.Edges, len(in.Graph.Edges))
	}
}

func TestInput_GraphAdjacency(t *testing.T) {
	in := fixtureInput(t)
	rec := core.AssetRef{Provider: "cloudflare", ID: "rec1"}

	if len(in.EdgesFrom(rec)) == 0 {
		t.Fatal("the DNS record should have an outgoing edge in the canonical chain")
	}
	if in.Degree(rec) != len(in.EdgesFrom(rec))+len(in.EdgesTo(rec)) {
		t.Error("Degree disagrees with the two edge lists")
	}
	if in.Degree(core.AssetRef{Provider: "oci", ID: "nope"}) != 0 {
		t.Error("an unknown asset should have degree 0, not a panic")
	}
}

func TestInput_RawAndRawPath(t *testing.T) {
	in := fixtureInput(t)
	pod, _ := in.AssetByID("uid-pod-1")
	zone, _ := in.AssetByID("z1")

	if _, ok := in.Raw(pod); !ok {
		t.Fatal("Raw did not parse a payload that is present")
	}
	if _, ok := in.Raw(zone); ok {
		t.Error("Raw claims to have parsed an absent payload")
	}
	if got, ok := in.RawString(pod, "spec.nodeName"); !ok || got != "node-1" {
		t.Errorf("RawString(spec.nodeName) = %q, %v", got, ok)
	}
	if got, ok := in.RawString(pod, "spec.containers.0.image"); !ok || got != "api:1.2.3" {
		t.Errorf("RawString through an array index = %q, %v", got, ok)
	}
	for _, path := range []string{"spec.missing", "spec.containers.9.image", "spec.containers.name", "spec.nodeName.deeper"} {
		if _, ok := in.RawPath(pod, path); ok {
			t.Errorf("RawPath(%q) resolved something that is not there", path)
		}
	}

	// Memoized: a second read of the same asset must not re-parse. Proven by
	// mutating the payload after the first read — a re-parse would see it.
	pod.Raw = json.RawMessage(`{"spec": {"nodeName": "node-2"}}`)
	if got, _ := in.RawString(pod, "spec.nodeName"); got != "node-1" {
		t.Errorf("Raw was re-parsed rather than memoized (got %q)", got)
	}
}

func TestInput_MonthlySplitsMeasuredFromEstimated(t *testing.T) {
	in := fixtureInput(t)
	lb, _ := in.AssetByID("ocid1.lb..lb1")

	if in.Priced() {
		t.Error("Priced() is true with no estimator")
	}
	if _, ok := in.Monthly(lb); ok {
		t.Error("Monthly returned a figure with cost off")
	}

	est := fixtureInput(t, WithEstimator(stubEstimator{monthly: map[string]float64{"oci.load_balancer": 18.25}}))
	m, ok := est.Monthly(lb)
	if !ok || m.Estimated != 18.25 || m.Measured != 0 {
		t.Fatalf("a list-price figure must land in Estimated: %+v (ok=%v)", m, ok)
	}
	if !strings.HasPrefix(m.String(), cost.EstimateMark) {
		t.Errorf("an estimated figure must carry the ~ glyph: %q", m.String())
	}
	if _, ok := est.Monthly(core.Asset{Provider: "oci", Type: "oci.vcn", ID: "v1"}); ok {
		t.Error("an unpriced asset must not produce a figure")
	}

	measured := fixtureInput(t, WithEstimator(stubEstimator{
		monthly: map[string]float64{"oci.load_balancer": 18.25}, measured: true,
	}))
	if m, _ := measured.Monthly(lb); m.Measured != 18.25 || m.Estimated != 0 {
		t.Errorf("a billed figure must land in Measured: %+v", m)
	}
}

func TestSumMoney_RefusesToCombineCurrencies(t *testing.T) {
	usd := []Money{{Currency: "USD", Estimated: 10}, {Currency: "USD", Measured: 5}}
	sum, ok := SumMoney(usd)
	if !ok || sum.Estimated != 10 || sum.Measured != 5 {
		t.Fatalf("same-currency sum is wrong: %+v (ok=%v)", sum, ok)
	}
	if _, ok := SumMoney([]Money{{Currency: "USD", Estimated: 10}, {Currency: "EUR", Estimated: 5}}); ok {
		t.Error("SumMoney combined two currencies; no exchange rate exists in this tool")
	}
	if _, ok := SumMoney(nil); ok {
		t.Error("summing nothing must not produce a zero figure")
	}
}

func TestNewInput_AcceptsAPrebuiltGraph(t *testing.T) {
	first := fixtureInput(t)
	second := NewInput(fixtureAssets(), WithNow(fixedNow), WithTopology(first.Graph))
	if second.Graph != first.Graph {
		t.Error("WithTopology did not reuse the supplied graph")
	}
	if len(NewInput(nil, WithNow(fixedNow)).Graph.Edges) != 0 {
		t.Error("an empty inventory must still produce a usable, empty graph")
	}
}

// ----------------------------------------------------------------------
// rendering
// ----------------------------------------------------------------------

func twoFindingReport(t *testing.T) *Report {
	t.Helper()
	a := goodFinding("exposure.public-endpoints", SeverityRisk)
	a.Caveat = "an inventory records that an address is public, not that anything reaches it"
	b := goodFinding("cost.idle-volumes", SeverityNotable)
	b.Title = "Detached block volumes"
	b.Caveat = "a volume can be detached deliberately, and this tool cannot see a restore plan"
	b.Total = &Money{Currency: "USD", Estimated: 412.9}
	b.Rows = []Row{
		{Label: "vol-a", Fact: "detached 30 days", Value: "200 GB", Money: &Money{Currency: "USD", Estimated: 12.5}},
		{Label: "vol-b", Fact: "detached 9 days", Value: "50 GB"},
	}
	return Run(context.Background(), fixtureInput(t), Options{Insights: []Insight{
		newFake("exposure.x", FamilyExposure, a),
		newFake("cost.x", FamilyCost, b),
	}})
}

// TestRenderTable_CaveatTravelsWithEveryFinding is the rendering half of the
// house rule: the caveat is next to the number, above the rows, once per
// finding — not pooled into a footer nobody reads.
func TestRenderTable_CaveatTravelsWithEveryFinding(t *testing.T) {
	rep := twoFindingReport(t)
	var buf bytes.Buffer
	if err := Render(rep, "table", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// Count the label column, not the phrase: the disclaimer uses the words
	// too, and a test that cannot tell them apart proves nothing.
	labelled := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "cannot know ") {
			labelled++
		}
	}
	if labelled != 2 {
		t.Errorf("want one 'cannot know' line per finding (2), got %d", labelled)
	}
	for _, f := range rep.Findings {
		// The caveat may be wrapped, so look for its opening words.
		head := strings.Join(strings.Fields(f.Caveat)[:4], " ")
		if !strings.Contains(out, head) {
			t.Errorf("finding %q: caveat is missing from the table", f.ID)
			continue
		}
		if strings.Index(out, head) > strings.Index(out, "vol-a") && f.ID == "cost.idle-volumes" {
			t.Errorf("finding %q: caveat is printed after its rows", f.ID)
		}
	}
	if !strings.Contains(out, "An inventory cannot see:") {
		t.Error("the disclaimer box is missing from the table")
	}
	// Wrapped, so match its opening rather than the whole line.
	if !strings.Contains(out, "Estimate, not an invoice") {
		t.Error("a finding carrying money must carry the cost disclaimer with it")
	}
}

// TestRenderTable_SeverityIsMarkedWithoutColour asserts the glyph and the word
// are both present: nothing here emits ANSI, so the mark is the whole signal.
func TestRenderTable_SeverityIsMarkedWithoutColour(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(twoFindingReport(t), "table", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "!! risk") {
		t.Error("a risk finding must carry both the glyph and the word")
	}
	if !strings.Contains(out, "*  notable") && !strings.Contains(out, "* notable") {
		t.Errorf("a notable finding must carry both the glyph and the word:\n%s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("table output must not contain ANSI escapes")
	}
}

// TestRenderTable_CountsAreRightAlignedAndTabular checks the headline layout:
// every count lands in the same column regardless of title length, which is
// what makes a page of findings scannable.
func TestRenderTable_CountsAreRightAlignedAndTabular(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(twoFindingReport(t), "table", &buf); err != nil {
		t.Fatal(err)
	}
	var headlines []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "..") && strings.HasPrefix(line, "  ") {
			headlines = append(headlines, line)
		}
	}
	if len(headlines) != 2 {
		t.Fatalf("want 2 finding headlines, got %d", len(headlines))
	}
	for _, line := range headlines {
		if runeLen(line) != pageWidth {
			t.Errorf("headline is %d columns, want %d: %q", runeLen(line), pageWidth, line)
		}
	}
}

func TestRenderTable_CapsRowsAndSaysSo(t *testing.T) {
	f := goodFinding("hygiene.untagged", SeverityInfo)
	f.Rows = nil
	for i := 0; i < DefaultMaxRows+5; i++ {
		f.Rows = append(f.Rows, Row{Label: fmt.Sprintf("asset-%d", i), Fact: "no owner tag"})
	}
	rep := Run(context.Background(), fixtureInput(t), Options{
		Insights: []Insight{newFake("hygiene.x", FamilyHygiene, f)},
	})

	var buf bytes.Buffer
	if err := Render(rep, "table", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "… and 5 more") {
		t.Error("a capped row list must say how many it dropped")
	}

	var all bytes.Buffer
	if err := Render(rep, "table", &all, WithMaxRows(-1)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(all.String(), "and 5 more") {
		t.Error("WithMaxRows(-1) should print every row")
	}
}

func TestRenderJSON_CarriesTheContract(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(twoFindingReport(t), "json", &buf); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Disclaimer string `json:"disclaimer"`
		Complete   bool   `json:"complete"`
		Scope      Scope  `json:"scope"`
		Findings   []struct {
			ID     string `json:"id"`
			Family string `json:"family"`
			Caveat string `json:"caveat"`
			Basis  string `json:"basis"`
			Total  *struct {
				Display string `json:"display"`
			} `json:"total"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	if got.Disclaimer == "" || !got.Complete {
		t.Error("the JSON report must carry the disclaimer as a required field")
	}
	if len(got.Findings) != 2 {
		t.Fatalf("want 2 findings, got %d", len(got.Findings))
	}
	for _, f := range got.Findings {
		if f.Caveat == "" || f.Basis == "" || f.Family == "" {
			t.Errorf("finding %q lost its contract fields in JSON: %+v", f.ID, f)
		}
	}
	// Money serialises as a display string, never a bare number a consumer
	// could add to an invoice.
	total := got.Findings[1].Total
	if total == nil || !strings.Contains(total.Display, "~") {
		t.Errorf("a total must serialise as an estimate-marked string: %+v", total)
	}
}

func TestRenderMarkdown_CaveatIsAQuoteAboveTheTable(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(twoFindingReport(t), "markdown", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if n := strings.Count(out, "> **Cannot know**"); n != 2 {
		t.Errorf("want one caveat blockquote per finding, got %d", n)
	}
	if strings.Index(out, "**Cannot know**") > strings.Index(out, "| vol-a") {
		t.Error("the caveat must come before the detail table")
	}
	if !strings.Contains(out, "### EXPOSURE") || !strings.Contains(out, "### COST") {
		t.Error("markdown must group findings by family")
	}
}

func TestRender_UnknownFormat(t *testing.T) {
	err := Render(twoFindingReport(t), "yaml", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "table|json|markdown") {
		t.Errorf("an unknown format must name the ones that exist: %v", err)
	}
}

func TestRenderTable_AbsentMoneyIsNeverZero(t *testing.T) {
	if got := moneyCell(nil); got != "" {
		t.Errorf("absent money rendered as %q; it must never look like a figure", got)
	}
	empty := &Money{Currency: "USD"}
	if got := empty.String(); got == "0.00" || got == "$0.00" {
		t.Errorf("an empty total rendered as %q rather than %q", got, cost.NoMoney)
	}
}

// ----------------------------------------------------------------------
// small pieces
// ----------------------------------------------------------------------

func TestSeverity(t *testing.T) {
	if SeverityRisk.Rank() <= SeverityWarn.Rank() || SeverityWarn.Rank() <= SeverityNotable.Rank() {
		t.Error("severity ranks are out of order")
	}
	if Severity("critical").Valid() {
		t.Error("an unknown severity must not validate")
	}
	for in, want := range map[string]Severity{
		"info": SeverityInfo, "NOTABLE": SeverityNotable, "warn": SeverityWarn,
		"warning": SeverityWarn, " risk ": SeverityRisk,
	} {
		got, err := ParseSeverity(in)
		if err != nil || got != want {
			t.Errorf("ParseSeverity(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseSeverity("nope"); err == nil {
		t.Error("ParseSeverity accepted an unknown severity")
	}
}

func TestReport_MaxSeverityAndCounts(t *testing.T) {
	rep := twoFindingReport(t)
	got, ok := rep.MaxSeverity()
	if !ok || got != SeverityRisk {
		t.Errorf("MaxSeverity = %q, %v", got, ok)
	}
	if counts := rep.Severities(); counts[SeverityRisk] != 1 || counts[SeverityNotable] != 1 {
		t.Errorf("severity counts are wrong: %v", counts)
	}
	if _, ok := (&Report{}).MaxSeverity(); ok {
		t.Error("an empty report has no max severity")
	}
}

func TestAssetRow_FallsBackToID(t *testing.T) {
	unnamed := core.Asset{Provider: "oci", Type: "oci.vcn", ID: "ocid1.vcn..x"}
	if got := AssetRow(unnamed, "no name").Label; got != "ocid1.vcn..x" {
		t.Errorf("an unnamed asset must display its id, got %q", got)
	}
	named := core.Asset{Provider: "oci", Type: "oci.vcn", ID: "ocid1.vcn..x", Name: "prod-vcn"}
	row := AssetRow(named, "fact")
	if row.Label != "prod-vcn" || row.Asset == nil || row.Asset.ID != "ocid1.vcn..x" {
		t.Errorf("AssetRow did not point at its asset: %+v", row)
	}
}

func TestWrapTextAndFormatInt(t *testing.T) {
	lines := wrapText(strings.Repeat("word ", 40), 20)
	if len(lines) < 2 {
		t.Fatal("wrapText did not wrap")
	}
	for _, l := range lines {
		if runeLen(l) > 20 {
			t.Errorf("line exceeds the width: %q", l)
		}
	}
	if wrapText("   ", 20) != nil {
		t.Error("wrapping nothing must produce nothing")
	}
	if got := formatInt(17432); got != "17,432" {
		t.Errorf("formatInt(17432) = %q", got)
	}
	if got := formatInt(-1234); got != "-1,234" {
		t.Errorf("formatInt(-1234) = %q", got)
	}
}

func TestFamilyTitle(t *testing.T) {
	if got := FamilyExposure.Title(); got != "EXPOSURE" {
		t.Errorf("Family.Title() = %q", got)
	}
	if got := Family("single-point-of-failure").Title(); got != "SINGLE POINT OF FAILURE" {
		t.Errorf("Family.Title() = %q", got)
	}
}
