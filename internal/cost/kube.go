package cost

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/pricing"
)

// Kubernetes cost attribution. This is the part of the feature that cannot
// stream: it needs every Node before it can price any Pod, and it needs every
// OCI instance before it can tell whether a Node's cost is already counted
// somewhere else. It therefore belongs to the buffered `auditor cost` report
// path (report.go) and is unreachable from Estimator.Annotate by construction —
// see the package doc.
//
// The formula is OpenCost's, restricted to what an inventory can see. OpenCost
// computes a workload's cost from max(request, usage); with no metrics pipeline
// there is no usage, so this is the requests-based lower bound and the report
// says so.

const (
	kubeNodeType = "v1.Node"
	kubePodType  = "v1.Pod"

	// Fallback rate ids from books/kubernetes.yaml — OpenCost's published
	// defaults, which are GCP us-central1 figures and emphatically not the
	// user's cloud's prices. Anything priced from them is `assumed`.
	rateFallbackCPU = "k8s.fallback.cpu"
	rateFallbackRAM = "k8s.fallback.ram"
	rateFallbackGPU = "k8s.fallback.gpu"

	// The conventional label carrying a node's machine type. Set by every
	// managed control plane, and surfaced as a plain tag by collapseTags, so
	// this branch works without --include-raw.
	instanceTypeLabel = "node.kubernetes.io/instance-type"

	// The pre-1.17 spelling, still emitted by some CSI drivers and by clusters
	// upgraded in place.
	legacyInstanceTypeLabel = "beta.kubernetes.io/instance-type"
)

// How a node's rate was established, best first. The counts appear in the
// report because "4 via a real instance, 2 via a generic fallback" is the
// difference between a figure worth acting on and one worth ignoring.
const (
	rateViaInstance = "oci-instance"
	rateViaShape    = "instance-type"
	rateViaFallback = "generic-fallback"
	rateUnresolved  = "unpriced"
)

// KubeSection is the Kubernetes part of the report. Nodes are the billable
// line; pods attribute that same money. The two are never added together, and
// keeping pod figures out of Report.Totals entirely — rather than relying on a
// renderer to remember — is what makes that structural.
type KubeSection struct {
	Clusters []KubeCluster `json:"clusters"`
	Note     string        `json:"note"`
}

// KubeCluster is one cluster's node bill and the attribution of it.
type KubeCluster struct {
	Cluster  string `json:"cluster"`
	Currency string `json:"currency,omitempty"`

	Nodes       int   `json:"nodes"`
	NodesPriced int   `json:"nodes_priced"`
	NodeMonthly Money `json:"node_monthly"`

	// CountedElsewhere is the part of NodeMonthly that already appears in
	// another provider's total, because the node's machine is itself in this
	// audit (an OKE worker is an oci.compute.instance). Adding it again is the
	// most obvious way to inflate a cross-provider total, so it is tracked
	// rather than assumed away.
	CountedElsewhere Estimated `json:"counted_elsewhere_monthly"`

	// RateSources counts how each node's rate was established.
	RateSources []RateSource `json:"rate_sources"`

	Pods           int       `json:"pods"`
	PodsAttributed int       `json:"pods_attributed"`
	PodsSkipped    int       `json:"pods_skipped"`
	Attributed     Estimated `json:"attributed_monthly"`

	// Unrequested is schedulable node capacity that no pod asked for, over the
	// nodes where attribution was possible at all. It is the most actionable
	// number the feature produces and the one that is invisible in cloud
	// billing, so it is reported even when it is zero.
	AttributableNodes int       `json:"attributable_nodes"`
	Unrequested       Estimated `json:"unrequested_monthly"`
	UnrequestedPct    float64   `json:"unrequested_pct"`

	TopPods []AssetCost `json:"top_pods,omitempty"`
}

// RateSource is one way node rates were established, and how many nodes used it.
type RateSource struct {
	Source string `json:"source"`
	Nodes  int    `json:"nodes"`
}

// kubeNote is printed above the section on every surface. It is a caveat about
// arithmetic rather than about prices, which is why it is not in the
// disclaimer.
const kubeNote = "Nodes are the billable line; pod costs ATTRIBUTE that same money. " +
	"Do not add the two together. Attribution is by resources.requests, which is a " +
	"lower bound — a pod using more than it requested is not charged for the excess here."

