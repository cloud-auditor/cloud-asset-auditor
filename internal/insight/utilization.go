package insight

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Utilization insights.
//
// This is the one file in the package where a number about *behaviour* is
// allowed to appear, and the licence for it is narrow. A Kubernetes cluster
// serves metrics.k8s.io PodMetrics and NodeMetrics, this tool already collects
// them like any other API object, and a Pod's spec carries resources.requests.
// So for Kubernetes — and only for Kubernetes — what a workload asked for and
// what it was using are both in the inventory and can be put side by side.
//
// Everything about that comparison is weaker than it looks, and the weakness is
// structural rather than a gap in this implementation. PodMetrics is the
// kubelet's most recent sample at the instant the audit ran: not an average,
// not a maximum, and not a window anybody chose. A cron job that runs between
// audits, a service that peaks at 09:00, a container still warming up — all of
// them read as idle. So every finding here carries that sentence in its Caveat,
// next to the number rather than under it, and the thresholds below are set so
// a finding is about headroom somebody could act on rather than about
// arithmetic performed on rounding error.
//
// For every other provider the honest answer is that utilisation is not
// knowable from an inventory at all. unmeasurableInsight says exactly that, with
// counts and with the name of the API that would answer it, because silence
// here would be read as "nothing to find" — and the one thing this package must
// never do is let an absence of data pass for a measurement.

// familyUtilization is a section of its own rather than a reuse of FamilyCost.
// These findings carry no money: a pod's cost is attributed to its node, and a
// requests-vs-usage gap is a sizing question that may or may not turn into a
// bill. Filing them under "cost" would put a section header over numbers that
// are not currency.
const familyUtilization Family = "utilization"

// The Kubernetes types this file joins. PodMetrics and NodeMetrics are computed
// resources with no stored UID, so the collector gives them a synthetic
// k8s/<apiVersion>/<Kind>/<ns>/<name> id — which is why everything below joins
// on (cluster, namespace, name) rather than on an id.
const (
	utilPodType         = "v1.Pod"
	utilNodeType        = "v1.Node"
	utilPodMetricsType  = "metrics.k8s.io/v1beta1.PodMetrics"
	utilNodeMetricsType = "metrics.k8s.io/v1beta1.NodeMetrics"
)

// Thresholds for "far below what it requested".
//
// Both floors exist to keep the finding about reclaimable headroom. A container
// requesting 10m and sampling at 1m is a tenfold gap over nine thousandths of a
// core: arithmetically true, and worth nobody's afternoon.
//
// The two ratios differ on purpose. A CPU sample is the weakest reading in this
// file — CPU is precisely the resource that bursts and idles between scrapes —
// so it takes a full order of magnitude before a single reading is worth
// raising. Memory is steadier: resident set size does not swing the way CPU
// does, so a memory sample well under the request is more likely to be the
// steady state, and a laxer bar is defensible. It is not proof: a runtime with
// a large configured heap grows into its request over hours, and one reading
// taken early in that curve looks like waste.
const (
	utilCPUFloorCores = 0.1
	utilCPURatio      = 0.10
	utilMemFloorBytes = 512 << 20 // 512Mi
	utilMemRatio      = 0.25
)

// utilSampleCaveat is the sentence this whole file is built around, and it is
// deliberately one constant reused verbatim by every finding derived from
// metrics.k8s.io. Rewording it per finding would produce four caveats of
// drifting strength, and the weakest one would end up on the finding somebody
// screenshots.
const utilSampleCaveat = "metrics.k8s.io serves a single instantaneous reading — the kubelet's most " +
	"recent sample at the moment this audit ran — not an average, a maximum, or a window anyone " +
	"chose. A workload that is bursty, cron-driven, still starting up, or merely quiet at that " +
	"instant is indistinguishable here from one that is permanently over-requested, and a peak a " +
	"minute earlier is invisible. This is a pointer for a time series (Prometheus, kubectl top over " +
	"a window) to confirm or refute, not a measurement of the workload."

func init() {
	Register(podRequestUsageInsight{})
	Register(nodeHeadroomInsight{})
	Register(unrequestedWorkloadInsight{})
	Register(unmeasurableInsight{})
}

