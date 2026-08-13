package topology

// Orphan reporting: which collected assets does the inferred graph connect to
// nothing?
//
// The computation is trivial — an orphan is a node of degree 0, the exact
// complement of what DropOrphans keeps. Everything else in this file exists to
// stop that trivial number from being read as something it is not.
//
// A degree-0 node means "no relationship this tool inferred touches this
// asset". It does not mean unused, unreferenced, or safe to delete. The same
// number is produced by a resolver that needed --include-raw on a snapshot
// collected without it, by a provider that was skipped or whose API token
// lacked the scope to list the other end, and by relationships no resolver
// models at all — this package infers nine kinds of edge and a real estate has
// hundreds. Acting on an orphan listing as if it were a finding is a genuine
// way to cause an outage, so the report leads with that and the renderer
// prints it before it prints a single count.
//
// The one thing the report can do to earn trust is separate signal from noise:
// a v1.ConfigMap has no resolver and never will, so listing 500 of them
// alongside the three load balancers that genuinely lost their DNS records
// buries the useful line. classifyType does that split — see its comment for
// how the "connectable" set is derived rather than hard-coded.

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// OrphanGroup is one (group, type) bucket of degree-0 nodes.
type OrphanGroup struct {
	// Group is the value of the report's grouping dimension — the provider by
	// default, or the account / region when the caller asked for those.
	Group string `json:"group"`
	Type  string `json:"type"`
	Count int    `json:"count"`

	// Examples are a few asset names from the bucket, so a reader has
	// somewhere concrete to start looking. Deliberately a sample and labelled
	// as one: printing 500 names would bury the counts that give the shape.
	Examples []string `json:"examples,omitempty"`
}

// OrphanReport is the full answer to "what does the graph connect to nothing".
//
// It carries its own caveat text so a saved JSON file still says what it does
// and does not mean months later, the same way ReachResult carries its
// Question. A count without that context is the part of this feature that can
// do harm.
type OrphanReport struct {
	Caveat []string `json:"caveat"`

	TotalNodes   int `json:"total_nodes"`
	TotalOrphans int `json:"total_orphans"`

	// GroupBy is the dimension Group was computed on ("provider" by default).
	GroupBy string `json:"group_by"`

	// Providers is every provider that contributed a node to this graph. It is
	// in the report because the commonest cause of a large orphan count is a
	// provider that is missing from this list.
	Providers []string `json:"providers"`

	// RawAvailable reports whether any node carried an Asset.Raw payload. When
	// false, three resolvers (Ingress/HTTPRoute backends, Service selectors,
	// NetworkPolicies) could not run at all, and the Kubernetes numbers below
	// are mostly an artefact of that rather than a property of the cluster.
	RawAvailable bool `json:"raw_available"`

	// RawMissingFor lists the raw-dependent types (see rawDependentTypes) that
	// are present in this graph but of which NOT ONE node carried a payload.
	//
	// RawAvailable alone is not enough, because it is a property of the whole
	// graph while every raw-reading resolver is Kubernetes-specific. A snapshot
	// carrying Cloudflare and OCI payloads but no Kubernetes ones — exactly what
	// post-processing a snapshot to drop the enormous Kubernetes blobs produces —
	// sets RawAvailable true while disconnecting every Service, Ingress,
	// HTTPRoute and NetworkPolicy in the cluster. That is the case where an
	// orphan listing is at its most confidently wrong, so it gets its own signal.
	RawMissingFor []string `json:"raw_missing_for,omitempty"`

	// Unconnected holds types the graph demonstrably can connect: some other
	// asset of the same type has an edge, or a resolver names the type as an
	// input. These are the lines worth reading.
	Unconnected []OrphanGroup `json:"unconnected"`

	// Unmodelled holds types nothing in this package relates to anything.
	// Reporting them as orphans is noise — they are separated rather than
	// dropped so the total still reconciles with the node count.
	Unmodelled []OrphanGroup `json:"unmodelled"`
}

