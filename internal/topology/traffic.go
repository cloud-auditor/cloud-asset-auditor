package topology

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Traffic-flow resolvers answer "who may talk to whom" by reading the policy
// documents the providers collect — Kubernetes NetworkPolicies, Tailscale ACL
// rules, NetBird policy rules. They complement the request-path resolvers in
// resolvers.go, which only ever say "this points at that".
//
// Shape: every rule is kept as a node in the middle of the path,
//
//	source ──▶ rule ──▶ destination
//
// rather than drawing the source × destination cross-product directly. Two
// reasons, one practical and one semantic:
//
//   - Cost. A single "allow group:all → group:all" rule over 500 peers is
//     250,000 direct edges but only 1,000 through a rule node. Real tailnets
//     and clusters have exactly these catch-all rules, so the cross-product
//     form is not merely wasteful — it makes the graph unrenderable.
//   - Readability. The rule that authorises a flow is the thing an auditor
//     actually wants to click on, and a middle node gives it somewhere to
//     live (with its ports, protocol, and action) instead of smearing those
//     attributes across every derived edge.
//
// A wildcard selector ("*", an empty Kubernetes selector) is deliberately NOT
// expanded to every node — see wildcardSelector.

// ----------------------------------------------------------------------
// netbirdPolicyFlow
// ----------------------------------------------------------------------

// netbirdPolicyFlow expands each netbird.policy_rule's source and destination
// group references into edges through the rule node. Group membership comes
// from the peer's own group_ids tag, so this works on a snapshot collected
// without --include-raw.
//
// Exact: the group ids are authoritative NetBird identifiers, not a
// cross-provider guess.
func netbirdPolicyFlow(idx *index) []core.Edge {
	rules := idx.byType["netbird.policy_rule"]
	if len(rules) == 0 {
		return nil
	}
	peersByGroup := membersByGroup(idx.byType["netbird.peer"], "group_ids")

	var out []core.Edge
	for _, rule := range rules {
		kind := core.TrafficEdgeKind(rule.Tags["action"])
		port := singlePort(splitCSV(rule.Tags["ports"]))

		srcs := expandGroups(idx, peersByGroup, splitCSV(rule.Tags["sources"]))
		dsts := expandGroups(idx, peersByGroup, splitCSV(rule.Tags["destinations"]))

		out = append(out, throughRule(rule, srcs, dsts, kind, port, core.ConfidenceExact)...)

		// A bidirectional rule authorises the reverse path too. Emitting it
		// explicitly matters: a reader tracing reachability follows edge
		// direction, and NetBird's default rules are bidirectional.
		if rule.Tags["bidirectional"] == "true" {
			out = append(out, throughRule(rule, dsts, srcs, kind, port, core.ConfidenceExact)...)
		}
	}
	return out
}

// expandGroups resolves group ids to their member assets, falling back to the
// group asset itself when no members are indexed — a rule referencing an empty
// or unlisted group still belongs in the graph, just with nothing behind it.
func expandGroups(idx *index, membersByGroup map[string][]core.Asset, groupIDs []string) []core.Asset {
	var out []core.Asset
	for _, gid := range groupIDs {
		if members := membersByGroup[gid]; len(members) > 0 {
			out = append(out, members...)
			continue
		}
		if g, ok := idx.byID[gid]; ok {
			out = append(out, g)
		}
	}
	return out
}

// membersByGroup inverts a "this asset belongs to groups X,Y" tag into a
// group → members index.
func membersByGroup(assets []core.Asset, tagKey string) map[string][]core.Asset {
	out := map[string][]core.Asset{}
	for _, a := range assets {
		for _, gid := range splitCSV(a.Tags[tagKey]) {
			out[gid] = append(out[gid], a)
		}
	}
	return out
}

// ----------------------------------------------------------------------
// tailscaleACLFlow
// ----------------------------------------------------------------------

