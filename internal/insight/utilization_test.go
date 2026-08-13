package insight

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Tests for the utilization insights.
//
// The assertions that matter here are not the arithmetic — they are the ones
// that keep the findings from claiming more than a single kubelet sample can
// support: that the sampling caveat is present and specific, that a pod with no
// reading is excluded rather than assumed idle, and that nothing outside
// Kubernetes is ever given a usage number.

// ----------------------------------------------------------------------
// fixtures
// ----------------------------------------------------------------------

const uzCluster = "prod-cluster"

// uzPod builds a Pod carrying one container with the given requests. An empty
// cpu or mem string omits that request, which is how a real spec expresses it.
func uzPod(ns, name, cpu, mem string) core.Asset {
	req := map[string]any{}
	if cpu != "" {
		req["cpu"] = cpu
	}
	if mem != "" {
		req["memory"] = mem
	}
	spec := map[string]any{
		"nodeName": "node-1",
		"containers": []any{map[string]any{
			"name":      "app",
			"resources": map[string]any{"requests": req},
		}},
	}
	return core.Asset{
		Provider: "kubernetes", AccountID: uzCluster, Type: utilPodType,
		ID: "uid-" + ns + "-" + name, Name: name, Status: "Running",
		Tags: map[string]string{"namespace": ns},
		Raw:  uzRaw(map[string]any{"spec": spec}),
	}
}

// uzPodMetrics builds the PodMetrics reading that belongs to a pod of the same
// (cluster, namespace, name) — the join this insight relies on.
func uzPodMetrics(ns, name, cpu, mem, window string) core.Asset {
	return core.Asset{
		Provider: "kubernetes", AccountID: uzCluster, Type: utilPodMetricsType,
		ID:   "k8s/metrics.k8s.io/v1beta1/PodMetrics/" + ns + "/" + name,
		Name: name,
		Tags: map[string]string{"namespace": ns},
		Raw: uzRaw(map[string]any{
			"window": window,
			"containers": []any{map[string]any{
				"name":  "app",
				"usage": map[string]any{"cpu": cpu, "memory": mem},
			}},
		}),
	}
}

func uzNode(name, allocCPU, allocMem string) core.Asset {
	return core.Asset{
		Provider: "kubernetes", AccountID: uzCluster, Type: utilNodeType,
		ID: "uid-" + name, Name: name, Status: "Ready",
		Raw: uzRaw(map[string]any{"status": map[string]any{
			"allocatable": map[string]any{"cpu": allocCPU, "memory": allocMem},
		}}),
	}
}

func uzNodeMetrics(name, cpu, mem string) core.Asset {
	return core.Asset{
		Provider: "kubernetes", AccountID: uzCluster, Type: utilNodeMetricsType,
		ID:   "k8s/metrics.k8s.io/v1beta1/NodeMetrics/" + name,
		Name: name,
		Raw:  uzRaw(map[string]any{"usage": map[string]any{"cpu": cpu, "memory": mem}}),
	}
}

