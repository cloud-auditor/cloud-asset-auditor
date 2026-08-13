package insight

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

// Tests for the hygiene insights.
//
// Each of these findings is one sentence away from being an accusation, so the
// assertions below are as much about what the findings refuse to say — an
// untagged resource is not unowned, an old resource is not stale, a namespace
// with no NetworkPolicy may be on a cluster that enforces none — as about the
// counting.

// ----------------------------------------------------------------------
// fixtures
// ----------------------------------------------------------------------

// hyAsset is a minimal asset with tags, for the ownership and expiry tests.
func hyAsset(provider, typ, id string, tags map[string]string) core.Asset {
	return core.Asset{
		Provider: provider, AccountID: "acct", Type: typ, ID: id, Name: id, Tags: tags,
	}
}

// hyAged is an asset created age before the fixed audit clock.
func hyAged(typ, id string, age time.Duration) core.Asset {
	created := fixedNow.Add(-age)
	return core.Asset{
		Provider: "oci", AccountID: "acct", Region: "us-ashburn-1",
		Type: typ, ID: id, Name: id, CreatedAt: &created,
	}
}

func hyOn(tb testing.TB, ins Insight, assets ...core.Asset) []Finding {
	tb.Helper()
	return ins.Run(context.Background(), NewInput(assets, WithNow(fixedNow)))
}

func hyOne(tb testing.TB, fs []Finding) Finding {
	tb.Helper()
	if len(fs) != 1 {
		tb.Fatalf("want exactly 1 finding, got %d (%v)", len(fs), uzIDs(fs))
	}
	return fs[0]
}

func hyPick(tb testing.TB, fs []Finding, id string) Finding {
	tb.Helper()
	for _, f := range fs {
		if f.ID == id {
			return f
		}
	}
	tb.Fatalf("no finding %q in %v", id, uzIDs(fs))
	return Finding{}
}

func hyInsights() []Insight {
	return []Insight{
		NewOwnershipInsight(), recentlyCreatedInsight{}, ageingInsight{},
		expiryInsight{}, namespacePolicyInsight{}, singleReplicaInsight{},
	}
}

// ----------------------------------------------------------------------
// ownership
// ----------------------------------------------------------------------

// The claim is "these fell out of a convention their own type follows", which
// is much stronger than "these have no owner tag" — and the only one an
// inventory supports. A type nobody tags is not a finding.
func TestOwnership_OnlyTypesWhereTheConventionExists(t *testing.T) {
	f := hyOne(t, hyOn(t, NewOwnershipInsight(),
		hyAsset("oci", "oci.compute.instance", "i1", map[string]string{"owner": "payments"}),
		hyAsset("oci", "oci.compute.instance", "i2", map[string]string{"owner": "search"}),
		hyAsset("oci", "oci.compute.instance", "i3", nil),
		// A type where nobody tags anything: not a deviation, not a finding.
		hyAsset("cloudflare", "cloudflare.dns_record", "r1", nil),
		hyAsset("cloudflare", "cloudflare.dns_record", "r2", nil),
	))

	if f.ID != "hygiene.unowned-resources" {
		t.Fatalf("id = %q", f.ID)
	}
	if f.Count != 1 {
		t.Errorf("count = %d, want only the instance that broke its type's convention", f.Count)
	}
	if got := uzRowLabels(f); !uzEqual(got, []string{"oci.compute.instance"}) {
		t.Errorf("rows = %v, want only the deviating type", got)
	}
	if f.Rows[0].Value != "1 of 3" {
		t.Errorf("value = %q, want the gap over the group", f.Rows[0].Value)
	}
	if !strings.Contains(f.Rows[0].Fact, "the other 2 use owner") {
		t.Errorf("the row must name the convention being broken, got %q", f.Rows[0].Fact)
	}
	if f.Severity != SeverityWarn {
		t.Errorf("severity = %q, want warn", f.Severity)
	}
}