// node is one node's working state during the two passes.
type node struct {
	asset      core.Asset
	est        Estimate
	rateSource string
	allocCPU   float64 // cores, from status.allocatable
	allocMem   float64 // GiB, from status.allocatable
	attributed float64
	pods       int
}

// attributeKubernetes prices every Node and attributes every Pod, overwriting
// their entries in est. It returns nil when the audit contains no Kubernetes
// nodes or pods at all, so a Cloudflare-only report grows no empty section.
//
// est is indexed in lockstep with assets; the caller has already filled it with
// per-asset estimates, and this replaces the two types the price book
// deliberately leaves as `unknown` because they cannot be answered per-asset.
func (e *Estimator) attributeKubernetes(assets []core.Asset, est []Estimate, opts Options) *KubeSection {
	nodes, pods := splitKubeAssets(assets)
	if e.book == nil || (len(nodes) == 0 && len(pods) == 0) {
		return nil
	}

	// Pass 1 — price the nodes. Needs the OCI instances, hence the whole set.
	instances := indexPricedInstances(assets, est)
	byCluster := map[string]map[string]*node{}
	for _, i := range nodes {
		n := e.priceNode(assets[i], instances)
		est[i] = n.est
		cluster := byCluster[assets[i].AccountID]
		if cluster == nil {
			cluster = map[string]*node{}
			byCluster[assets[i].AccountID] = cluster
		}
		// Pods reference their node by metadata.name, which is Asset.Name.
		cluster[assets[i].Name] = n
	}

	// Pass 2 — attribute the pods against the nodes priced above.
	podCosts := map[string][]AssetCost{}
	for _, i := range pods {
		a := assets[i]
		n := byCluster[a.AccountID][kubePodNodeName(a)]
		e.attributePod(a, n, &est[i])
		if est[i].Attributed {
			n.attributed += est[i].Monthly
			n.pods++
			podCosts[a.AccountID] = append(podCosts[a.AccountID], assetCost(a, est[i]))
		}
	}

	return e.summarizeClusters(byCluster, assets, pods, podCosts, opts)
}

func splitKubeAssets(assets []core.Asset) (nodes, pods []int) {
	for i, a := range assets {
		switch a.Type {
		case kubeNodeType:
			nodes = append(nodes, i)
		case kubePodType:
			pods = append(pods, i)
		}
	}
	return nodes, pods
}

// indexPricedInstances maps every priced compute instance by its own id, so a
// node's providerID can be joined straight to it. Only priced instances are
// indexed: joining to an instance this tool could not price would produce a
// node estimate that inherits "unknown", which the shape and fallback branches
// can often improve on.
func indexPricedInstances(assets []core.Asset, est []Estimate) map[string]Estimate {
	idx := map[string]Estimate{}
	for i, a := range assets {
		if a.Type == "oci.compute.instance" && est[i].Priced {
			idx[a.ID] = est[i]
		}
	}
	return idx
}

// priceNode establishes one node's monthly cost, best source first.
func (e *Estimator) priceNode(a core.Asset, instances map[string]Estimate) *node {
	n := &node{asset: a, rateSource: rateUnresolved}
	r := &resolver{asset: a}
	n.allocCPU, _ = kubeCPU(r, "status", "allocatable", "cpu")
	n.allocMem, _ = kubeMemGiB(r, "status", "allocatable", "memory")

	// 1. The node's machine is in this audit. This is the good case, it is
	//    common because OKE and OCI are audited together, and it is the only
	//    branch that reflects the tenancy's real prices.
	if id, ok := providerInstanceID(r); ok {
		if src, ok := instances[id]; ok {
			n.rateSource = rateViaInstance
			n.est = nodeFromInstance(id, src)
			return n
		}
	}

	// 2. The instance-type label resolves to a shape in the price book.
	if est, ok := e.nodeFromShape(a, r); ok {
		n.rateSource = rateViaShape
		n.est = est
		return n
	}

	// 3. Generic fallback rates. Not the user's cloud's prices, and the detail
	//    has to say so every single time.
	if est, ok := e.nodeFromFallback(r); ok {
		n.rateSource = rateViaFallback
		n.est = est
		return n
	}

	n.est = unknownf("node rate unresolved: spec.providerID names no instance in this audit, " +
		"no " + instanceTypeLabel + " label resolves to a price-book shape, and " +
		"status.capacity is unreadable (needs --include-raw)")
	return n
}