// ----------------------------------------------------------------------
// requests vs sampled usage
// ----------------------------------------------------------------------

type podRequestUsageInsight struct{}

func (podRequestUsageInsight) ID() string { return "utilization.pod-requests-vs-usage" }

func (podRequestUsageInsight) Title() string {
	return "Pods whose sampled usage is far below what they reserve"
}

func (podRequestUsageInsight) Family() Family { return familyUtilization }

// Requires PodMetrics by type rather than treating its absence as "nothing
// found". A cluster with no metrics-server produces exactly the same empty
// result as a perfectly sized one, and the NOT RUN row is the difference.
func (podRequestUsageInsight) Requires() Requirements {
	return Requirements{Raw: true, Types: []string{utilPodMetricsType}}
}

func (podRequestUsageInsight) Run(_ context.Context, in *Input) []Finding {
	samples := utilIndexPodMetrics(in)
	if len(samples) == 0 {
		return nil
	}

	type candidate struct {
		key      string
		pod      core.Asset
		req, use utilResources
		window   string
		cpu, mem bool // which dimension raised it
		headroom float64
	}
	var (
		cands   []candidate
		scanned int // pods that set a request and are still holding capacity
		sampled int // ... of which a metrics reading was found
	)
	for _, pod := range in.ByType(utilPodType) {
		if !utilHoldsCapacity(pod) {
			continue
		}
		req, ok := utilPodRequests(in, pod)
		if !ok || (req.cpu <= 0 && req.mem <= 0) {
			continue // no requests set at all — that is unrequestedWorkloadInsight's question
		}
		scanned++
		s, ok := samples[utilPodKey(pod)]
		if !ok {
			continue
		}
		sampled++

		c := candidate{key: utilPodKey(pod), pod: pod, req: req, use: s.use, window: s.window}
		c.cpu = req.cpu >= utilCPUFloorCores && s.use.cpu <= req.cpu*utilCPURatio
		c.mem = req.mem >= utilMemFloorBytes && s.use.mem <= req.mem*utilMemRatio
		if !c.cpu && !c.mem {
			continue
		}
		// Rank on CPU headroom because cores are the scarce, schedulable thing
		// on almost every cluster; memory-only candidates fall to the bottom
		// with a zero here, which is the right place for the weaker evidence.
		if c.cpu {
			c.headroom = req.cpu - s.use.cpu
		}
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		return nil
	}

	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].headroom != cands[j].headroom {
			return cands[i].headroom > cands[j].headroom
		}
		return cands[i].key < cands[j].key
	})

	rows := make([]Row, 0, len(cands))
	var totalCPU float64
	for _, c := range cands {
		totalCPU += c.headroom
		row := AssetRow(c.pod, utilGapFact(c.req, c.use, c.window, c.cpu, c.mem))
		row.Label = utilPodLabel(c.pod)
		row.Value = utilCoreText(c.headroom)
		if !c.cpu {
			// A memory-only candidate has no CPU headroom to quote, and
			// printing "0 CPU" in the measure column would read as a claim
			// that the pod is idle on a dimension nothing was raised about.
			row.Value = utilByteText(c.req.mem - c.use.mem)
		}
		rows = append(rows, row)
	}

	caveat := utilSampleCaveat + " A request is a reservation, not a budget: a pod under its request " +
		"is behaving exactly as configured, and lowering it trades scheduling headroom for density."
	if unsampled := scanned - sampled; unsampled > 0 {
		// Only when there were any. "0 pods had no sample" is a sentence that
		// makes a reader stop and work out whether it matters.
		caveat += fmt.Sprintf(" %s of the %s pods that set a request had no sample in this audit and "+
			"%s absent from this list entirely.", formatInt(unsampled), formatInt(scanned),
			pluralVerb(unsampled, "is", "are"))
	}

	return []Finding{{
		ID:    "utilization.pod-requests-vs-usage",
		Title: "Pods whose sampled usage is far below what they reserve",
		Summary: fmt.Sprintf(
			"%s of %s pods with both a request and a metrics sample %s using under %d%% of %s CPU "+
				"request (or under %d%% of memory) in the one reading this audit took; the CPU gap "+
				"adds up to %s cores.",
			formatInt(len(cands)), formatInt(sampled), pluralVerb(len(cands), "was", "were"),
			int(utilCPURatio*100), pluralVerb(len(cands), "its", "their"), int(utilMemRatio*100),
			utilNum(totalCPU, 2)),
		Severity: SeverityNotable,
		Count:    len(cands),
		Basis: fmt.Sprintf(
			"v1.Pod spec.containers[].resources.requests summed per pod and joined to %s "+
				"containers[].usage on (cluster, namespace, name); both read from Asset.Raw. Pods in "+
				"Succeeded or Failed are excluded because their requests are no longer reserved. "+
				"Raised at CPU request >= %s cores with usage <= %d%% of it, or memory request >= %s "+
				"with usage <= %d%% of it.",
			utilPodMetricsType, utilNum(utilCPUFloorCores, 2), int(utilCPURatio*100),
			utilByteText(utilMemFloorBytes), int(utilMemRatio*100)),
		Caveat: caveat,
		Rows:   rows,
	}}
}