// The concrete gap in this tool, and the reason the finding can never be read
// as "these resources are unowned".
func TestOwnership_CaveatNamesTheOCIDefinedTagGap(t *testing.T) {
	f := hyOne(t, hyOn(t, NewOwnershipInsight(),
		hyAsset("oci", "oci.compute.instance", "i1", map[string]string{"team": "payments"}),
		hyAsset("oci", "oci.compute.instance", "i2", nil),
	))
	for _, want := range []string{"freeform tags only", "OCI defined tags", "CODEOWNERS", "unowned, unused or safe to delete"} {
		if !strings.Contains(f.Caveat, want) {
			t.Errorf("caveat missing %q:\n%s", want, f.Caveat)
		}
	}
}

// One vocabulary entry has to cover the four spellings every estate
// accumulates, or the finding reports a convention break that is really a
// capitalisation difference.
func TestOwnership_KeySpellingsNormalise(t *testing.T) {
	got := hyOn(t, NewOwnershipInsight("cost-center"),
		hyAsset("oci", "oci.compute.instance", "i1", map[string]string{"CostCenter": "cc-1"}),
		hyAsset("oci", "oci.compute.instance", "i2", map[string]string{"cost_center": "cc-2"}),
		hyAsset("oci", "oci.compute.instance", "i3", map[string]string{"Cost Center": "cc-3"}),
		// Namespaced keys compare on their last segment.
		hyAsset("kubernetes", "v1.Pod", "p1", map[string]string{"acme.io/cost-center": "cc-4"}),
	)
	if len(got) != 0 {
		t.Fatalf("every asset carries the key in some spelling; got %v", uzIDs(got))
	}
}

// A key that was filled in rather than answered is not ownership. Counting
// owner=TBD as tagged is how a governance report reaches 100% while nobody can
// find who to page.
func TestOwnership_PlaceholderValuesAreNotOwnership(t *testing.T) {
	f := hyOne(t, hyOn(t, NewOwnershipInsight(),
		hyAsset("oci", "oci.compute.instance", "i1", map[string]string{"owner": "payments"}),
		hyAsset("oci", "oci.compute.instance", "i2", map[string]string{"owner": "TBD"}),
		hyAsset("oci", "oci.compute.instance", "i3", map[string]string{"owner": "  "}),
	))
	if f.Count != 2 {
		t.Errorf("count = %d, want the TBD and the blank counted as untagged", f.Count)
	}
}

// An estate that matched nothing gets a different claim, because the likeliest
// explanation is the vocabulary rather than the estate.
func TestOwnership_NoConventionAnywhere(t *testing.T) {
	f := hyOne(t, hyOn(t, NewOwnershipInsight(),
		hyAsset("oci", "oci.compute.instance", "i1", map[string]string{"shape": "VM.Standard"}),
		hyAsset("cloudflare", "cloudflare.zone", "z1", nil),
	))

	if f.ID != "hygiene.no-ownership-convention" {
		t.Fatalf("id = %q, want the no-convention finding", f.ID)
	}
	if f.Severity != SeverityNotable {
		t.Errorf("severity = %q; this is more likely a statement about the vocabulary", f.Severity)
	}
	if !strings.Contains(f.Caveat, "tag names this insight was given") {
		t.Errorf("caveat must implicate the vocabulary first:\n%s", f.Caveat)
	}
	if got := uzRowLabels(f); !uzEqual(got, []string{"cloudflare", "oci"}) {
		t.Errorf("rows = %v, want one per provider", got)
	}
}

func TestOwnership_CustomVocabulary(t *testing.T) {
	assets := []core.Asset{
		hyAsset("oci", "oci.compute.instance", "i1", map[string]string{"squad": "payments"}),
		hyAsset("oci", "oci.compute.instance", "i2", nil),
	}
	// The default vocabulary knows nothing about "squad", so it sees no
	// convention at all.
	if f := hyOne(t, hyOn(t, NewOwnershipInsight(), assets...)); f.ID != "hygiene.no-ownership-convention" {
		t.Errorf("default vocabulary: id = %q", f.ID)
	}
	f := hyOne(t, hyOn(t, NewOwnershipInsight("squad"), assets...))
	if f.ID != "hygiene.unowned-resources" || f.Count != 1 {
		t.Errorf("custom vocabulary: id = %q count = %d", f.ID, f.Count)
	}
	if !strings.Contains(f.Basis, "squad") {
		t.Errorf("basis must name the vocabulary it used: %q", f.Basis)
	}
}