func uzRaw(v map[string]any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// uzOn is the shorthand every test below uses: build the input at the fixed
// clock and run one insight over it.
func uzOn(tb testing.TB, ins Insight, assets ...core.Asset) []Finding {
	tb.Helper()
	return ins.Run(context.Background(), NewInput(assets, WithNow(fixedNow)))
}

// uzOne asserts a single finding came back and returns it.
func uzOne(tb testing.TB, fs []Finding) Finding {
	tb.Helper()
	if len(fs) != 1 {
		tb.Fatalf("want exactly 1 finding, got %d (%v)", len(fs), uzIDs(fs))
	}
	return fs[0]
}

func uzIDs(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

func uzRowLabels(f Finding) []string {
	out := make([]string, 0, len(f.Rows))
	for _, r := range f.Rows {
		out = append(out, r.Label)
	}
	return out
}

// uzInsights is every insight this file owns.
func uzInsights() []Insight {
	return []Insight{
		podRequestUsageInsight{}, nodeHeadroomInsight{},
		unrequestedWorkloadInsight{}, unmeasurableInsight{},
	}
}

// ----------------------------------------------------------------------
// requests vs usage
// ----------------------------------------------------------------------

func TestPodRequestsVsUsage_ReportsTheGapAndTheWindow(t *testing.T) {
	f := uzOne(t, uzOn(t, podRequestUsageInsight{},
		uzPod("prod", "batch", "8", "12Gi"),
		uzPodMetrics("prod", "batch", "50m", "1Gi", "30s"),
	))

	if f.Count != 1 {
		t.Errorf("count = %d, want 1", f.Count)
	}
	if len(f.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(f.Rows))
	}
	row := f.Rows[0]
	if row.Label != "prod/batch" {
		t.Errorf("label = %q, want the namespaced pod name", row.Label)
	}
	if row.Value != "7.95 CPU" {
		t.Errorf("value = %q, want the headroom 8 - 0.05", row.Value)
	}
	// The row has to carry both numbers and the window: a reader must be able
	// to re-take the measurement, which means knowing what was measured.
	for _, want := range []string{"req 8 CPU", "used 0.05 CPU", "over 30s"} {
		if !strings.Contains(row.Fact, want) {
			t.Errorf("fact %q is missing %q", row.Fact, want)
		}
	}
	if row.Asset == nil || row.Asset.ID != "uid-prod-batch" {
		t.Errorf("row must reference the pod, got %+v", row.Asset)
	}
}

// The house rule, on the finding this file exists for. A single kubelet sample
// is the whole evidentiary basis, and the caveat has to say so in terms a
// reader can act on rather than in a general disclaimer.
func TestPodRequestsVsUsage_CaveatNamesTheSampleHonestly(t *testing.T) {
	f := uzOne(t, uzOn(t, podRequestUsageInsight{},
		uzPod("prod", "batch", "8", "12Gi"),
		uzPodMetrics("prod", "batch", "50m", "1Gi", "30s"),
	))

	if err := ValidateFinding(f); err != nil {
		t.Fatalf("finding does not meet the contract: %v", err)
	}
	for _, want := range []string{
		"instantaneous",  // what the reading is
		"not an average", // what it is not
		"bursty",         // the workload it misreads
		"cron-driven",    // ... and the other one
		"invisible",      // the peak it cannot see
		"time series",    // where to go instead
		"reservation",    // requests are not a budget
	} {
		if !strings.Contains(f.Caveat, want) {
			t.Errorf("caveat is missing %q:\n%s", want, f.Caveat)
		}
	}
	if f.Severity != SeverityNotable {
		t.Errorf("severity = %q; one sample cannot support more than notable", f.Severity)
	}
}

// A pod with no reading is absent from the finding, and the caveat says how
// many such pods there were. Treating "no sample" as "no usage" is precisely
// the confident-wrong output this package refuses.
func TestPodRequestsVsUsage_UnsampledPodsAreExcludedAndCounted(t *testing.T) {
	f := uzOne(t, uzOn(t, podRequestUsageInsight{},
		uzPod("prod", "batch", "8", "12Gi"),
		uzPodMetrics("prod", "batch", "50m", "1Gi", "30s"),
		uzPod("prod", "quiet-but-unsampled", "8", "12Gi"),
	))

	if f.Count != 1 {
		t.Fatalf("count = %d, want only the sampled pod", f.Count)
	}
	if labels := uzRowLabels(f); strings.Contains(strings.Join(labels, ","), "unsampled") {
		t.Errorf("an unsampled pod must not appear as a finding row: %v", labels)
	}
	if !strings.Contains(f.Caveat, "1 of the 2 pods that set a request had no sample") {
		t.Errorf("caveat must count what it could not see:\n%s", f.Caveat)
	}
}

func TestPodRequestsVsUsage_CompletedPodsHoldNothing(t *testing.T) {
	done := uzPod("prod", "job-1", "8", "12Gi")
	done.Status = "Succeeded"

	if got := uzOn(t, podRequestUsageInsight{}, done, uzPodMetrics("prod", "job-1", "1m", "8Mi", "30s")); len(got) != 0 {
		t.Errorf("a Succeeded pod reserves nothing; got %v", uzIDs(got))
	}
}

// Small requests are arithmetic, not headroom: a container asking for 10m and
// sampling at 1m is a tenfold gap over nothing anybody can reclaim.
func TestPodRequestsVsUsage_TinyRequestsAreNotFindings(t *testing.T) {
	if got := uzOn(t, podRequestUsageInsight{},
		uzPod("prod", "sidecar", "10m", "16Mi"),
		uzPodMetrics("prod", "sidecar", "1m", "8Mi", "30s"),
	); len(got) != 0 {
		t.Errorf("want no finding below the floors; got %v", uzIDs(got))
	}
}

// A pod raised on memory alone must report memory headroom. Printing "0 CPU"
// in the measure column would read as a claim about a dimension nothing was
// raised about.
func TestPodRequestsVsUsage_MemoryOnlyCandidateReportsMemory(t *testing.T) {
	f := uzOne(t, uzOn(t, podRequestUsageInsight{},
		uzPod("prod", "cache", "100m", "8Gi"),
		uzPodMetrics("prod", "cache", "90m", "512Mi", "30s"),
	))
	row := f.Rows[0]
	if !strings.HasSuffix(row.Value, "Gi") {
		t.Errorf("value = %q, want the memory headroom", row.Value)
	}
	if strings.Contains(row.Fact, "req ") {
		t.Errorf("fact %q quotes CPU, but CPU was inside its ratio", row.Fact)
	}
}

func TestPodRequestsVsUsage_RowsRankByCPUHeadroom(t *testing.T) {
	f := uzOne(t, uzOn(t, podRequestUsageInsight{},
		uzPod("prod", "small", "1", "1Gi"), uzPodMetrics("prod", "small", "10m", "64Mi", "30s"),
		uzPod("prod", "large", "16", "1Gi"), uzPodMetrics("prod", "large", "10m", "64Mi", "30s"),
		uzPod("prod", "medium", "4", "1Gi"), uzPodMetrics("prod", "medium", "10m", "64Mi", "30s"),
	))
	want := []string{"prod/large", "prod/medium", "prod/small"}
	if got := uzRowLabels(f); !uzEqual(got, want) {
		t.Errorf("row order = %v, want %v (most reclaimable first)", got, want)
	}
}

// Requirements, not silence: a cluster with no metrics-server produces exactly
// the same empty result as a perfectly sized one, and the report has to
// distinguish them.
func TestPodRequestsVsUsage_SkippedWithoutMetrics(t *testing.T) {
	in := NewInput([]core.Asset{uzPod("prod", "batch", "8", "12Gi")}, WithNow(fixedNow))
	rep := Run(context.Background(), in, Options{Insights: []Insight{podRequestUsageInsight{}}})

	if len(rep.Skipped) != 1 {
		t.Fatalf("want the insight skipped, got skipped=%v findings=%v", rep.Skipped, uzIDs(rep.Findings))
	}
	if !strings.Contains(rep.Skipped[0].Reason, utilPodMetricsType) {
		t.Errorf("skip reason should name the missing type, got %q", rep.Skipped[0].Reason)
	}
}

func TestPodRequestsVsUsage_SkippedWithoutRaw(t *testing.T) {
	bare := core.Asset{
		Provider: "kubernetes", AccountID: uzCluster, Type: utilPodMetricsType,
		ID: "pm-1", Name: "batch", Tags: map[string]string{"namespace": "prod"},
	}
	rep := Run(context.Background(), NewInput([]core.Asset{bare}, WithNow(fixedNow)),
		Options{Insights: []Insight{podRequestUsageInsight{}}})

	if len(rep.Skipped) != 1 || !strings.Contains(rep.Skipped[0].Reason, "--include-raw") {
		t.Errorf("want a skip pointing at --include-raw, got %+v", rep.Skipped)
	}
}

// ----------------------------------------------------------------------
// quantity parsing
// ----------------------------------------------------------------------

// The reason utilQuantity does not use MilliValue. metrics-server reports CPU
// in nanocores, and MilliValue rounds up to a whole milli — so a container
// sampled at half a millicore comes back as 1m, double the truth, at exactly
// the scale these ratios are computed on.
func TestUtilQuantity_NanocoresKeepTheirPrecision(t *testing.T) {
	got, ok := utilQuantity("500000n")
	if !ok {
		t.Fatal("500000n must parse")
	}
	if got != 0.0005 {
		t.Errorf("500000n = %v cores, want 0.0005 (MilliValue would round to 0.001)", got)
	}
}

func TestUtilQuantity_Forms(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want float64
	}{
		{"8", 8},
		{"500m", 0.5},
		{"12Gi", 12 * 1024 * 1024 * 1024},
		{"1500Mi", 1500 * 1024 * 1024},
		{float64(2), 2},
		{int64(3), 3},
	} {
		got, ok := utilQuantity(tc.in)
		if !ok || got != tc.want {
			t.Errorf("utilQuantity(%v) = %v, %v; want %v, true", tc.in, got, ok, tc.want)
		}
	}
	for _, bad := range []any{"", "not-a-quantity", nil, true} {
		if _, ok := utilQuantity(bad); ok {
			t.Errorf("utilQuantity(%v) parsed, want a refusal", bad)
		}
	}
}