// utilGapFact is the row's checkable observation: the two numbers and the
// window they were taken over, so a reader can go and re-take the measurement.
func utilGapFact(req, use utilResources, window string, cpu, mem bool) string {
	var parts []string
	if cpu {
		parts = append(parts, fmt.Sprintf("req %s, used %s", utilCoreText(req.cpu), utilCoreText(use.cpu)))
	}
	if mem {
		parts = append(parts, fmt.Sprintf("mem %s, used %s", utilByteText(req.mem), utilByteText(use.mem)))
	}
	if window != "" {
		parts = append(parts, "over "+window)
	}
	return strings.Join(parts, "; ")
}

// ----------------------------------------------------------------------
// node headroom
// ----------------------------------------------------------------------

// Escalation thresholds for the cluster ratio. Both directions are worth a
// second glance and neither is a defect: a cluster whose requests nearly fill
// allocatable has no room for the next rollout, and one at a fifth of it is
// paying for nodes the scheduler has nothing to put on. Between the two is
// simply a working cluster, and this finding stays SeverityInfo there.
const (
	utilPackedRatio = 0.90
	utilSlackRatio  = 0.20
)

type nodeHeadroomInsight struct{}

func (nodeHeadroomInsight) ID() string { return "utilization.node-headroom" }

func (nodeHeadroomInsight) Title() string { return "Node capacity reserved by pod requests" }

func (nodeHeadroomInsight) Family() Family { return familyUtilization }

func (nodeHeadroomInsight) Requires() Requirements {
	return Requirements{Raw: true, Types: []string{utilNodeType}}
}

// utilCluster accumulates one cluster's two sides of the ratio.
type utilCluster struct {
	name          string
	nodes         int
	nodesReadable int
	allocCPU      float64
	allocMem      float64

	pods          int
	podsNoRequest int
	reqCPU        float64
	reqMem        float64

	sampledNodes int
	useCPU       float64
	useMem       float64
}