// tailscaleACLFlow expands each tailscale.acl_rule's src/dst selectors into
// edges through the rule node. Selectors are the tailnet policy language:
// `tag:prod`, `group:eng`, a MagicDNS host, a user's login name, or a literal
// address — resolveTailscaleSelector handles each form.
//
// Exact: tag and group membership come from the tailnet's own records.
func tailscaleACLFlow(idx *index) []core.Edge {
	rules := idx.byType["tailscale.acl_rule"]
	if len(rules) == 0 {
		return nil
	}
	devicesByTag := membersByGroup(idx.byType["tailscale.device"], "acl_tags")
	usersByLogin := map[string]core.Asset{}
	for _, u := range idx.byType["tailscale.user"] {
		if login := u.Tags["login_name"]; login != "" {
			usersByLogin[strings.ToLower(login)] = u
		}
	}

	var out []core.Edge
	for _, rule := range rules {
		// A rule's Status carries its action ("accept" / "deny" / "check").
		// SSH "check" is an allow gated on re-authentication, which
		// TrafficEdgeKind's default arm already maps to allow.
		kind := core.TrafficEdgeKind(rule.Tags["action"])

		var (
			srcs []core.Asset
			dsts []core.Asset
			port int
		)
		for _, sel := range splitCSV(rule.Tags["src"]) {
			s, _ := resolveTailscaleSelector(idx, devicesByTag, usersByLogin, sel)
			srcs = append(srcs, s...)
		}
		for _, sel := range splitCSV(rule.Tags["dst"]) {
			d, p := resolveTailscaleSelector(idx, devicesByTag, usersByLogin, sel)
			dsts = append(dsts, d...)
			// Destination selectors carry the port ("tag:prod:22"). Record it
			// only while it stays unambiguous — a rule spanning several ports
			// has no single port to label the edge with.
			if p != 0 && port == 0 {
				port = p
			} else if p != port {
				port = 0
			}
		}
		out = append(out, throughRule(rule, srcs, dsts, kind, port, core.ConfidenceExact)...)
	}
	return out
}

// resolveTailscaleSelector maps one policy selector to the assets it denotes,
// plus the port it pins (0 when unspecified). Recognised forms:
//
//	tag:prod         → every device carrying that ACL tag
//	tag:prod:22      → …restricted to port 22
//	group:eng        → the policy group node
//	user@example.com → that tailnet user
//	bastion          → a policy `hosts` alias
//	100.64.0.9       → whatever exposes that address
//	*, autogroup:*   → nothing (see wildcardSelector)
func resolveTailscaleSelector(
	idx *index,
	devicesByTag map[string][]core.Asset,
	usersByLogin map[string]core.Asset,
	sel string,
) ([]core.Asset, int) {
	sel = strings.TrimSpace(sel)
	if sel == "" || wildcardSelector(sel) {
		return nil, 0
	}

	base, port := splitSelectorPort(sel)

	switch {
	case strings.HasPrefix(base, "tag:"):
		if devs := devicesByTag[base]; len(devs) > 0 {
			return devs, port
		}
		// No device carries the tag yet — fall back to the tag node so the
		// rule still connects to something nameable.
		return byNameOfType(idx, "tailscale.acl_tag", base), port

	case strings.HasPrefix(base, "group:"):
		return byNameOfType(idx, "tailscale.acl_group", base), port

	case strings.Contains(base, "@"):
		if u, ok := usersByLogin[strings.ToLower(base)]; ok {
			return []core.Asset{u}, port
		}
		return nil, port
	}

	// A bare token is a policy host alias, a MagicDNS name, or a literal
	// address. Try each index in turn.
	if hosts := byNameOfType(idx, "tailscale.acl_host", base); len(hosts) > 0 {
		return hosts, port
	}
	if hosts := idx.byHostname[normalizeHost(base)]; len(hosts) > 0 {
		return hosts, port
	}
	return idx.byIP[base], port
}

// splitSelectorPort peels a trailing ":<port>" off a policy selector.
//
// Only a numeric final segment counts: "tag:prod" must not be read as host
// "tag" on port "prod", and an IPv6 literal's colons are not port separators.
// A non-numeric port spec ("*", "80-90") leaves the selector intact and
// reports no port, which is correct — neither pins a single port.
func splitSelectorPort(sel string) (string, int) {
	i := strings.LastIndex(sel, ":")
	if i <= 0 || i == len(sel)-1 {
		return sel, 0
	}
	port, err := strconv.Atoi(sel[i+1:])
	if err != nil || port <= 0 || port > 65535 {
		return sel, 0
	}
	return sel[:i], port
}

// byNameOfType finds assets of a type whose Name matches exactly.
func byNameOfType(idx *index, typ, name string) []core.Asset {
	var out []core.Asset
	for _, a := range idx.byType[typ] {
		if a.Name == name {
			out = append(out, a)
		}
	}
	return out
}

// wildcardSelector reports whether a selector means "everything". These are
// left unexpanded on purpose: expanding "*" would attach every node in the
// tailnet to the rule, which is both the largest edge set in the graph and
// the least informative one — "everyone can reach everyone" is better read
// off the rule itself than off 250,000 edges.
func wildcardSelector(sel string) bool {
	return sel == "*" || strings.HasPrefix(sel, "autogroup:")
}

// ----------------------------------------------------------------------
// kubeNetworkPolicyFlow
// ----------------------------------------------------------------------