// ----------------------------------------------------------------------
// node headroom
// ----------------------------------------------------------------------

func TestNodeHeadroom_RatiosPerCluster(t *testing.T) {
	f := uzOne(t, uzOn(t, nodeHeadroomInsight{},
		uzNode("node-1", "4", "16Gi"),
		uzPod("prod", "a", "1", "4Gi"),
		uzPod("prod", "b", "1", "4Gi"),
	))

	if f.Count != 1 {
		t.Errorf("count = %d, want one cluster", f.Count)
	}
	if len(f.Rows) != 2 {
		t.Fatalf("want a CPU and a memory row, got %v", uzRowLabels(f))
	}
	if f.Rows[0].Value != "50%" || f.Rows[1].Value != "50%" {
		t.Errorf("ratios = %q/%q, want 50%% of 4 cores and 16Gi", f.Rows[0].Value, f.Rows[1].Value)
	}
	if !strings.Contains(f.Rows[0].Fact, "2 of 4 cores requested by 2 pods") {
		t.Errorf("CPU fact = %q, want both sides of the ratio", f.Rows[0].Fact)
	}
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %q, want info for a cluster in its normal range", f.Severity)
	}
	if strings.Contains(f.Caveat, "sampled-use") {
		t.Error("no NodeMetrics were supplied; the caveat must not claim a measurement")
	}
}