func (nodeHeadroomInsight) Run(_ context.Context, in *Input) []Finding {
	clusters := map[string]*utilCluster{}
	get := func(account string) *utilCluster {
		c, ok := clusters[account]
		if !ok {
			c = &utilCluster{name: utilClusterName(account)}
			clusters[account] = c
		}
		return c
	}

	// Pass 1 — the supply side. A node whose status.allocatable cannot be read
	// contributes to neither side of the ratio: counting its pods' requests
	// against the remaining nodes' capacity would report a cluster as full when
	// it is only half-observed.
	readable := map[string]bool{}
	for _, n := range in.ByType(utilNodeType) {
		c := get(n.AccountID)
		c.nodes++
		cpu, cpuOK := utilRawQuantity(in, n, "status.allocatable.cpu")
		mem, memOK := utilRawQuantity(in, n, "status.allocatable.memory")
		if !cpuOK && !memOK {
			continue
		}
		c.nodesReadable++
		c.allocCPU += cpu
		c.allocMem += mem
		readable[n.AccountID+"\x00"+n.Name] = true
	}

	// Pass 2 — the demand side, restricted to pods actually placed on a node
	// this run could measure. An unscheduled pod reserves nothing yet.
	for _, p := range in.ByType(utilPodType) {
		if !utilHoldsCapacity(p) {
			continue
		}
		node, ok := in.RawString(p, "spec.nodeName")
		if !ok || !readable[p.AccountID+"\x00"+node] {
			continue
		}
		c := get(p.AccountID)
		c.pods++
		req, ok := utilPodRequests(in, p)
		if !ok || (req.cpu <= 0 && req.mem <= 0) {
			c.podsNoRequest++
			continue
		}
		c.reqCPU += req.cpu
		c.reqMem += req.mem
	}

	// Pass 3 — the sample, when the cluster serves one. This is the only line
	// in the finding that is about behaviour, and it is marked as such.
	for _, m := range in.ByType(utilNodeMetricsType) {
		if !readable[m.AccountID+"\x00"+m.Name] {
			continue
		}
		c := get(m.AccountID)
		cpu, cpuOK := utilRawQuantity(in, m, "usage.cpu")
		mem, memOK := utilRawQuantity(in, m, "usage.memory")
		if !cpuOK && !memOK {
			continue
		}
		c.sampledNodes++
		c.useCPU += cpu
		c.useMem += mem
	}

	// A cluster with nothing readable on the supply side is dropped rather than
	// reported as 0% — an unknown denominator is not an empty one.
	var names []string
	for k, c := range clusters {
		if c.nodesReadable == 0 || c.allocCPU <= 0 {
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil
	}

	var (
		rows       []Row
		sampled    bool
		extreme    bool
		lead       string
		leadPct    = -1
		unreadable bool
		unbounded  int // pods contributing nothing to the requested side
	)
	for _, k := range names {
		c := clusters[k]
		cpuPct := utilPct(c.reqCPU, c.allocCPU)
		memPct := utilPct(c.reqMem, c.allocMem)
		unbounded += c.podsNoRequest
		if c.nodes != c.nodesReadable {
			unreadable = true
		}
		if cpuPct > leadPct {
			leadPct, lead = cpuPct, c.name
		}
		if utilExtreme(c.reqCPU, c.allocCPU) || utilExtreme(c.reqMem, c.allocMem) {
			extreme = true
		}

		rows = append(rows,
			Row{
				Label: c.name + " · CPU",
				Value: fmt.Sprintf("%d%%", cpuPct),
				Fact: fmt.Sprintf("%s of %s cores requested by %s pods",
					utilNum(c.reqCPU, 1), utilNum(c.allocCPU, 1), formatInt(c.pods)),
			},
			Row{
				Label: c.name + " · memory",
				Value: fmt.Sprintf("%d%%", memPct),
				Fact: fmt.Sprintf("%s of %s requested; %s node%s read of %s",
					utilByteText(c.reqMem), utilByteText(c.allocMem),
					formatInt(c.nodesReadable), plural(c.nodesReadable), formatInt(c.nodes)),
			})
		if c.sampledNodes > 0 {
			sampled = true
			rows = append(rows, Row{
				Label: c.name + " · sampled use",
				Value: fmt.Sprintf("%d%%", utilPct(c.useCPU, c.allocCPU)),
				Fact: fmt.Sprintf("%s cores, %s in one reading of %s node%s",
					utilNum(c.useCPU, 1), utilByteText(c.useMem),
					formatInt(c.sampledNodes), plural(c.sampledNodes)),
			})
		}
	}

	severity := SeverityInfo
	if extreme {
		// Still not a warning. Both ends of this range are ordinary operating
		// choices; what changes is that they are worth a look, which is exactly
		// what SeverityNotable means.
		severity = SeverityNotable
	}

	caveat := "Allocatable is what the kubelet reports it can schedule and requests are what pods " +
		"reserved — both are declarations, and neither is consumption: a cluster at 30% requested may " +
		"be running at 90% CPU or at 2%. Pods this audit could not place on a readable node are " +
		"excluded from both sides."
	if unbounded > 0 {
		// Named with its number rather than as a general hedge, because this is
		// the one term that makes the ratio a floor instead of a measurement.
		caveat += fmt.Sprintf(" The requested figure is also a lower bound: %s pod%s here %s no "+
			"requests at all, so they occupy real capacity while contributing nothing to it.",
			formatInt(unbounded), plural(unbounded), pluralVerb(unbounded, "sets", "set"))
	}
	if unreadable {
		caveat += " Some nodes' status.allocatable could not be read at all and are excluded entirely, " +
			"so the totals here are smaller than the cluster."
	}
	if sampled {
		caveat += " The sampled-use row is the one line here derived from measurement, and " + utilSampleCaveat
	}

	basis := "v1.Node status.allocatable summed per cluster (Asset.AccountID) against the " +
		"spec.containers[].resources.requests of every v1.Pod whose spec.nodeName names one of those " +
		"nodes; all from Asset.Raw"
	if sampled {
		basis += ", plus " + utilNodeMetricsType + " usage.cpu/usage.memory where the cluster serves it"
	}

	summary := fmt.Sprintf("In %s, pod requests reserve %d%% of allocatable CPU.", lead, leadPct)
	if len(names) > 1 {
		summary = fmt.Sprintf("Across %s clusters, pod requests reserve up to %d%% of allocatable "+
			"CPU (%s).", formatInt(len(names)), leadPct, lead)
	}

	return []Finding{{
		ID:       "utilization.node-headroom",
		Title:    "Node capacity reserved by pod requests",
		Summary:  summary,
		Severity: severity,
		Count:    len(names),
		Basis:    basis,
		Caveat:   caveat,
		Rows:     rows,
	}}
}

// utilExtreme reports whether a ratio is far enough towards either end to be
// worth naming. Zero denominators are not extreme, they are unknown.
func utilExtreme(part, whole float64) bool {
	if whole <= 0 || part <= 0 {
		return false
	}
	r := part / whole
	return r >= utilPackedRatio || r <= utilSlackRatio
}

// ----------------------------------------------------------------------
// workloads with no requests at all
// ----------------------------------------------------------------------

// utilExampleRefs caps how many pod references a namespace row carries. The
// row's Value is the true count; these are there so a reader has somewhere to
// start, and forty ids in one row is not a start.
const utilExampleRefs = 5

type unrequestedWorkloadInsight struct{}

func (unrequestedWorkloadInsight) ID() string { return "utilization.workloads-without-requests" }

func (unrequestedWorkloadInsight) Title() string {
	return "Pods that set no CPU or memory request"
}

func (unrequestedWorkloadInsight) Family() Family { return familyUtilization }

func (unrequestedWorkloadInsight) Requires() Requirements {
	return Requirements{Raw: true, Types: []string{utilPodType}}
}

func (unrequestedWorkloadInsight) Run(_ context.Context, in *Input) []Finding {
	type nsGroup struct {
		label string
		pods  int
		refs  []core.AssetRef
	}
	var (
		groups  = map[string]*nsGroup{}
		order   []string
		total   int
		partial int
		scanned int
	)
	for _, p := range in.ByType(utilPodType) {
		if !utilHoldsCapacity(p) {
			continue
		}
		req, ok := utilPodRequests(in, p)
		if !ok {
			continue // spec unreadable — absence of evidence, not evidence of absence
		}
		scanned++
		switch {
		case req.cpu > 0 || req.mem > 0:
			// Some container asked for something. A pod where only *some*
			// containers set requests is a weaker version of this finding and
			// is counted rather than listed: the QoS class is still Burstable
			// and the scheduler still has a number to work with.
			if req.withRequests < req.containers {
				partial++
			}
			continue
		}
		total++
		key := p.AccountID + "\x00" + utilNamespace(p)
		g, ok := groups[key]
		if !ok {
			g = &nsGroup{label: utilClusterName(p.AccountID) + "/" + utilNamespace(p)}
			groups[key] = g
			order = append(order, key)
		}
		g.pods++
		if len(g.refs) < utilExampleRefs {
			g.refs = append(g.refs, p.AsRef())
		}
	}
	if total == 0 {
		return nil
	}

	sort.SliceStable(order, func(i, j int) bool {
		a, b := groups[order[i]], groups[order[j]]
		if a.pods != b.pods {
			return a.pods > b.pods
		}
		return a.label < b.label
	})
	rows := make([]Row, 0, len(order))
	for _, k := range order {
		g := groups[k]
		rows = append(rows, Row{
			Label: g.label,
			Value: fmt.Sprintf("%s pod%s", formatInt(g.pods), plural(g.pods)),
			// Short on purpose: the renderer appends "(+N related)" to this
			// cell and truncates at 44 columns, and the related marker is the
			// affordance that sends a reader to the ids in the JSON.
			Fact:    "no CPU or memory request set",
			Related: g.refs,
		})
	}

	caveat := "A pod with no requests is not thereby idle or wasteful — it is unscheduled-for, which " +
		"is a different problem: the scheduler places it on capacity it never reserved, it lands in " +
		"the BestEffort QoS class and is evicted first under pressure, and no requests-based cost " +
		"model (this tool's included) can attribute a cent of node cost to it. This inventory cannot " +
		"see what any of them actually consume, and some of them — short-lived Jobs, one-shot debug " +
		"pods, DaemonSets a platform team deliberately leaves unbounded — are meant to be this way."
	if partial > 0 {
		caveat += fmt.Sprintf(" A further %s pod%s %s requests on some containers but not all; those "+
			"are counted here only in this sentence.", formatInt(partial), plural(partial),
			pluralVerb(partial, "sets", "set"))
	}

	return []Finding{{
		ID:    "utilization.workloads-without-requests",
		Title: "Pods that set no CPU or memory request",
		Summary: fmt.Sprintf("%s of %s running pods %s no CPU or memory request on any container, "+
			"across %s namespace%s.", formatInt(total), formatInt(scanned),
			pluralVerb(total, "sets", "set"), formatInt(len(order)), plural(len(order))),
		// Cheap to check and rarely deliberate at scale — the definition of a
		// warning. It is also the one finding in this file that needs no metrics
		// pipeline to act on, because the gap is in the declaration itself.
		Severity: SeverityWarn,
		Count:    total,
		Basis: "v1.Pod spec.containers[].resources.requests read from Asset.Raw, grouped by " +
			"(cluster, namespace); pods in Succeeded or Failed are excluded, and a pod whose spec " +
			"could not be parsed is not counted either way",
		Caveat: caveat,
		Rows:   rows,
	}}
}

// ----------------------------------------------------------------------
// everywhere that is not Kubernetes
// ----------------------------------------------------------------------

// utilUnmeasurable names, per provider, the resource types a reader will ask a
// utilisation question about and the service that could actually answer it.
//
// A table rather than a heuristic on the type string, because the useful half
// is the second column: which API to go and query is provider knowledge that
// cannot be derived from "oci.compute.instance". The list not being exhaustive
// does not weaken the finding — the point is the absence of data, and that
// absence covers every type, listed or not.
var utilUnmeasurable = []struct {
	provider string
	source   string
	types    []string
}{
	{
		provider: "oci",
		source:   "OCI Monitoring (oci_computeagent)",
		types: []string{
			"oci.compute.instance", "oci.container_instance", "oci.block_volume", "oci.boot_volume",
			"oci.autonomous_database", "oci.db_system", "oci.load_balancer",
			"oci.network_load_balancer", "oci.object_storage.bucket", "oci.oke.cluster",
			"oci.functions.function",
		},
	},
	{
		provider: "gcp",
		source:   "Cloud Monitoring (instance/cpu/utilization)",
		types: []string{
			"compute.googleapis.com/Instance", "compute.googleapis.com/Disk",
			"sqladmin.googleapis.com/Instance", "container.googleapis.com/Cluster",
			"run.googleapis.com/Service", "cloudfunctions.googleapis.com/CloudFunction",
			"storage.googleapis.com/Bucket",
		},
	},
	{
		provider: "cloudflare",
		source:   "the Cloudflare GraphQL Analytics API",
		types: []string{
			"cloudflare.worker_script", "cloudflare.r2_bucket", "cloudflare.kv_namespace",
			"cloudflare.d1_database", "cloudflare.pages_project", "cloudflare.load_balancer",
			"cloudflare.zone",
		},
	},
}

type unmeasurableInsight struct{}

func (unmeasurableInsight) ID() string { return "utilization.no-usage-data" }

func (unmeasurableInsight) Title() string {
	return "Resources whose utilisation this tool cannot determine"
}

func (unmeasurableInsight) Family() Family { return familyUtilization }

// No Requirements on purpose. This finding is at its most useful in exactly the
// run where everything else in the file was skipped.
func (unmeasurableInsight) Run(_ context.Context, in *Input) []Finding {
	var (
		rows  []Row
		total int
	)
	for _, p := range utilUnmeasurable {
		n := 0
		for _, t := range p.types {
			n += in.Count(t)
		}
		if n == 0 {
			continue
		}
		total += n
		rows = append(rows, Row{
			Label: p.provider,
			Value: fmt.Sprintf("%s asset%s", formatInt(n), plural(n)),
			Fact:  p.source,
		})
	}
	if total == 0 {
		return nil
	}

	return []Finding{{
		ID:    "utilization.no-usage-data",
		Title: "Resources whose utilisation this tool cannot determine",
		Summary: fmt.Sprintf("%s capacity-bearing asset%s across %s provider%s %s listed here with no "+
			"utilisation figure, because an inventory API does not carry one.", formatInt(total),
			plural(total), formatInt(len(rows)), plural(len(rows)), pluralVerb(total, "is", "are")),
		// Information, and deliberately not a warning: nothing is wrong. The
		// finding exists so that "no utilisation findings for OCI" is read as
		// "this tool cannot see that" rather than "OCI is fine".
		Severity: SeverityInfo,
		Count:    total,
		Basis: "a count of collected assets of the compute, storage, database and edge types listed " +
			"per provider, none of which carries a usage metric in the inventory APIs this tool calls",
		Caveat: "This finding asserts an absence in this tool, not a fact about the estate: these " +
			"resources may be flat out or entirely idle and nothing here distinguishes the two. " +
			"Kubernetes is the single exception, and only because metrics.k8s.io is served by the same " +
			"API server the inventory comes from. Do not infer sizing from instance shape either — a " +
			"shape is what was bought, and this tool refuses to report that as what is used. The " +
			"counted types are the ones this table knows to be capacity-bearing; a type missing from " +
			"it is not thereby measurable.",
		Rows: rows,
	}}
}

// ----------------------------------------------------------------------
// shared reading of Kubernetes payloads
// ----------------------------------------------------------------------

// utilResources is one pod's summed container resources, plus how much of the
// pod actually declared them — the denominator that separates "no requests" from
// "requests on two of three containers".
type utilResources struct {
	cpu          float64 // cores
	mem          float64 // bytes
	containers   int
	withRequests int
}

// utilPodSample is one PodMetrics reading.
type utilPodSample struct {
	use    utilResources
	window string
}

// utilIndexPodMetrics keys every PodMetrics reading by (cluster, namespace,
// name) — PodMetrics has no UID of its own, and its identity is the Pod's.
func utilIndexPodMetrics(in *Input) map[string]utilPodSample {
	out := make(map[string]utilPodSample)
	for _, m := range in.ByType(utilPodMetricsType) {
		s := utilPodSample{}
		if w, ok := in.RawString(m, "window"); ok {
			s.window = w
		}
		containers, ok := in.RawPath(m, "containers")
		if !ok {
			continue
		}
		list, ok := containers.([]any)
		if !ok {
			continue
		}
		for _, c := range list {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			usage, ok := cm["usage"].(map[string]any)
			if !ok {
				continue
			}
			s.use.containers++
			if cpu, ok := utilQuantity(usage["cpu"]); ok {
				s.use.cpu += cpu
			}
			if mem, ok := utilQuantity(usage["memory"]); ok {
				s.use.mem += mem
			}
		}
		if s.use.containers == 0 {
			continue
		}
		out[utilPodKey(m)] = s
	}
	return out
}

// utilPodRequests sums resources.requests across a pod's containers.
//
// initContainers are deliberately left out. Their requests are not additive
// with the app containers' — the scheduler takes the maximum of the two — so
// adding them would overstate every pod that has one, and taking the maximum
// correctly would mean this helper had to explain itself at four call sites.
func utilPodRequests(in *Input, pod core.Asset) (utilResources, bool) {
	v, ok := in.RawPath(pod, "spec.containers")
	if !ok {
		return utilResources{}, false
	}
	list, ok := v.([]any)
	if !ok {
		return utilResources{}, false
	}
	var out utilResources
	for _, c := range list {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		out.containers++
		res, ok := cm["resources"].(map[string]any)
		if !ok {
			continue
		}
		req, ok := res["requests"].(map[string]any)
		if !ok {
			continue
		}
		var declared bool
		if cpu, ok := utilQuantity(req["cpu"]); ok {
			out.cpu += cpu
			declared = true
		}
		if mem, ok := utilQuantity(req["memory"]); ok {
			out.mem += mem
			declared = true
		}
		if declared {
			out.withRequests++
		}
	}
	return out, out.containers > 0
}

// utilRawQuantity reads a Kubernetes quantity out of an asset's payload.
func utilRawQuantity(in *Input, a core.Asset, path string) (float64, bool) {
	v, ok := in.RawPath(a, path)
	if !ok {
		return 0, false
	}
	return utilQuantity(v)
}

// utilQuantity parses a Kubernetes resource quantity — cores for CPU, bytes for
// memory — through apimachinery's own parser. The format has milli, nano,
// binary-SI and decimal-exponent forms and this project already depends on the
// canonical implementation (internal/cost/kube.go does the same).
//
// The float conversion is AsApproximateFloat64 rather than MilliValue, which is
// not interchangeable here. MilliValue rounds *up* to a whole milli, so a
// container sampled at 500000n — half a millicore, the reading a genuinely idle
// sidecar produces — comes back as 1m, double the truth, at exactly the scale
// this file's ratios are computed on.
func utilQuantity(v any) (float64, bool) {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case float64:
		s = strconv.FormatFloat(t, 'f', -1, 64)
	case int64:
		s = strconv.FormatInt(t, 10)
	default:
		return 0, false
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, false
	}
	return q.AsApproximateFloat64(), true
}