// Configuration must not be retroactive: an insight already built keeps the
// vocabulary it was built with.
func TestOwnership_DefaultKeysAreCopiedAtConstruction(t *testing.T) {
	ins := NewOwnershipInsight()
	saved := DefaultOwnerTagKeys
	DefaultOwnerTagKeys = []string{"squad"}
	defer func() { DefaultOwnerTagKeys = saved }()

	f := hyOne(t, hyOn(t, ins,
		hyAsset("oci", "oci.compute.instance", "i1", map[string]string{"owner": "payments"}),
		hyAsset("oci", "oci.compute.instance", "i2", nil),
	))
	if f.ID != "hygiene.unowned-resources" {
		t.Errorf("id = %q; the built insight must keep its own keys", f.ID)
	}
}

func TestOwnership_EmptyInventoryIsNoFinding(t *testing.T) {
	if got := hyOn(t, NewOwnershipInsight()); len(got) != 0 {
		t.Errorf("got %v", uzIDs(got))
	}
}

// ----------------------------------------------------------------------
// age
// ----------------------------------------------------------------------

func TestRecentlyCreated_WindowAndFreshCount(t *testing.T) {
	f := hyOne(t, hyOn(t, recentlyCreatedInsight{},
		hyAged("oci.compute.instance", "yesterday", 24*time.Hour),
		hyAged("oci.compute.instance", "three-weeks", 21*24*time.Hour),
		hyAged("oci.compute.instance", "last-year", 400*24*time.Hour),
		core.Asset{Provider: "oci", Type: "oci.vcn", ID: "undated", Name: "undated"},
	))

	if f.Count != 2 {
		t.Errorf("count = %d, want the two inside 30 days", f.Count)
	}
	if got := uzRowLabels(f); !uzEqual(got, []string{"yesterday", "three-weeks"}) {
		t.Errorf("rows = %v, want newest first", got)
	}
	if f.Rows[0].Value != "1 day ago" {
		t.Errorf("value = %q", f.Rows[0].Value)
	}
	if !strings.Contains(f.Summary, "1 of them in the last 7") {
		t.Errorf("summary must call out the last week: %q", f.Summary)
	}
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %q; new resources are what a working estate produces", f.Severity)
	}
	// The number that makes this finding a floor rather than a census.
	if !strings.Contains(f.Caveat, "Only 3 of 4 collected assets report a creation time") {
		t.Errorf("caveat must state the coverage:\n%s", f.Caveat)
	}
}

func TestAgeing_OldestFirstAndNotAnAccusation(t *testing.T) {
	f := hyOne(t, hyOn(t, ageingInsight{},
		hyAged("oci.compute.instance", "three-years", 3*365*24*time.Hour),
		hyAged("oci.compute.instance", "five-years", 5*365*24*time.Hour),
		hyAged("oci.compute.instance", "one-year", 365*24*time.Hour),
	))

	if f.Count != 2 {
		t.Errorf("count = %d, want the two past two years", f.Count)
	}
	if got := uzRowLabels(f); !uzEqual(got, []string{"five-years", "three-years"}) {
		t.Errorf("rows = %v, want oldest first", got)
	}
	if !strings.Contains(f.Rows[0].Value, "5 years ago") {
		t.Errorf("value = %q", f.Rows[0].Value)
	}
	if f.Severity != SeverityNotable {
		t.Errorf("severity = %q, want notable", f.Severity)
	}
	for _, want := range []string{"Age is not disuse", "load-bearing"} {
		if !strings.Contains(f.Caveat, want) {
			t.Errorf("caveat missing %q:\n%s", want, f.Caveat)
		}
	}
}