// The one line in this file's output derived from a measurement has to carry
// the sampling caveat with it.
func TestNodeHeadroom_SampleRowCarriesTheSampleCaveat(t *testing.T) {
	f := uzOne(t, uzOn(t, nodeHeadroomInsight{},
		uzNode("node-1", "4", "16Gi"),
		uzPod("prod", "a", "1", "4Gi"),
		uzNodeMetrics("node-1", "400m", "2Gi"),
	))

	var (
		sample Row
		found  bool
	)
	for _, r := range f.Rows {
		if strings.HasSuffix(r.Label, "sampled use") {
			sample, found = r, true
		}
	}
	if !found {
		t.Fatalf("want a sampled-use row, got %v", uzRowLabels(f))
	}
	if sample.Value != "10%" {
		t.Errorf("sampled use = %q, want 0.4 of 4 cores", sample.Value)
	}
	if !strings.Contains(f.Caveat, "instantaneous") {
		t.Errorf("a finding carrying a measurement must carry the sampling caveat:\n%s", f.Caveat)
	}
	if !strings.Contains(f.Basis, utilNodeMetricsType) {
		t.Errorf("basis must name the metrics type it read: %q", f.Basis)
	}
}

// Pods with no requests are the term that turns the ratio into a floor, so the
// caveat has to name how many of them there were rather than hedge in general.
func TestNodeHeadroom_UnrequestedPodsAreNamedInTheCaveat(t *testing.T) {
	f := uzOne(t, uzOn(t, nodeHeadroomInsight{},
		uzNode("node-1", "4", "16Gi"),
		uzPod("prod", "sized", "1", "4Gi"),
		uzPod("prod", "unbounded", "", ""),
	))
	if !strings.Contains(f.Caveat, "1 pod here sets no requests at all") {
		t.Errorf("caveat must count the pods missing from the requested side:\n%s", f.Caveat)
	}

	clean := uzOne(t, uzOn(t, nodeHeadroomInsight{},
		uzNode("node-1", "4", "16Gi"), uzPod("prod", "sized", "1", "4Gi")))
	if strings.Contains(clean.Caveat, "lower bound") {
		t.Errorf("with every pod sized, the lower-bound clause qualifies nothing:\n%s", clean.Caveat)
	}
}