// UnconnectedCount is the number of orphans in types the graph can connect.
func (r OrphanReport) UnconnectedCount() int { return sumCounts(r.Unconnected) }

// UnmodelledCount is the number of orphans in types no resolver models.
func (r OrphanReport) UnmodelledCount() int { return sumCounts(r.Unmodelled) }

func sumCounts(groups []OrphanGroup) int {
	n := 0
	for _, g := range groups {
		n += g.Count
	}
	return n
}

// maxExamples caps the sample names carried per bucket. Three is enough to
// recognise a naming pattern ("all the -canary pods") without turning the
// report into the asset list it is summarising.
const maxExamples = 3

// Orphans reports the degree-0 nodes of the graph, bucketed by (dim, type).
//
// dim is a grouping dimension accepted by groupOf ("provider", "account",
// "region"); an empty or unknown dim falls back to "provider", which is the
// only dimension every asset is guaranteed to have a value for.
//
// It must be called on an uncollapsed graph. Collapse replaces each asset with
// a per-group summary node and *drops* edges that stayed inside one group (it
// moves their count to an internal_edges tag), so a collapsed node can read as
// degree 0 while every asset inside it is connected — the answer would not
// merely be coarse, it would be wrong. The CLI refuses the combination; this
// comment is why.
func (t *Topology) Orphans(dim string) OrphanReport {
	// Probe rather than re-list the valid dimensions: groupOf answers "" for
	// anything it does not handle, so this fallback tracks that function
	// automatically if a fourth dimension is ever added to it.
	if groupOf(core.Asset{Provider: "probe"}, dim) == "" {
		dim = "provider"
	}

	// Degree-0 detection: the exact complement of DropOrphans' keep set.
	incident := make(map[string]struct{}, len(t.Edges)*2)
	connectedTypes := map[string]struct{}{}
	for _, e := range t.Edges {
		incident[refKey(e.From)] = struct{}{}
		incident[refKey(e.To)] = struct{}{}
	}
	providers := map[string]struct{}{}
	rawAvailable := false
	// Present raw-dependent types, mapped to "did ANY node of this type carry a
	// payload". Tracked per type rather than per graph because the raw-reading
	// resolvers are all Kubernetes-specific — see RawMissingFor.
	rawSeen := map[string]bool{}
	for _, n := range t.Nodes {
		providers[n.Provider] = struct{}{}
		if len(n.Raw) > 0 {
			rawAvailable = true
		}
		if _, needsRaw := rawDependentTypes[n.Type]; needsRaw {
			rawSeen[n.Type] = rawSeen[n.Type] || len(n.Raw) > 0
		}
		if _, ok := incident[refKey(n.AsRef())]; ok {
			connectedTypes[n.Type] = struct{}{}
		}
	}

	type bucketKey struct{ group, typ string }
	buckets := map[bucketKey]*OrphanGroup{}
	orphans := 0
	for _, n := range t.Nodes {
		if _, ok := incident[refKey(n.AsRef())]; ok {
			continue
		}
		orphans++
		k := bucketKey{group: groupOf(n, dim), typ: n.Type}
		b, ok := buckets[k]
		if !ok {
			b = &OrphanGroup{Group: k.group, Type: k.typ}
			buckets[k] = b
		}
		b.Count++
		if len(b.Examples) < maxExamples {
			b.Examples = append(b.Examples, displayName(n))
		}
	}

	report := OrphanReport{
		TotalNodes:    len(t.Nodes),
		TotalOrphans:  orphans,
		GroupBy:       dim,
		Providers:     sortedKeys(providers),
		RawAvailable:  rawAvailable,
		RawMissingFor: rawMissingFor(rawSeen),
	}
	for _, b := range buckets {
		if connectableType(b.Type, connectedTypes) {
			report.Unconnected = append(report.Unconnected, *b)
			continue
		}
		report.Unmodelled = append(report.Unmodelled, *b)
	}
	sortGroups(report.Unconnected)
	sortGroups(report.Unmodelled)
	report.Caveat = orphanCaveat(report)
	return report
}