// kubeNetworkPolicyFlow reads networking.k8s.io/v1.NetworkPolicy payloads and
// draws the flows they authorise, through the policy node:
//
//	ingress:  peer pod ──▶ policy ──▶ selected pod
//	egress:   selected pod ──▶ policy ──▶ peer pod
//
// Requires --include-raw for the policy spec (the topology CLI forces it on);
// without Raw this resolver is a no-op. Exact — an authoritative same-cluster
// join on labels the API server itself uses.
//
// ipBlock peers are not expanded: a CIDR denotes addresses, not collected
// assets, and inventing a node for it would put a non-asset in a graph whose
// nodes are all assets.
func kubeNetworkPolicyFlow(idx *index) []core.Edge {
	policies := idx.byType["networking.k8s.io/v1.NetworkPolicy"]
	if len(policies) == 0 {
		return nil
	}
	pods := idx.byType["v1.Pod"]
	if len(pods) == 0 {
		return nil
	}

	// Bucket pods by cluster so a policy never matches a pod in a different
	// cluster that happens to share a namespace name.
	const sep = "\x00"
	podsByNS := map[string][]core.Asset{}
	podsByCluster := map[string][]core.Asset{}
	for _, pod := range pods {
		podsByNS[pod.AccountID+sep+pod.Tags["namespace"]] = append(podsByNS[pod.AccountID+sep+pod.Tags["namespace"]], pod)
		podsByCluster[pod.AccountID] = append(podsByCluster[pod.AccountID], pod)
	}

	var out []core.Edge
	for _, pol := range policies {
		spec, ok := parseNetworkPolicy(pol.Raw)
		if !ok {
			continue
		}
		ns := pol.Tags["namespace"]
		local := podsByNS[pol.AccountID+sep+ns]

		// spec.podSelector is the set the policy protects. Unlike a Service
		// selector, an EMPTY podSelector here is meaningful: it selects every
		// pod in the namespace. That is the "default deny" idiom, so it must
		// not be skipped the way serviceToWorkload skips an empty selector.
		selected := matchPods(local, spec.PodSelector.MatchLabels)
		if len(selected) == 0 {
			continue
		}

		for _, rule := range spec.Ingress {
			peers := resolveNetPolPeers(rule.From, pol, ns, podsByNS, podsByCluster, sep)
			port := singlePort(netPolPorts(rule.Ports))
			out = append(out, throughRule(pol, peers, selected, core.EdgeKindTrafficAllow, port, core.ConfidenceExact)...)
		}
		for _, rule := range spec.Egress {
			peers := resolveNetPolPeers(rule.To, pol, ns, podsByNS, podsByCluster, sep)
			port := singlePort(netPolPorts(rule.Ports))
			out = append(out, throughRule(pol, selected, peers, core.EdgeKindTrafficAllow, port, core.ConfidenceExact)...)
		}
	}
	return out
}

// networkPolicySpec is the subset of a NetworkPolicy we read. Only
// matchLabels is honoured — matchExpressions would need a full label-selector
// evaluator, and silently ignoring the expressions of a selector that has
// both would over-match. resolveNetPolPeers therefore skips any peer whose
// selector carries expressions rather than guessing.
type networkPolicySpec struct {
	PodSelector labelSelector `json:"podSelector"`
	PolicyTypes []string      `json:"policyTypes"`
	Ingress     []netPolRule  `json:"ingress"`
	Egress      []netPolRule  `json:"egress"`
}

type labelSelector struct {
	MatchLabels      map[string]string `json:"matchLabels"`
	MatchExpressions []json.RawMessage `json:"matchExpressions"`
}

type netPolRule struct {
	From  []netPolPeer `json:"from"`
	To    []netPolPeer `json:"to"`
	Ports []netPolPort `json:"ports"`
}

type netPolPeer struct {
	PodSelector       *labelSelector `json:"podSelector"`
	NamespaceSelector *labelSelector `json:"namespaceSelector"`
	IPBlock           *struct {
		CIDR string `json:"cidr"`
	} `json:"ipBlock"`
}

type netPolPort struct {
	Port     json.RawMessage `json:"port"` // int or named string
	Protocol string          `json:"protocol"`
}