// nodeFromInstance inherits the backing instance's estimate — and marks the
// figure Attributed rather than Priced.
//
// That distinction is load-bearing. The instance is already in this audit and
// already in the OCI total; counting the node as spend of its own would report
// every OKE worker twice. The node still carries a number, because the
// Kubernetes section needs one to attribute pods against, but the number is
// declared to be somebody else's.
func nodeFromInstance(ocid string, src Estimate) Estimate {
	return Estimate{
		Monthly:    src.Monthly,
		Attributed: true,
		Currency:   src.Currency,
		Basis:      src.Basis,
		Detail: fmt.Sprintf("runs on compute instance %s, already counted under its own provider — "+
			"this is that instance's cost, not additional spend: %s", shortOCID(ocid), src.Detail),
	}
}

// nodeFromShape prices a node from its instance-type label.
//
// The label names the shape but not the size, and for a flexible shape the size
// is the whole question, so the OCPU count comes from the book's default and
// the estimate is `assumed`. Memory is different: the node reports its own, and
// gigabytes mean the same thing on every family, so that term is read from
// status.capacity when it is available. There is deliberately no attempt to
// derive OCPUs from status.capacity.cpu — an OCPU is two vCPUs on x86 and one
// on Ampere, and getting that backwards is a silent 2x error in either
// direction.
func (e *Estimator) nodeFromShape(a core.Asset, r *resolver) (Estimate, bool) {
	name := a.Tags[instanceTypeLabel]
	if name == "" {
		name = a.Tags[legacyInstanceTypeLabel]
	}
	if name == "" {
		return Estimate{}, false
	}
	shape, table, ok := e.findShape(name)
	if !ok || shape.OCPURate == "" {
		return Estimate{}, false
	}
	ocpuRate, ok := e.book.Rate(shape.OCPURate)
	if !ok || shape.DefaultOCPU <= 0 {
		return Estimate{}, false
	}

	monthly := shape.DefaultOCPU * e.book.MonthlyAmount(ocpuRate)
	parts := []string{fmt.Sprintf("%s x%s @%s/%s (shape %s default size)",
		rateLabel(ocpuRate), formatQuantity(shape.DefaultOCPU), formatQuantity(ocpuRate.Amount), ocpuRate.Unit, name)}

	if memRate, ok := e.book.Rate(shape.MemoryRate); ok {
		mem, memSrc := shape.DefaultMemoryGB, "shape default"
		if capMem, ok := kubeMemGiB(r, "status", "capacity", "memory"); ok && capMem > 0 {
			mem, memSrc = capMem, "status.capacity.memory"
		}
		if mem > 0 {
			monthly += mem * e.book.MonthlyAmount(memRate)
			parts = append(parts, fmt.Sprintf("%s x%s @%s/%s (%s)",
				rateLabel(memRate), formatQuantity(round2(mem)), formatQuantity(memRate.Amount), memRate.Unit, memSrc))
		}
	}
	if monthly <= 0 {
		return Estimate{}, false
	}
	return Estimate{
		Monthly:  monthly,
		Priced:   true,
		Currency: e.book.CurrencyOf(ocpuRate),
		Basis:    BasisAssumed,
		Detail: fmt.Sprintf("%s (%gh/mo); shape %s/%s matched on the %s label, which carries no OCPU count — "+
			"the book's default size is assumed",
			strings.Join(parts, " + "), e.book.HoursPerMonth, table, name, instanceTypeLabel),
	}, true
}