// Every asset carrying a date is the common case for a Kubernetes-heavy or
// OCI-heavy audit, and the coverage sentence used to render as "Only 3 of 3
// collected assets report a creation time at all; the rest are absent" — a
// remainder that does not exist. A caveat containing an arithmetic absurdity
// gets read as boilerplate, which costs the sentences beside it that matter.
func TestAgeCoverage_FullCoverageDoesNotDescribeAnEmptyRemainder(t *testing.T) {
	f := hyOne(t, hyOn(t, ageingInsight{},
		hyAged("oci.compute.instance", "three-years", 3*365*24*time.Hour),
		hyAged("oci.compute.instance", "five-years", 5*365*24*time.Hour),
	))

	if strings.Contains(f.Caveat, "Only 2 of 2") {
		t.Errorf("caveat claims a missing remainder that cannot exist:\n%s", f.Caveat)
	}
	if strings.Contains(f.Caveat, "the rest are absent") {
		t.Errorf("caveat refers to assets outside the finding when there are none:\n%s", f.Caveat)
	}
	if !strings.Contains(f.Caveat, "All 2 collected assets report a creation time") {
		t.Errorf("caveat must still state coverage, positively:\n%s", f.Caveat)
	}
	// The half that is not about coverage must survive either branch.
	if !strings.Contains(f.Caveat, "not the configuration's") {
		t.Errorf("caveat lost the created-vs-reconfigured warning:\n%s", f.Caveat)
	}
}

func TestAge_NoDatedAssetsIsNoFinding(t *testing.T) {
	bare := core.Asset{Provider: "oci", Type: "oci.vcn", ID: "v1", Name: "v1"}
	if got := hyOn(t, recentlyCreatedInsight{}, bare); len(got) != 0 {
		t.Errorf("recent: got %v", uzIDs(got))
	}
	if got := hyOn(t, ageingInsight{}, bare); len(got) != 0 {
		t.Errorf("ageing: got %v", uzIDs(got))
	}
}