func parseNetworkPolicy(raw json.RawMessage) (networkPolicySpec, bool) {
	if len(raw) == 0 {
		return networkPolicySpec{}, false
	}
	var obj struct {
		Spec networkPolicySpec `json:"spec"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return networkPolicySpec{}, false
	}
	return obj.Spec, true
}

// resolveNetPolPeers turns a rule's from/to list into the pods it denotes.
//
// The three selector combinations follow the Kubernetes semantics exactly:
//
//	podSelector only                → pods in the policy's own namespace
//	namespaceSelector only          → all pods in the matching namespaces
//	namespaceSelector + podSelector → the pod selector applied within them
//
// An empty from/to list means "all sources", which is left unexpanded for the
// same reason as a Tailscale wildcard.
func resolveNetPolPeers(
	peers []netPolPeer,
	pol core.Asset,
	ns string,
	podsByNS, podsByCluster map[string][]core.Asset,
	sep string,
) []core.Asset {
	var out []core.Asset
	for _, peer := range peers {
		switch {
		case peer.IPBlock != nil:
			// A CIDR is not an asset — nothing to link to.
			continue

		case peer.NamespaceSelector != nil:
			// Namespace labels are not carried on the pod, so a namespace
			// selector can only be honoured when it is empty ("every
			// namespace"). A labelled one would need the Namespace objects
			// themselves; over-matching would invent flows that don't exist.
			if len(peer.NamespaceSelector.MatchLabels) > 0 || len(peer.NamespaceSelector.MatchExpressions) > 0 {
				continue
			}
			candidates := podsByCluster[pol.AccountID]
			if peer.PodSelector == nil {
				out = append(out, candidates...)
				continue
			}
			if len(peer.PodSelector.MatchExpressions) > 0 {
				continue
			}
			out = append(out, matchPods(candidates, peer.PodSelector.MatchLabels)...)

		case peer.PodSelector != nil:
			if len(peer.PodSelector.MatchExpressions) > 0 {
				continue
			}
			out = append(out, matchPods(podsByNS[pol.AccountID+sep+ns], peer.PodSelector.MatchLabels)...)
		}
	}
	return out
}

// matchPods returns the pods whose Tags satisfy every label in sel. An empty
// selector matches all candidates — the Kubernetes "select everything in
// scope" semantics.
func matchPods(candidates []core.Asset, sel map[string]string) []core.Asset {
	if len(sel) == 0 {
		return candidates
	}
	// The provider injects a synthetic "namespace" pseudo-tag that would
	// shadow a real label of the same name; namespace scoping is already
	// applied by the caller's bucketing, so drop it (same reasoning as
	// serviceToWorkload).
	clean := make(map[string]string, len(sel))
	for k, v := range sel {
		if k == "namespace" {
			continue
		}
		clean[k] = v
	}
	if len(clean) == 0 {
		return candidates
	}
	var out []core.Asset
	for _, pod := range candidates {
		if labelsMatch(clean, pod.Tags) {
			out = append(out, pod)
		}
	}
	return out
}

// netPolPorts extracts numeric ports, skipping named ones (which resolve
// against a container's ports, not something the graph can label).
func netPolPorts(ports []netPolPort) []string {
	var out []string
	for _, p := range ports {
		var n int
		if err := json.Unmarshal(p.Port, &n); err == nil {
			out = append(out, strconv.Itoa(n))
		}
	}
	return out
}

// ----------------------------------------------------------------------
// shared helpers
// ----------------------------------------------------------------------

// throughRule wires sources ──▶ rule ──▶ destinations, skipping self-edges
// and emitting nothing when either side is empty (a rule with an unresolvable
// source authorises no drawable flow).
//
// The rule node's own duplicate incident edges collapse in dedupEdges, so a
// selector that names the same asset twice costs nothing.
func throughRule(rule core.Asset, sources, destinations []core.Asset, kind string, port int, confidence string) []core.Edge {
	if len(sources) == 0 || len(destinations) == 0 {
		return nil
	}
	ref := rule.AsRef()
	out := make([]core.Edge, 0, len(sources)+len(destinations))
	for _, s := range sources {
		if s.ID == rule.ID {
			continue
		}
		out = append(out, core.Edge{
			From:       s.AsRef(),
			To:         ref,
			Kind:       kind,
			Port:       port,
			Confidence: confidence,
		})
	}
	for _, d := range destinations {
		if d.ID == rule.ID {
			continue
		}
		out = append(out, core.Edge{
			From:       ref,
			To:         d.AsRef(),
			Kind:       kind,
			Port:       port,
			Confidence: confidence,
		})
	}
	return out
}

// singlePort returns the port when a rule pins exactly one numeric port, and
// 0 otherwise. Labelling an edge with the first of several ports would be a
// lie; labelling it with none is merely incomplete.
func singlePort(ports []string) int {
	if len(ports) != 1 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(ports[0]))
	if err != nil || n <= 0 || n > 65535 {
		return 0
	}
	return n
}