// utilHoldsCapacity reports whether a pod is still occupying what it asked for.
// A Succeeded or Failed pod is a record of something that ran; its requests are
// no longer reserved, and counting them would inflate every figure in this file
// on any cluster that runs CronJobs.
func utilHoldsCapacity(a core.Asset) bool {
	switch a.Status {
	case "Succeeded", "Failed":
		return false
	default:
		return true
	}
}

func utilNamespace(a core.Asset) string {
	if ns := a.Tags["namespace"]; ns != "" {
		return ns
	}
	return "(cluster-scoped)"
}

// utilPodKey is the (cluster, namespace, name) identity a Pod and its
// PodMetrics share.
func utilPodKey(a core.Asset) string {
	return a.AccountID + "\x00" + utilNamespace(a) + "\x00" + a.Name
}

func utilPodLabel(a core.Asset) string { return utilNamespace(a) + "/" + DisplayName(a) }

// utilClusterName falls back to a marker rather than an empty string: an
// unlabelled row reads as a rendering bug, and AccountID is empty for a
// cluster whose kube-system UID could not be read.
func utilClusterName(account string) string {
	if account == "" {
		return "(unnamed cluster)"
	}
	return account
}

// ----------------------------------------------------------------------
// formatting
// ----------------------------------------------------------------------

// utilNum formats with at most n decimals and no trailing zeros, so a column
// mixing 8 and 0.05 does not print "8.000".
func utilNum(v float64, decimals int) string {
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

func utilCoreText(cores float64) string { return utilNum(cores, 3) + " CPU" }

// utilByteText renders bytes in the binary units Kubernetes reports them in.
// Converting to decimal GB would inflate every memory figure by 7%.
func utilByteText(bytes float64) string {
	const gi = 1 << 30
	if bytes >= gi {
		return utilNum(bytes/gi, 1) + "Gi"
	}
	return utilNum(bytes/(1<<20), 0) + "Mi"
}

// utilPct is a percentage, rounded, and 0 when the denominator is unknown —
// never a division by zero dressed up as a number.
func utilPct(part, whole float64) int {
	if whole <= 0 {
		return 0
	}
	return int(part/whole*100 + 0.5)
}

// pluralVerb picks the verb form for a count, the companion to render.go's
// plural. A summary that reads "1 assets were created" is machine output, and
// machine output gets skimmed — which is fatal for a sentence whose whole job
// is to be quoted on its own.
func pluralVerb(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