// An unreadable node is excluded from both sides. Counting its pods' requests
// against the readable nodes' capacity would report a half-observed cluster as
// full.
func TestNodeHeadroom_UnreadableNodesLeaveBothSides(t *testing.T) {
	blind := core.Asset{
		Provider: "kubernetes", AccountID: uzCluster, Type: utilNodeType,
		ID: "uid-node-2", Name: "node-2", Raw: uzRaw(map[string]any{"status": map[string]any{}}),
	}
	onBlind := uzPod("prod", "elsewhere", "3", "8Gi")
	onBlind.Raw = uzRaw(map[string]any{"spec": map[string]any{
		"nodeName":   "node-2",
		"containers": []any{map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": "3"}}}},
	}})

	f := uzOne(t, uzOn(t, nodeHeadroomInsight{},
		uzNode("node-1", "4", "16Gi"), blind, uzPod("prod", "a", "1", "4Gi"), onBlind,
	))

	if f.Rows[0].Value != "25%" {
		t.Errorf("CPU = %q; the pod on the unreadable node must not be counted", f.Rows[0].Value)
	}
	if !strings.Contains(f.Rows[1].Fact, "1 node read of 2") {
		t.Errorf("the row must say how much of the cluster was read: %q", f.Rows[1].Fact)
	}
	if !strings.Contains(f.Caveat, "could not be read at all") {
		t.Errorf("caveat must disclose the excluded nodes:\n%s", f.Caveat)
	}
}

func TestNodeHeadroom_ExtremesAreNotableNotWarnings(t *testing.T) {
	packed := uzOne(t, uzOn(t, nodeHeadroomInsight{},
		uzNode("node-1", "4", "16Gi"),
		uzPod("prod", "a", "3900m", "15Gi"),
	))
	if packed.Severity != SeverityNotable {
		t.Errorf("a cluster at 97%% requested = %q, want notable", packed.Severity)
	}

	slack := uzOne(t, uzOn(t, nodeHeadroomInsight{},
		uzNode("node-1", "64", "256Gi"),
		uzPod("prod", "a", "1", "1Gi"),
	))
	if slack.Severity != SeverityNotable {
		t.Errorf("a cluster at 2%% requested = %q, want notable — the other end of the same range", slack.Severity)
	}
}

func TestNodeHeadroom_NoReadableSupplyIsNoFinding(t *testing.T) {
	blind := core.Asset{
		Provider: "kubernetes", AccountID: uzCluster, Type: utilNodeType,
		ID: "n", Name: "node-1", Raw: uzRaw(map[string]any{"status": map[string]any{}}),
	}
	if got := uzOn(t, nodeHeadroomInsight{}, blind, uzPod("prod", "a", "1", "1Gi")); len(got) != 0 {
		t.Errorf("an unknown denominator is not a 0%% cluster; got %v", uzIDs(got))
	}
}

// ----------------------------------------------------------------------
// no requests at all
// ----------------------------------------------------------------------

func TestWorkloadsWithoutRequests_GroupedByNamespace(t *testing.T) {
	f := uzOne(t, uzOn(t, unrequestedWorkloadInsight{},
		uzPod("prod", "a", "", ""),
		uzPod("prod", "b", "", ""),
		uzPod("staging", "c", "", ""),
		uzPod("prod", "sized", "500m", "1Gi"),
	))

	if f.Count != 3 {
		t.Errorf("count = %d, want the three unrequested pods", f.Count)
	}
	if f.Severity != SeverityWarn {
		t.Errorf("severity = %q, want warn", f.Severity)
	}
	want := []string{uzCluster + "/prod", uzCluster + "/staging"}
	if got := uzRowLabels(f); !uzEqual(got, want) {
		t.Errorf("rows = %v, want %v (busiest namespace first)", got, want)
	}
	if f.Rows[0].Value != "2 pods" {
		t.Errorf("value = %q, want the namespace's count", f.Rows[0].Value)
	}
	// The ids belong in the JSON so a consumer can act on them without
	// parsing prose out of the row.
	if len(f.Rows[0].Related) != 2 {
		t.Errorf("want the example pod refs on the row, got %d", len(f.Rows[0].Related))
	}
}

// A pod where only some containers set requests is a weaker version of this
// finding: it is counted in the caveat rather than listed as if it were the
// same thing.
func TestWorkloadsWithoutRequests_PartialPodsAreCountedNotListed(t *testing.T) {
	partial := uzPod("prod", "partial", "500m", "")
	partial.Raw = uzRaw(map[string]any{"spec": map[string]any{
		"containers": []any{
			map[string]any{"name": "app", "resources": map[string]any{"requests": map[string]any{"cpu": "500m"}}},
			map[string]any{"name": "sidecar"},
		},
	}})

	f := uzOne(t, uzOn(t, unrequestedWorkloadInsight{}, uzPod("prod", "none", "", ""), partial))
	if f.Count != 1 {
		t.Errorf("count = %d, want only the fully unrequested pod", f.Count)
	}
	if !strings.Contains(f.Caveat, "A further 1 pod sets requests on some containers but not all") {
		t.Errorf("caveat must account for the partial pod:\n%s", f.Caveat)
	}
}

// The caveat must not read as "these are wasting resources" — the actual
// consequence is different and this tool cannot see consumption either way.
func TestWorkloadsWithoutRequests_CaveatNamesTheRealConsequence(t *testing.T) {
	f := uzOne(t, uzOn(t, unrequestedWorkloadInsight{}, uzPod("prod", "a", "", "")))
	for _, want := range []string{"BestEffort", "evicted", "cannot see what any of them actually consume", "meant to be this way"} {
		if !strings.Contains(f.Caveat, want) {
			t.Errorf("caveat missing %q:\n%s", want, f.Caveat)
		}
	}
}

func TestWorkloadsWithoutRequests_UnparseableSpecIsNotCounted(t *testing.T) {
	broken := uzPod("prod", "broken", "", "")
	broken.Raw = json.RawMessage(`{"spec":{}}`)
	if got := uzOn(t, unrequestedWorkloadInsight{}, broken); len(got) != 0 {
		t.Errorf("an unreadable spec is not evidence of a missing request; got %v", uzIDs(got))
	}
}

// ----------------------------------------------------------------------
// everywhere that is not Kubernetes
// ----------------------------------------------------------------------

func TestNoUsageData_CountsPerProviderAndNamesTheAPI(t *testing.T) {
	f := uzOne(t, uzOn(t, unmeasurableInsight{},
		core.Asset{Provider: "oci", Type: "oci.compute.instance", ID: "i1", Name: "web-1"},
		core.Asset{Provider: "oci", Type: "oci.block_volume", ID: "v1", Name: "data"},
		core.Asset{Provider: "gcp", Type: "compute.googleapis.com/Instance", ID: "g1", Name: "gce-1"},
		core.Asset{Provider: "oci", Type: "oci.iam.user", ID: "u1", Name: "alice"},
	))

	if f.Count != 3 {
		t.Errorf("count = %d, want the three capacity-bearing assets (not the IAM user)", f.Count)
	}
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %q; nothing is wrong here", f.Severity)
	}
	if got, want := uzRowLabels(f), []string{"oci", "gcp"}; !uzEqual(got, want) {
		t.Errorf("rows = %v, want %v in table order", got, want)
	}
	if !strings.Contains(f.Rows[0].Fact, "OCI Monitoring") {
		t.Errorf("the row must name the API that would answer it, got %q", f.Rows[0].Fact)
	}
	if !strings.Contains(f.Rows[1].Fact, "Cloud Monitoring") {
		t.Errorf("the GCP row must name Cloud Monitoring, got %q", f.Rows[1].Fact)
	}
	// The whole point: it must not be readable as "these are fine".
	for _, want := range []string{"asserts an absence in this tool", "Do not infer sizing from instance shape"} {
		if !strings.Contains(f.Caveat, want) {
			t.Errorf("caveat missing %q:\n%s", want, f.Caveat)
		}
	}
}