// nodeFromFallback prices a node from OpenCost's generic defaults. It is the
// last resort and the detail says exactly that, because these are one cloud's
// list prices standing in for another's.
func (e *Estimator) nodeFromFallback(r *resolver) (Estimate, bool) {
	cpuRate, cpuOK := e.book.Rate(rateFallbackCPU)
	ramRate, ramOK := e.book.Rate(rateFallbackRAM)
	if !cpuOK || !ramOK {
		return Estimate{}, false
	}
	// Capacity, not allocatable: the bill is for the whole machine. Allocatable
	// is the right denominator for a pod's share, and it is used as such below.
	cores, coresOK := kubeCPU(r, "status", "capacity", "cpu")
	mem, memOK := kubeMemGiB(r, "status", "capacity", "memory")
	if !coresOK && !memOK {
		return Estimate{}, false
	}

	monthly := cores*e.book.MonthlyAmount(cpuRate) + mem*e.book.MonthlyAmount(ramRate)
	parts := []string{
		fmt.Sprintf("%s x%s @%s/hour", cpuRate.ID, formatQuantity(round2(cores)), formatQuantity(cpuRate.Amount)),
		fmt.Sprintf("%s x%s @%s/hour", ramRate.ID, formatQuantity(round2(mem)), formatQuantity(ramRate.Amount)),
	}
	// A GPU node priced on CPU and RAM alone is wrong by an order of magnitude,
	// so the accelerator count is included when the node reports one — with the
	// same warning attached, since GPU prices vary more than any other line.
	if gpuRate, ok := e.book.Rate(rateFallbackGPU); ok {
		if gpus, ok := kubeCPU(r, "status", "capacity", "nvidia.com/gpu"); ok && gpus > 0 {
			monthly += gpus * e.book.MonthlyAmount(gpuRate)
			parts = append(parts, fmt.Sprintf("%s x%s @%s/hour",
				gpuRate.ID, formatQuantity(gpus), formatQuantity(gpuRate.Amount)))
		}
	}
	if monthly <= 0 {
		return Estimate{}, false
	}
	return Estimate{
		Monthly:  monthly,
		Priced:   true,
		Currency: e.book.CurrencyOf(cpuRate),
		Basis:    BasisAssumed,
		Detail: fmt.Sprintf("%s (%gh/mo) — generic fallback rate, NOT your cloud's price: "+
			"OpenCost's published GCP us-central1 defaults, used because neither spec.providerID nor the %s "+
			"label resolved to a known machine",
			strings.Join(parts, " + "), e.book.HoursPerMonth, instanceTypeLabel),
	}, true
}

// findShape looks a machine type up across every shape table, checking tables
// in a fixed order so the answer does not depend on map iteration.
func (e *Estimator) findShape(name string) (pricing.Shape, string, bool) {
	tables := make([]string, 0, len(e.book.Shapes))
	for t := range e.book.Shapes {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		if s, ok := e.book.Shape(t, name); ok {
			return s, t, true
		}
	}
	return pricing.Shape{}, "", false
}

// attributePod writes a pod's share of its node's cost into est.
//
// Every refusal below is deliberate. There is no "divide the node's cost by the
// pod count" fallback: on any cluster with mixed pod sizes that is wrong by an
// order of magnitude, and it would produce a number indistinguishable from a
// real one.
func (e *Estimator) attributePod(a core.Asset, n *node, est *Estimate) {
	if phase := a.Status; strings.EqualFold(phase, "Succeeded") || strings.EqualFold(phase, "Failed") {
		*est = unknownf("pod is %s and holds no node capacity", phase)
		return
	}
	if n == nil {
		switch name := kubePodNodeName(a); {
		case len(a.Raw) == 0:
			*est = unknownf("pod attribution needs --include-raw (spec.nodeName and resources.requests)")
		case name == "":
			*est = unknownf("pod is not scheduled to a node (spec.nodeName is empty)")
		default:
			*est = unknownf("node %q is not in this audit, so there is no cost to attribute from", name)
		}
		return
	}
	if !n.est.Priced && !n.est.Attributed {
		*est = unknownf("node %q could not be priced: %s", n.asset.Name, n.est.Detail)
		return
	}

	r := &resolver{asset: a}
	cpu, mem, ok := podRequests(r)
	if !ok {
		*est = unknownf("pod attribution needs --include-raw (spec.containers[].resources.requests)")
		return
	}
	if cpu <= 0 && mem <= 0 {
		// A real finding rather than a gap: an unrequested pod is scheduled on
		// capacity somebody else is paying for, and reporting 0.00 would read as
		// "free" instead of "unaccounted".
		*est = unknownf("declares no resources.requests, so a requests-based attribution gives it nothing — " +
			"that is a statement about the manifest, not about free compute")
		return
	}

	// Share of allocatable, not of capacity: the reserved slice is not
	// schedulable, so a pod cannot be said to have claimed any of it. The
	// consequence is that "unrequested" below means capacity a pod could have
	// been scheduled onto and was not, which is the actionable reading.
	cpuShare := share(cpu, n.allocCPU)
	memShare := share(mem, n.allocMem)
	if cpuShare == 0 && memShare == 0 {
		*est = unknownf("node %q reports no allocatable cpu or memory (needs --include-raw)", n.asset.Name)
		return
	}
	// Cost splits evenly between the two dimensions, which is OpenCost's
	// convention in the absence of a per-resource node price breakdown.
	monthly := n.est.Monthly * (cpuShare + memShare) / 2

	*est = Estimate{
		Monthly:    monthly,
		Attributed: true,
		Currency:   n.est.Currency,
		Basis:      weaker(n.est.Basis, BasisInferred),
		Detail: fmt.Sprintf("%s%s%s/mo attributed from node %s (%.1f%% of allocatable CPU, %.1f%% of allocatable memory; "+
			"requests %s cores, %s GiB) — an attribution of node cost, not additional spend",
			EstimateMark, currencySymbol(n.est.Currency), formatAmount(monthly), n.asset.Name,
			cpuShare*100, memShare*100, formatQuantity(round2(cpu)), formatQuantity(round2(mem))),
	}
}