func TestHygieneWhen(t *testing.T) {
	day := 24 * time.Hour
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{6 * time.Hour, "today"},
		{6 * day, "in 6 days"},
		{-6 * day, "6 days ago"},
		{-120 * day, "3 months ago"},
		{-3 * 365 * day, "3 years ago"},
	} {
		if got := hygieneWhen(tc.d); got != tc.want {
			t.Errorf("hygieneWhen(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// ----------------------------------------------------------------------
// expiry
// ----------------------------------------------------------------------

func hyCert(id string, in time.Duration) core.Asset {
	return hyAsset("cloudflare", "cloudflare.custom_certificate", id, map[string]string{
		"expires_on": fixedNow.Add(in).Format(time.RFC3339),
	})
}

func TestExpiry_SoonAndAlreadyPast(t *testing.T) {
	day := 24 * time.Hour
	got := hyOn(t, expiryInsight{},
		hyCert("expiring-soon", 6*day),
		hyCert("expiring-later", 20*day),
		hyCert("well-clear", 200*day),
		hyCert("lapsed", -3*day),
	)
	if len(got) != 2 {
		t.Fatalf("want an expiring and an expired finding, got %v", uzIDs(got))
	}

	soon := hyPick(t, got, "hygiene.expiring-credentials")
	if soon.Count != 2 {
		t.Errorf("count = %d, want the two inside 30 days", soon.Count)
	}
	if labels := uzRowLabels(soon); !uzEqual(labels, []string{"expiring-soon", "expiring-later"}) {
		t.Errorf("rows = %v, want soonest first", labels)
	}
	if soon.Rows[0].Value != "in 6 days" {
		t.Errorf("value = %q", soon.Rows[0].Value)
	}
	if !strings.Contains(soon.Caveat, "reissued automatically") {
		t.Errorf("caveat must allow for auto-renewal:\n%s", soon.Caveat)
	}

	expired := hyPick(t, got, "hygiene.expired-credentials")
	if expired.Count != 1 || expired.Rows[0].Label != "lapsed" {
		t.Errorf("expired = %d rows %v", expired.Count, uzRowLabels(expired))
	}
	if !strings.Contains(expired.Caveat, "usually inert") {
		t.Errorf("caveat must not imply an outage:\n%s", expired.Caveat)
	}
	// Both findings must say which expiries this tool never sees.
	for _, f := range got {
		if !strings.Contains(f.Caveat, "inside a payload") {
			t.Errorf("%s caveat must bound its coverage:\n%s", f.ID, f.Caveat)
		}
	}
}

// A Tailscale node with key expiry disabled still reports the timestamp its key
// would have had. Reading that as an expiry manufactures an expired credential
// for every long-lived server in the tailnet.
func TestExpiry_DisabledKeyExpiryIsIgnored(t *testing.T) {
	device := hyAsset("tailscale", "tailscale.device", "server-1", map[string]string{
		"expires":             fixedNow.Add(-90 * 24 * time.Hour).Format(time.RFC3339),
		"key_expiry_disabled": "true",
	})
	if got := hyOn(t, expiryInsight{}, device); len(got) != 0 {
		t.Errorf("a frozen key expiry is not an expiry; got %v", uzIDs(got))
	}
}

// An unparseable date is skipped and counted, never guessed at — and the zero
// time providers use for "no expiry" is not forty thousand years overdue.
func TestExpiry_UnparseableAndZeroDatesAreSkipped(t *testing.T) {
	got := hyOn(t, expiryInsight{},
		hyAsset("netbird", "netbird.setup_key", "k1", map[string]string{"expires": "never"}),
		hyAsset("tailscale", "tailscale.key", "k2", map[string]string{"expires": "0001-01-01T00:00:00Z"}),
	)
	if len(got) != 0 {
		t.Fatalf("nothing here has a readable expiry; got %v", uzIDs(got))
	}

	withReal := hyOn(t, expiryInsight{},
		hyAsset("netbird", "netbird.setup_key", "k1", map[string]string{"expires": "never"}),
		hyCert("real", 5*24*time.Hour),
	)
	f := hyOne(t, withReal)
	if !strings.Contains(f.Caveat, "could not parse") {
		t.Errorf("the skipped tag must be disclosed:\n%s", f.Caveat)
	}
}

func TestExpiry_TagKeyChoiceIsDeterministic(t *testing.T) {
	// Two expiry-shaped keys on one asset: Tags is a map, so an unsorted
	// lookup would make the report differ between runs.
	a := hyAsset("cloudflare", "cloudflare.custom_certificate", "c1", map[string]string{
		"expires":    fixedNow.Add(2 * 24 * time.Hour).Format(time.RFC3339),
		"expires_on": fixedNow.Add(9 * 24 * time.Hour).Format(time.RFC3339),
	})
	for i := 0; i < 20; i++ {
		key, _, ok := hygieneExpiryTag(a)
		if !ok || key != "expires" {
			t.Fatalf("run %d picked %q (ok=%v), want the first key in sorted order", i, key, ok)
		}
	}
}

// ----------------------------------------------------------------------
// namespaces with no NetworkPolicy
// ----------------------------------------------------------------------

func hyPod(ns, name string, labels map[string]string) core.Asset {
	tags := map[string]string{"namespace": ns}
	for k, v := range labels {
		tags[k] = v
	}
	return core.Asset{
		Provider: "kubernetes", AccountID: uzCluster, Type: hygienePodType,
		ID: "uid-" + ns + "-" + name, Name: name, Status: "Running", Tags: tags,
	}
}

func hyNetPol(ns, name string, raw string) core.Asset {
	return core.Asset{
		Provider: "kubernetes", AccountID: uzCluster, Type: hygieneNetworkPolicyType,
		ID: "uid-np-" + ns + "-" + name, Name: name,
		Tags: map[string]string{"namespace": ns},
		Raw:  json.RawMessage(raw),
	}
}

func TestNamespacePolicy_FlagsOnlyNamespacesWithNothingCoveringThem(t *testing.T) {
	f := hyOne(t, hyOn(t, namespacePolicyInsight{},
		hyPod("open", "a", nil),
		hyPod("open", "b", nil),
		hyPod("guarded", "c", nil),
		hyNetPol("guarded", "default-deny", `{"spec":{"podSelector":{}}}`),
	))

	if f.Count != 1 {
		t.Errorf("count = %d, want only the unguarded namespace", f.Count)
	}
	if got := uzRowLabels(f); !uzEqual(got, []string{uzCluster + "/open"}) {
		t.Errorf("rows = %v", got)
	}
	if f.Rows[0].Value != "2 pods" {
		t.Errorf("value = %q", f.Rows[0].Value)
	}
	if !strings.Contains(f.Summary, "against 1 that do") {
		t.Errorf("summary must show the denominator: %q", f.Summary)
	}
	if f.Severity != SeverityNotable {
		t.Errorf("severity = %q; default-allow is the platform default, not a mistake", f.Severity)
	}
	if !strings.Contains(f.Caveat, "cannot see which plugin is installed") {
		t.Errorf("caveat must name the CNI unknown:\n%s", f.Caveat)
	}
}

// The reuse the brief asks for: internal/topology has already read every policy
// body, so a pod on either end of a traffic edge is covered by a policy
// statement this run could read — whatever namespace it lives in.
func TestNamespacePolicy_TrafficEdgesCountAsCoverage(t *testing.T) {
	pod := hyPod("beta", "a", nil)
	graph := &topology.Topology{Edges: []core.Edge{{
		From: core.AssetRef{Provider: "kubernetes", Type: "tailscale.acl_rule", ID: "rule-1"},
		To:   pod.AsRef(),
		Kind: core.EdgeKindTrafficAllow, Confidence: core.ConfidenceExact,
	}}}

	in := NewInput([]core.Asset{pod}, WithNow(fixedNow), WithTopology(graph))
	if got := (namespacePolicyInsight{}).Run(context.Background(), in); len(got) != 0 {
		t.Errorf("a namespace named by a policy edge is not default-allow; got %v", uzIDs(got))
	}
}

// The edge-derived coverage has to work against the real resolver, not just a
// hand-built graph: this is the join the finding claims in its Basis.
func TestNamespacePolicy_RealResolverProducesTheCoverage(t *testing.T) {
	assets := []core.Asset{
		hyPod("secure", "api", map[string]string{"app": "api"}),
		hyPod("secure", "web", map[string]string{"app": "web"}),
		hyNetPol("secure", "api-ingress", `{"spec":{
			"podSelector":{"matchLabels":{"app":"api"}},
			"ingress":[{"from":[{"podSelector":{"matchLabels":{"app":"web"}}}]}]}}`),
	}
	in := NewInput(assets, WithNow(fixedNow))
	if in.Scope.Edges == 0 {
		t.Fatal("the fixture must produce real traffic edges for this test to mean anything")
	}
	if got := (namespacePolicyInsight{}).Run(context.Background(), in); len(got) != 0 {
		t.Errorf("got %v", uzIDs(got))
	}
}

// A namespace whose only pods are finished CronJob runs is not a workload the
// absence of a policy exposes.
func TestNamespacePolicy_TerminalPodsDoNotOpenANamespace(t *testing.T) {
	done := hyPod("batch", "job-run-1", nil)
	done.Status = "Succeeded"
	if got := (namespacePolicyInsight{}).Run(context.Background(),
		NewInput([]core.Asset{done}, WithNow(fixedNow))); len(got) != 0 {
		t.Errorf("got %v", uzIDs(got))
	}
}

// Zero policies anywhere is what an unpoliced cluster looks like and equally
// what a service account that cannot list them looks like. The finding has to
// name the ambiguity rather than pick a side.
func TestNamespacePolicy_NoPoliciesAnywhereIsAmbiguous(t *testing.T) {
	f := hyOne(t, hyOn(t, namespacePolicyInsight{}, hyPod("prod", "a", nil)))
	if !strings.Contains(f.Caveat, "cannot list networkpolicies") {
		t.Errorf("caveat must offer the RBAC explanation:\n%s", f.Caveat)
	}
}

func TestNamespacePolicy_NeedsPodsToRun(t *testing.T) {
	rep := Run(context.Background(), NewInput(nil, WithNow(fixedNow)),
		Options{Insights: []Insight{namespacePolicyInsight{}}})
	if len(rep.Skipped) != 1 {
		t.Fatalf("want a skip, got %+v", rep.Skipped)
	}
}

// It reads the presence of policy objects and the graph, never a policy spec —
// so it must work on a snapshot collected without --include-raw.
func TestNamespacePolicy_WorksWithoutRaw(t *testing.T) {
	f := hyOne(t, hyOn(t, namespacePolicyInsight{}, hyPod("prod", "a", nil)))
	if f.Count != 1 {
		t.Errorf("count = %d", f.Count)
	}
}

// ----------------------------------------------------------------------
// single-replica workloads
// ----------------------------------------------------------------------

func hyWorkload(typ, ns, name string, replicas, ready int) core.Asset {
	return core.Asset{
		Provider: "kubernetes", AccountID: uzCluster, Type: typ,
		ID: "uid-" + name, Name: name, Tags: map[string]string{"namespace": ns},
		Raw: uzRaw(map[string]any{
			"spec":   map[string]any{"replicas": replicas},
			"status": map[string]any{"readyReplicas": ready},
		}),
	}
}

func TestSingleReplica_OnesNotZeros(t *testing.T) {
	f := hyOne(t, hyOn(t, singleReplicaInsight{},
		hyWorkload(hygieneDeploymentType, "prod", "api", 1, 1),
		hyWorkload(hygieneDeploymentType, "prod", "web", 3, 3),
		hyWorkload(hygieneStatefulSetType, "prod", "db", 1, 1),
		// Scaled to zero is a different question, not a redundancy one.
		hyWorkload(hygieneDeploymentType, "prod", "paused", 0, 0),
	))

	if f.Count != 2 {
		t.Errorf("count = %d, want the Deployment and the StatefulSet at 1", f.Count)
	}
	if got := uzRowLabels(f); !uzEqual(got, []string{"prod/api", "prod/db"}) {
		t.Errorf("rows = %v", got)
	}
	if f.Rows[0].Value != "1 replica" {
		t.Errorf("value = %q", f.Rows[0].Value)
	}
	if !strings.Contains(f.Rows[0].Fact, "1 ready") {
		t.Errorf("fact should carry the observed side too: %q", f.Rows[0].Fact)
	}
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %q; a single replica is a legitimate configuration far more often "+
			"than it is an oversight", f.Severity)
	}
	if !strings.Contains(f.Summary, "of 4") {
		t.Errorf("summary should carry the denominator: %q", f.Summary)
	}
	for _, want := range []string{"leader-elected", "HorizontalPodAutoscaler", "DaemonSets are not counted"} {
		if !strings.Contains(f.Caveat, want) {
			t.Errorf("caveat missing %q:\n%s", want, f.Caveat)
		}
	}
}

// The API server defaults spec.replicas, so an unreadable one means the payload
// is absent or truncated — not that the workload runs one copy.
func TestSingleReplica_UnreadableSpecIsDisclosedNotAssumed(t *testing.T) {
	blind := core.Asset{
		Provider: "kubernetes", AccountID: uzCluster, Type: hygieneDeploymentType,
		ID: "uid-blind", Name: "blind", Tags: map[string]string{"namespace": "prod"},
		Raw: json.RawMessage(`{"spec":{}}`),
	}
	f := hyOne(t, hyOn(t, singleReplicaInsight{},
		hyWorkload(hygieneDeploymentType, "prod", "api", 1, 1), blind))

	if f.Count != 1 {
		t.Errorf("count = %d, want only the readable one", f.Count)
	}
	if !strings.Contains(f.Caveat, "1 workload had no readable spec.replicas") {
		t.Errorf("caveat must disclose what it could not read:\n%s", f.Caveat)
	}
}

func TestSingleReplica_NeedsRaw(t *testing.T) {
	bare := core.Asset{
		Provider: "kubernetes", AccountID: uzCluster, Type: hygieneDeploymentType,
		ID: "d1", Name: "api", Tags: map[string]string{"namespace": "prod"},
	}
	rep := Run(context.Background(), NewInput([]core.Asset{bare}, WithNow(fixedNow)),
		Options{Insights: []Insight{singleReplicaInsight{}}})
	if len(rep.Skipped) != 1 || !strings.Contains(rep.Skipped[0].Reason, "--include-raw") {
		t.Errorf("want a skip pointing at --include-raw, got %+v", rep.Skipped)
	}
}

// ----------------------------------------------------------------------
// the contract, over every insight in this file
// ----------------------------------------------------------------------

// hyEstate triggers every hygiene finding at once.
func hyEstate() []core.Asset {
	day := 24 * time.Hour
	return []core.Asset{
		hyAsset("oci", "oci.compute.instance", "owned", map[string]string{"owner": "payments"}),
		hyAsset("oci", "oci.compute.instance", "orphaned", nil),
		hyAged("oci.compute.instance", "brand-new", 2*day),
		hyAged("oci.vcn", "ancient", 4*365*day),
		hyCert("expiring", 5*day),
		hyCert("lapsed", -5*day),
		hyPod("open", "a", nil),
		hyWorkload(hygieneDeploymentType, "open", "api", 1, 1),
	}
}

func TestHygieneFindings_MeetTheContract(t *testing.T) {
	in := NewInput(hyEstate(), WithNow(fixedNow))
	seen := map[string]bool{}
	for _, ins := range hyInsights() {
		for i, f := range ins.Run(context.Background(), in) {
			f.Family = ins.Family()
			if err := ValidateFinding(f); err != nil {
				t.Errorf("%s finding %d does not meet the contract: %v", ins.ID(), i, err)
			}
			if f.Family != FamilyHygiene {
				t.Errorf("%s: family = %q", f.ID, f.Family)
			}
			// A caveat short enough to be generic is a disclaimer, and this
			// package already has one of those.
			if len(strings.Fields(f.Caveat)) < 12 {
				t.Errorf("%s: caveat says nothing specific: %q", f.ID, f.Caveat)
			}
			seen[f.ID] = true
		}
	}
	for _, want := range []string{
		"hygiene.unowned-resources",
		"hygiene.recently-created",
		"hygiene.ageing-resources",
		"hygiene.expiring-credentials",
		"hygiene.expired-credentials",
		"hygiene.namespaces-without-network-policy",
		"hygiene.single-replica-workloads",
	} {
		if !seen[want] {
			t.Errorf("the estate should have triggered %s", want)
		}
	}
}

func TestHygieneInsights_AreDeterministic(t *testing.T) {
	first := Run(context.Background(), NewInput(hyEstate(), WithNow(fixedNow)), Options{Insights: hyInsights()})

	shuffled := hyEstate()
	shuffled[0], shuffled[len(shuffled)-1] = shuffled[len(shuffled)-1], shuffled[0]
	second := Run(context.Background(), NewInput(shuffled, WithNow(fixedNow)), Options{Insights: hyInsights()})

	a, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("two runs over the same estate differ; the report is diffed, so the order is a contract")
	}
}