func TestNoUsageData_KubernetesOnlyAuditSaysNothing(t *testing.T) {
	if got := uzOn(t, unmeasurableInsight{}, uzPod("prod", "a", "1", "1Gi")); len(got) != 0 {
		t.Errorf("Kubernetes is the exception, not a subject of this finding; got %v", uzIDs(got))
	}
}

// It has no Requirements on purpose: this finding is at its most useful in the
// run where every other one in the file was skipped.
func TestNoUsageData_RunsWithoutRawOrMetrics(t *testing.T) {
	in := NewInput([]core.Asset{{Provider: "oci", Type: "oci.compute.instance", ID: "i1", Name: "web"}},
		WithNow(fixedNow))
	rep := Run(context.Background(), in, Options{Insights: uzInsights()})

	if len(rep.Findings) != 1 || rep.Findings[0].ID != "utilization.no-usage-data" {
		t.Fatalf("want only the no-usage-data finding, got %v", uzIDs(rep.Findings))
	}
	if len(rep.Skipped) != 3 {
		t.Errorf("want the three Kubernetes insights skipped and said so, got %d", len(rep.Skipped))
	}
}

// ----------------------------------------------------------------------
// the contract, over every insight in this file
// ----------------------------------------------------------------------

// uzEstate is an inventory that triggers every utilization finding at once, so
// the contract test below exercises real findings rather than empty slices.
func uzEstate() []core.Asset {
	return []core.Asset{
		uzNode("node-1", "8", "32Gi"),
		uzNodeMetrics("node-1", "500m", "4Gi"),
		uzPod("prod", "over-requested", "6", "16Gi"),
		uzPodMetrics("prod", "over-requested", "50m", "1Gi", "30s"),
		uzPod("prod", "unbounded", "", ""),
		uzPod("staging", "unbounded-2", "", ""),
		{Provider: "oci", Type: "oci.compute.instance", ID: "i1", Name: "web-1"},
		{Provider: "gcp", Type: "compute.googleapis.com/Instance", ID: "g1", Name: "gce-1"},
	}
}