// sortGroups orders buckets biggest-first, which is the order a reader wants:
// the 500-of-a-kind line is the one that explains the total. Ties break on
// group then type so two runs over the same inventory print identically.
func sortGroups(groups []OrphanGroup) {
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Type < b.Type
	})
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func displayName(a core.Asset) string {
	if a.Name != "" {
		return a.Name
	}
	return a.ID
}

// ----------------------------------------------------------------------
// classification
// ----------------------------------------------------------------------

// connectableType reports whether the graph is capable of connecting this
// asset type at all, which is what separates a line worth investigating from
// noise.
//
// Two signals, unioned, because neither alone is honest:
//
//   - Observed. Some *other* asset of the same type got an edge in this very
//     graph, so a resolver plainly handles the type and this instance is a
//     genuine outlier. This signal can never go stale — it is read off the
//     resolvers' actual output — but it is snapshot-dependent: an inventory
//     containing exactly one v1.Service, unconnected, gives it nothing to
//     observe.
//   - Declared. The type appears in resolverInputTypes, the set of types the
//     registered resolvers name. That covers the thin-snapshot case the
//     observed signal misses. It is a table, and a table can rot — so
//     orphans_test.go parses this package's own resolver source and fails if
//     a resolver names a type the table has not been told about.
//
// The union errs toward calling a type connectable, i.e. toward putting a line
// in the section a reader actually reads. For a report whose whole risk is
// being over-trusted, over-reporting a maybe is the safer failure.
func connectableType(typ string, connected map[string]struct{}) bool {
	if _, ok := connected[typ]; ok {
		return true
	}
	_, ok := resolverInputTypes()[typ]
	return ok
}

// declaredResolverTypes are the asset types the registered resolvers name
// directly. Keep it in sync with resolvers.go and traffic.go — and note that
// TestResolverInputTypes_CoversEveryTypeTheResolversName enforces one
// direction of that automatically, so a new `idx.byType["..."]` lookup cannot
// be added without this list being updated.
//
// The reverse direction is deliberately unenforced: several entries are join
// *targets* reached through idx.byID rather than by type (a wafBinding edge
// lands on a cloudflare.zone, an ociNetworkContainment edge on an oci.vcn, a
// netbird policy on a netbird.group), so they never appear as a literal
// anywhere and an automated check would delete them.
var declaredResolverTypes = []string{
	// dnsToTarget, wafBinding — Cloudflare.
	"cloudflare.dns_record",
	"cloudflare.zone",
	"cloudflare.ruleset",
	"cloudflare.access_app",
	"cloudflare.tunnel",
	"cloudflare.page_rule",

	// lbToGateway, ociNetworkContainment — OCI. The containment *sources* are
	// pulled from ociContainmentRules at runtime (see resolverInputTypes), so
	// adding a row there needs no edit here; these are the targets and the
	// types named outside that table.
	"oci.load_balancer",
	"oci.vcn",
	"oci.subnet",

	// gatewayToService, serviceToWorkload, kubeNetworkPolicyFlow — Kubernetes.
	"v1.Service",
	"v1.Pod",
	"networking.k8s.io/v1.Ingress",
	"networking.k8s.io/v1.NetworkPolicy",
	"gateway.networking.k8s.io/v1.HTTPRoute",
	"gateway.networking.k8s.io/v1beta1.HTTPRoute",

	// netbirdPolicyFlow.
	"netbird.peer",
	"netbird.group",
	"netbird.policy_rule",

	// tailscaleACLFlow.
	"tailscale.device",
	"tailscale.user",
	"tailscale.acl_rule",
	"tailscale.acl_tag",
	"tailscale.acl_group",
	"tailscale.acl_host",
}