func TestHygieneInsights_DoNotMutateTheInput(t *testing.T) {
	in := NewInput(hyEstate(), WithNow(fixedNow))
	before := fmt.Sprint(in.Assets, in.ByType(hygienePodType), in.ByType(hygieneDeploymentType))
	for _, ins := range hyInsights() {
		ins.Run(context.Background(), in)
	}
	if after := fmt.Sprint(in.Assets, in.ByType(hygienePodType), in.ByType(hygieneDeploymentType)); after != before {
		t.Error("an insight mutated the shared input")
	}
}

func TestHygieneInsights_AreRegistered(t *testing.T) {
	for _, ins := range hyInsights() {
		got, ok := Lookup(ins.ID())
		if !ok {
			t.Errorf("%s is not registered", ins.ID())
			continue
		}
		if got.Family() != FamilyHygiene {
			t.Errorf("%s family = %q", ins.ID(), got.Family())
		}
		if err := Validate(got); err != nil {
			t.Errorf("%s: %v", ins.ID(), err)
		}
	}
}

// ----------------------------------------------------------------------
// both families, through the real runner
// ----------------------------------------------------------------------

// The framework refuses to publish a finding that does not meet the contract
// and reports the refusal as a bug in the insight. Nothing this pair of files
// produces may end up there, and every renderer has to accept all of it — the
// caveat is only enforcement if it survives to the surface a reader sees.
func TestUtilizationAndHygiene_NothingRefusedAndEveryFormatRenders(t *testing.T) {
	in := NewInput(append(uzEstate(), hyEstate()...), WithNow(fixedNow))
	rep := Run(context.Background(), in, Options{Insights: append(uzInsights(), hyInsights()...)})

	if len(rep.Suppressed) != 0 {
		t.Fatalf("the framework refused %d findings: %+v", len(rep.Suppressed), rep.Suppressed)
	}
	if len(rep.Findings) < 10 {
		t.Fatalf("the combined estate should trigger both families, got %v", uzIDs(rep.Findings))
	}
	for _, format := range []string{"table", "json", "markdown"} {
		var buf bytes.Buffer
		if err := Render(rep, format, &buf); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		// Every finding's caveat has to reach every surface, not just the JSON.
		for _, f := range rep.Findings {
			if !strings.Contains(buf.String(), f.Caveat[:40]) {
				t.Errorf("%s output does not carry %s's caveat", format, f.ID)
			}
		}
	}
}