func TestUtilizationFindings_MeetTheContract(t *testing.T) {
	in := NewInput(uzEstate(), WithNow(fixedNow))
	seen := 0
	for _, ins := range uzInsights() {
		for i, f := range ins.Run(context.Background(), in) {
			seen++
			f.Family = ins.Family()
			if err := ValidateFinding(f); err != nil {
				t.Errorf("%s finding %d does not meet the contract: %v", ins.ID(), i, err)
			}
			// A caveat that could be pasted onto any other finding is a
			// disclaimer, not a caveat. Every one of these has to be about
			// what *this* finding cannot know.
			if len(strings.Fields(f.Caveat)) < 12 {
				t.Errorf("%s: caveat is too short to say anything specific: %q", f.ID, f.Caveat)
			}
			if f.Total != nil {
				t.Errorf("%s: utilization findings carry no money — a pod's cost belongs to its node", f.ID)
			}
		}
	}
	if seen != 4 {
		t.Fatalf("the estate should trigger all four findings, got %d", seen)
	}
}

func TestUtilizationInsights_AreDeterministic(t *testing.T) {
	first := Run(context.Background(), NewInput(uzEstate(), WithNow(fixedNow)), Options{Insights: uzInsights()})

	shuffled := uzEstate()
	shuffled[0], shuffled[len(shuffled)-1] = shuffled[len(shuffled)-1], shuffled[0]
	second := Run(context.Background(), NewInput(shuffled, WithNow(fixedNow)), Options{Insights: uzInsights()})

	a, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("two runs over the same estate differ; this report is diffed, so the order is a contract")
	}
}

// The Input is shared by every insight in a run. Sorting or appending to one of
// its index buckets would corrupt whichever insight ran next.
func TestUtilizationInsights_DoNotMutateTheInput(t *testing.T) {
	in := NewInput(uzEstate(), WithNow(fixedNow))
	before := fmt.Sprint(in.Assets, in.ByType(utilPodType), in.ByType(utilNodeType))
	for _, ins := range uzInsights() {
		ins.Run(context.Background(), in)
	}
	if after := fmt.Sprint(in.Assets, in.ByType(utilPodType), in.ByType(utilNodeType)); after != before {
		t.Error("an insight mutated the shared input")
	}
}

func TestUtilizationInsights_AreRegistered(t *testing.T) {
	for _, ins := range uzInsights() {
		got, ok := Lookup(ins.ID())
		if !ok {
			t.Errorf("%s is not registered", ins.ID())
			continue
		}
		if got.Family() != familyUtilization {
			t.Errorf("%s family = %q, want %q", ins.ID(), got.Family(), familyUtilization)
		}
		if err := Validate(got); err != nil {
			t.Errorf("%s: %v", ins.ID(), err)
		}
	}
}

// ----------------------------------------------------------------------
// formatting helpers
// ----------------------------------------------------------------------

func TestUtilFormatting(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{utilCoreText(8), "8 CPU"},
		{utilCoreText(0.05), "0.05 CPU"},
		{utilCoreText(1.23456), "1.235 CPU"},
		{utilByteText(12 * 1024 * 1024 * 1024), "12Gi"},
		{utilByteText(512 * 1024 * 1024), "512Mi"},
		{utilNum(0, 2), "0"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
	// A percentage of an unknown denominator is 0, not a panic and not a
	// fabricated number.
	if got := utilPct(5, 0); got != 0 {
		t.Errorf("utilPct(5, 0) = %d, want 0", got)
	}
}

func uzEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