// resolverInputTypes returns the declared set, extended with every source type
// in ociContainmentRules.
//
// Reading that table at runtime rather than copying it is the point: it is the
// one resolver input list that is already data, and a containment rule added
// there should light up in the orphan classification without anyone
// remembering this file exists.
func resolverInputTypes() map[string]struct{} {
	out := make(map[string]struct{}, len(declaredResolverTypes)+len(ociContainmentRules))
	for _, t := range declaredResolverTypes {
		out[t] = struct{}{}
	}
	for _, r := range ociContainmentRules {
		out[r.fromType] = struct{}{}
	}
	return out
}

// rawDependentTypes are the types whose resolver reads Asset.Raw and can do
// nothing without it. Every one of them is Kubernetes: the upstream fields
// these resolvers need (Ingress .spec.rules, HTTPRoute .spec.rules.backendRefs,
// Service .spec.selector, NetworkPolicy .spec) have no home on core.Asset and
// are only reachable through the payload.
//
// v1.Pod is deliberately NOT here. Pods are matched by their label Tags, which
// survive a Raw-less collection — they disconnect as a *consequence* of the
// Service selector being unreadable, not because their own resolver needs Raw.
// That indirection is why the warning names the knock-on explicitly: a reader
// looking at 37 orphaned Pods would otherwise never suspect the Services.
var rawDependentTypes = map[string]struct{}{
	"v1.Service":                                  {},
	"networking.k8s.io/v1.Ingress":                {},
	"networking.k8s.io/v1.NetworkPolicy":          {},
	"gateway.networking.k8s.io/v1.HTTPRoute":      {},
	"gateway.networking.k8s.io/v1beta1.HTTPRoute": {},
}

// rawMissingFor returns the raw-dependent types that appear in the graph with
// no payload on a single one of their nodes, sorted for deterministic output.
//
// A type is only reported when it is absent from the graph's payloads
// *entirely*. One Service carrying Raw among ten is a per-object gap, which is
// a different (and much less alarming) story than a resolver that never ran.
func rawMissingFor(rawSeen map[string]bool) []string {
	var out []string
	for typ, seen := range rawSeen {
		if !seen {
			out = append(out, typ)
		}
	}
	sort.Strings(out)
	return out
}

// ----------------------------------------------------------------------
// the caveat
// ----------------------------------------------------------------------

// orphanCaveat builds the text that must accompany every orphan count, in
// every format. It is assembled per report rather than kept as a constant so
// the two conditions that most often explain a large number — a snapshot with
// no Raw, a graph with only one provider in it — can be named outright instead
// of left for the reader to deduce.
func orphanCaveat(r OrphanReport) []string {
	out := []string{
		"An orphan is a node of degree 0: no relationship this tool inferred touches it. " +
			"That is a fact about the graph, not about the resource.",

		"It does not mean the asset is unused, unreferenced, or safe to delete. The same " +
			"degree-0 node is produced by: a resolver that reads Asset.Raw running against a " +
			"snapshot collected without --include-raw; a provider that was skipped, errored, or " +
			"whose API token lacked the scope to list the other end of the relationship; or no " +
			"resolver modelling that relationship at all — this tool infers nine kinds of edge, " +
			"and a cloud estate has hundreds.",

		"Deleting a resource because this report listed it is a genuine way to cause an outage. " +
			"Every line here is a question to go and answer, not a finding.",
	}

	switch {
	case !r.RawAvailable:
		out = append(out, "This graph carries no Asset.Raw payloads, so the Ingress/HTTPRoute "+
			"backend, Service-selector and NetworkPolicy resolvers could not run at all. Any "+
			"Kubernetes count below is mostly an artefact of that. Re-collect with "+
			"'audit --include-raw -o json'.")

	// Some payloads present, but none on the types that need them. Without this
	// branch the report stays silent in exactly the case it is most wrong: a
	// partial snapshot disconnects every Service, Ingress and NetworkPolicy in
	// the cluster and then lists them under "the lines to look at".
	case len(r.RawMissingFor) > 0:
		out = append(out, fmt.Sprintf("Some assets here carry Asset.Raw, but not one node of these "+
			"types does: %s. Their resolvers read the payload and nothing else, so those resolvers "+
			"produced no edges at all — and the workloads they would have reached are orphaned with "+
			"them (an unreadable Service selector orphans every Pod it would have selected, so a "+
			"large v1.Pod count below is a symptom, not a finding). A snapshot filtered to drop "+
			"Kubernetes payloads does exactly this. Re-collect with 'audit --include-raw -o json'.",
			strings.Join(r.RawMissingFor, ", ")))
	}
	if len(r.Providers) == 1 {
		out = append(out, fmt.Sprintf("Only one provider (%s) contributed to this graph, so every "+
			"cross-provider resolver had nothing on the far side to join to.", r.Providers[0]))
	}
	return out
}