// summarizeClusters folds the two passes into the report section.
func (e *Estimator) summarizeClusters(byCluster map[string]map[string]*node, assets []core.Asset,
	pods []int, podCosts map[string][]AssetCost, opts Options,
) *KubeSection {
	names := make([]string, 0, len(byCluster))
	for c := range byCluster {
		names = append(names, c)
	}
	// Pods can belong to a cluster with no Node assets (a --kube-namespace
	// audit), and those still need a row saying why nothing was attributed.
	for _, i := range pods {
		if _, ok := byCluster[assets[i].AccountID]; !ok {
			byCluster[assets[i].AccountID] = map[string]*node{}
			names = append(names, assets[i].AccountID)
		}
	}
	sort.Strings(names)

	podTotals := map[string]int{}
	for _, i := range pods {
		podTotals[assets[i].AccountID]++
	}

	sec := &KubeSection{Note: kubeNote}
	for _, name := range names {
		kc := KubeCluster{Cluster: name, Pods: podTotals[name]}
		sources := map[string]int{}
		for _, n := range byCluster[name] {
			kc.Nodes++
			sources[n.rateSource]++
			if n.est.Priced || n.est.Attributed {
				kc.NodesPriced++
				kc.NodeMonthly.add(n.est)
				if kc.Currency == "" {
					kc.Currency = n.est.Currency
				}
				if n.est.Attributed {
					kc.CountedElsewhere.Amount += n.est.Monthly
				}
			}
			if n.pods > 0 {
				kc.AttributableNodes++
				kc.Attributed.Amount += n.attributed
				kc.Unrequested.Amount += max0(n.est.Monthly - n.attributed)
			}
			kc.PodsAttributed += n.pods
		}
		kc.PodsSkipped = kc.Pods - kc.PodsAttributed
		kc.CountedElsewhere.Currency = kc.Currency
		kc.Attributed.Currency = kc.Currency
		kc.Unrequested.Currency = kc.Currency
		if pool := kc.Attributed.Amount + kc.Unrequested.Amount; pool > 0 {
			kc.UnrequestedPct = kc.Unrequested.Amount / pool * 100
		}
		kc.RateSources = sortedRateSources(sources)
		kc.TopPods = topN(podCosts[name], opts.TopN)
		sec.Clusters = append(sec.Clusters, kc)
	}
	return sec
}

func sortedRateSources(counts map[string]int) []RateSource {
	order := map[string]int{rateViaInstance: 0, rateViaShape: 1, rateViaFallback: 2, rateUnresolved: 3}
	out := make([]RateSource, 0, len(counts))
	for s, n := range counts {
		out = append(out, RateSource{Source: s, Nodes: n})
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i].Source] < order[out[j].Source] })
	return out
}

// providerInstanceID extracts the cloud machine id from spec.providerID.
// Formats vary by cloud provider ("oci://ocid1.instance...", a bare OCID,
// "gce://project/zone/name"); only an OCID is joinable to anything this tool
// collects, so anything else is left for the next branch.
func providerInstanceID(r *resolver) (string, bool) {
	v, ok := r.rawValue("spec.providerID")
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if !strings.HasPrefix(s, "ocid1.instance.") {
		return "", false
	}
	return s, true
}