// ----------------------------------------------------------------------
// rendering
// ----------------------------------------------------------------------

// RenderOrphans writes an orphan report in the given format.
//
// Only table and json are offered. The graph formats are meaningless here —
// the subject is precisely the nodes that have no edges, so there is no graph
// to draw — and quietly accepting `-o dot` to emit a pile of disconnected
// boxes would be a worse answer than an error.
func RenderOrphans(r OrphanReport, format string, w io.Writer) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "table":
		return renderOrphanTable(r, w)
	case "json":
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	default:
		return fmt.Errorf("unknown orphan report format %q (want table|json); "+
			"the graph formats do not apply — an orphan report is by definition the nodes with no edges", format)
	}
}

func renderOrphanTable(r OrphanReport, w io.Writer) error {
	bw := newWriter(w)

	bw.linef("Orphan report — %d of %d node%s have no inferred relationship.",
		r.TotalOrphans, r.TotalNodes, plural(r.TotalNodes))
	bw.linef("Graph built from: %s", strings.Join(r.Providers, ", "))
	bw.line("")

	// The caveat goes first, always, even when there is nothing to report.
	// Below the counts it reads as a footnote, and a footnote is exactly the
	// weight this warning must not have.
	bw.line("WHAT THIS IS, AND WHAT IT IS NOT")
	for _, para := range r.Caveat {
		bw.line("")
		for _, line := range wrapText(para, 74) {
			bw.linef("  %s", line)
		}
	}
	bw.line("")

	if r.TotalOrphans == 0 {
		bw.line("Every node in this graph has at least one edge.")
		return bw.err
	}

	writeOrphanSection(bw, r,
		"CONNECTABLE, BUT NOT CONNECTED HERE",
		"the graph does relate these types elsewhere, so these are the lines to look at",
		r.Unconnected)

	writeOrphanSection(bw, r,
		"NO RESOLVER RELATES THESE TYPES",
		"structurally never connectable, listed only so the totals reconcile",
		r.Unmodelled)

	return bw.err
}

func writeOrphanSection(bw *writer, r OrphanReport, title, subtitle string, groups []OrphanGroup) {
	bw.linef("%s (%d)", title, sumCounts(groups))
	bw.linef("  %s", subtitle)
	bw.line("")
	if len(groups) == 0 {
		bw.line("  (none)")
		bw.line("")
		return
	}

	groupWidth := len(r.GroupBy)
	typeWidth := len("type")
	for _, g := range groups {
		groupWidth = max(groupWidth, len(g.Group))
		typeWidth = max(typeWidth, len(g.Type))
	}

	bw.linef("  %5s  %-*s  %-*s  %s", "count", groupWidth, r.GroupBy, typeWidth, "type", "e.g.")
	for _, g := range groups {
		bw.linef("  %5d  %-*s  %-*s  %s",
			g.Count, groupWidth, g.Group, typeWidth, g.Type, strings.Join(g.Examples, ", "))
	}
	bw.line("")
}

// wrapText greedily wraps a paragraph to width columns. The caveat is the one
// piece of output that is prose rather than data, and prose printed as one
// 400-column line is prose nobody reads.
func wrapText(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var (
		out  []string
		line = words[0]
	)
	for _, word := range words[1:] {
		if len(line)+1+len(word) > width {
			out = append(out, line)
			line = word
			continue
		}
		line += " " + word
	}
	return append(out, line)
}