// kubePodNodeName reads spec.nodeName, which is how a Pod names its Node and
// matches the Node object's metadata.name — Asset.Name on the node side.
func kubePodNodeName(a core.Asset) string {
	r := &resolver{asset: a}
	if v, ok := r.rawValue("spec.nodeName"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// podRequests sums the pod's effective resource requests.
//
// The effective request is max(sum of regular containers, max of init
// containers): init containers run before the others and their reservation is
// released, so the scheduler takes whichever is larger rather than the sum.
// (Sidecar init containers — those with restartPolicy Always — do add to the
// running total in modern Kubernetes; treating them as ordinary init containers
// under-attributes a sidecar-heavy pod slightly, which is the safe direction.)
//
// ok is false only when the containers list is unreadable, which in practice
// means Raw is absent.
func podRequests(r *resolver) (cpu, mem float64, ok bool) {
	containers, found := containerList(r, "spec.containers")
	if !found {
		return 0, 0, false
	}
	for _, c := range containers {
		ccpu, cmem := containerRequests(c)
		cpu += ccpu
		mem += cmem
	}
	if inits, found := containerList(r, "spec.initContainers"); found {
		var maxCPU, maxMem float64
		for _, c := range inits {
			ccpu, cmem := containerRequests(c)
			maxCPU = maxf(maxCPU, ccpu)
			maxMem = maxf(maxMem, cmem)
		}
		cpu = maxf(cpu, maxCPU)
		mem = maxf(mem, maxMem)
	}
	return cpu, mem, true
}

func containerList(r *resolver, path string) ([]map[string]any, bool) {
	v, ok := r.rawValue(path)
	if !ok {
		return nil, false
	}
	items, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, true
}

func containerRequests(c map[string]any) (cpu, mem float64) {
	res, ok := c["resources"].(map[string]any)
	if !ok {
		return 0, 0
	}
	req, ok := res["requests"].(map[string]any)
	if !ok {
		return 0, 0
	}
	if s, ok := req["cpu"]; ok {
		cpu, _ = quantityCores(s)
	}
	if s, ok := req["memory"]; ok {
		mem, _ = quantityGiB(s)
	}
	return cpu, mem
}

func kubeCPU(r *resolver, path ...string) (float64, bool) {
	v, ok := r.rawValue(strings.Join(path, "."))
	if !ok {
		return 0, false
	}
	return quantityCores(v)
}

func kubeMemGiB(r *resolver, path ...string) (float64, bool) {
	v, ok := r.rawValue(strings.Join(path, "."))
	if !ok {
		return 0, false
	}
	return quantityGiB(v)
}

// quantityCores parses a Kubernetes CPU quantity ("500m", "2", "1500m") into
// cores. Parsing goes through apimachinery's resource package rather than a
// hand-rolled suffix table: the format has milli, binary-SI and decimal-exponent
// forms, and this project already depends on the canonical parser.
func quantityCores(v any) (float64, bool) {
	q, ok := parseQuantity(v)
	if !ok {
		return 0, false
	}
	return float64(q.MilliValue()) / 1000, true
}

// quantityGiB parses a memory quantity into gibibytes. GiB rather than GB
// because both Kubernetes ("8Gi") and OCI shape configs report memory in powers
// of two; converting to decimal gigabytes here would inflate every memory term
// by 7%.
func quantityGiB(v any) (float64, bool) {
	q, ok := parseQuantity(v)
	if !ok {
		return 0, false
	}
	return float64(q.Value()) / (1 << 30), true
}

func parseQuantity(v any) (resource.Quantity, bool) {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case float64:
		s = formatQuantity(t)
	default:
		return resource.Quantity{}, false
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return resource.Quantity{}, false
	}
	return q, true
}

func share(requested, allocatable float64) float64 {
	if allocatable <= 0 || requested <= 0 {
		return 0
	}
	// A pod cannot claim more than the whole node even when the numbers say so
	// (a static pod scheduled outside the scheduler, or allocatable read from a
	// node that has since shrunk).
	if requested > allocatable {
		return 1
	}
	return requested / allocatable
}

func shortOCID(s string) string {
	if len(s) <= 24 {
		return s
	}
	return s[:12] + "..." + s[len(s)-8:]
}

func maxf(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}

func max0(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
