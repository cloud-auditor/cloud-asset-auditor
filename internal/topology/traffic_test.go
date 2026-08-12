package topology

import (
	"encoding/json"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// --- test helpers ---------------------------------------------------------

func asset(provider, typ, id, name string, tags map[string]string) core.Asset {
	return core.Asset{Provider: provider, Type: typ, ID: id, Name: name, Tags: tags}
}

func pod(cluster, ns, name string, labels map[string]string) core.Asset {
	tags := map[string]string{"namespace": ns}
	for k, v := range labels {
		tags[k] = v
	}
	return core.Asset{
		Provider: "kubernetes", AccountID: cluster, Type: "v1.Pod",
		ID: cluster + "/" + ns + "/" + name, Name: name, Tags: tags,
	}
}

func netpol(cluster, ns, name string, spec any) core.Asset {
	raw, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		panic(err)
	}
	return core.Asset{
		Provider: "kubernetes", AccountID: cluster,
		Type: "networking.k8s.io/v1.NetworkPolicy",
		ID:   cluster + "/" + ns + "/" + name, Name: name,
		Tags: map[string]string{"namespace": ns},
		Raw:  raw,
	}
}

// edgeSet renders edges as "from→to:kind" strings for order-insensitive
// comparison.
func edgeSet(edges []core.Edge) map[string]core.Edge {
	out := make(map[string]core.Edge, len(edges))
	for _, e := range edges {
		out[e.From.ID+"→"+e.To.ID+":"+e.Kind] = e
	}
	return out
}

func hasEdge(t *testing.T, edges []core.Edge, from, to, kind string) core.Edge {
	t.Helper()
	e, ok := edgeSet(edges)[from+"→"+to+":"+kind]
	if !ok {
		t.Errorf("missing edge %s→%s (%s); got %v", from, to, kind, edgeKeys(edges))
	}
	return e
}

func noEdge(t *testing.T, edges []core.Edge, from, to, kind string) {
	t.Helper()
	if _, ok := edgeSet(edges)[from+"→"+to+":"+kind]; ok {
		t.Errorf("unexpected edge %s→%s (%s)", from, to, kind)
	}
}

func edgeKeys(edges []core.Edge) []string {
	out := make([]string, 0, len(edges))
	for k := range edgeSet(edges) {
		out = append(out, k)
	}
	return out
}

// --- NetBird --------------------------------------------------------------

func TestNetbirdPolicyFlow_ExpandsGroupsThroughRule(t *testing.T) {
	assets := []core.Asset{
		asset("netbird", "netbird.peer", "p1", "laptop", map[string]string{"group_ids": "gA", "ip": "100.64.0.1"}),
		asset("netbird", "netbird.peer", "p2", "server", map[string]string{"group_ids": "gB", "ip": "100.64.0.2"}),
		asset("netbird", "netbird.policy_rule", "pol1/r1", "allow eng→prod", map[string]string{
			"action": "accept", "sources": "gA", "destinations": "gB", "ports": "443",
		}),
	}
	edges := netbirdPolicyFlow(newIndex(assets))

	e := hasEdge(t, edges, "p1", "pol1/r1", core.EdgeKindTrafficAllow)
	if e.Port != 443 {
		t.Errorf("edge Port = %d, want 443", e.Port)
	}
	hasEdge(t, edges, "pol1/r1", "p2", core.EdgeKindTrafficAllow)

	// The whole point of routing through the rule node: no direct peer→peer
	// edge, so a catch-all rule stays linear rather than a cross-product.
	noEdge(t, edges, "p1", "p2", core.EdgeKindTrafficAllow)
}

func TestNetbirdPolicyFlow_BidirectionalAddsReversePath(t *testing.T) {
	assets := []core.Asset{
		asset("netbird", "netbird.peer", "p1", "a", map[string]string{"group_ids": "gA"}),
		asset("netbird", "netbird.peer", "p2", "b", map[string]string{"group_ids": "gB"}),
		asset("netbird", "netbird.policy_rule", "r", "two-way", map[string]string{
			"action": "accept", "sources": "gA", "destinations": "gB", "bidirectional": "true",
		}),
	}
	edges := netbirdPolicyFlow(newIndex(assets))
	hasEdge(t, edges, "p1", "r", core.EdgeKindTrafficAllow) // forward
	hasEdge(t, edges, "r", "p2", core.EdgeKindTrafficAllow)
	hasEdge(t, edges, "p2", "r", core.EdgeKindTrafficAllow) // reverse
	hasEdge(t, edges, "r", "p1", core.EdgeKindTrafficAllow)
}

// A "drop" rule must render as a denial, not as reachability.
func TestNetbirdPolicyFlow_DropBecomesDenyEdge(t *testing.T) {
	assets := []core.Asset{
		asset("netbird", "netbird.peer", "p1", "a", map[string]string{"group_ids": "gA"}),
		asset("netbird", "netbird.peer", "p2", "b", map[string]string{"group_ids": "gB"}),
		asset("netbird", "netbird.policy_rule", "r", "block", map[string]string{
			"action": "drop", "sources": "gA", "destinations": "gB",
		}),
	}
	edges := netbirdPolicyFlow(newIndex(assets))
	hasEdge(t, edges, "p1", "r", core.EdgeKindTrafficDeny)
	noEdge(t, edges, "p1", "r", core.EdgeKindTrafficAllow)
}

// A rule naming a group with no peers still connects to the group node, so
// the rule doesn't vanish from the graph.
func TestNetbirdPolicyFlow_FallsBackToGroupNode(t *testing.T) {
	assets := []core.Asset{
		asset("netbird", "netbird.group", "gEmpty", "empty", nil),
		asset("netbird", "netbird.peer", "p2", "b", map[string]string{"group_ids": "gB"}),
		asset("netbird", "netbird.policy_rule", "r", "x", map[string]string{
			"action": "accept", "sources": "gEmpty", "destinations": "gB",
		}),
	}
	edges := netbirdPolicyFlow(newIndex(assets))
	hasEdge(t, edges, "gEmpty", "r", core.EdgeKindTrafficAllow)
}

// --- Tailscale ------------------------------------------------------------

func TestTailscaleACLFlow_TagSelectorsAndPort(t *testing.T) {
	assets := []core.Asset{
		asset("tailscale", "tailscale.device", "n1", "laptop.ts.net", map[string]string{"acl_tags": "tag:eng", "ip": "100.0.0.1"}),
		asset("tailscale", "tailscale.device", "n2", "db.ts.net", map[string]string{"acl_tags": "tag:prod,tag:db", "ip": "100.0.0.2"}),
		asset("tailscale", "tailscale.acl_rule", "acl/0", "acl 0", map[string]string{
			"action": "accept", "src": "tag:eng", "dst": "tag:prod:22", "rule_kind": "acl",
		}),
	}
	edges := tailscaleACLFlow(newIndex(assets))
	e := hasEdge(t, edges, "acl/0", "n2", core.EdgeKindTrafficAllow)
	if e.Port != 22 {
		t.Errorf("edge Port = %d, want 22 (parsed off the dst selector)", e.Port)
	}
	hasEdge(t, edges, "n1", "acl/0", core.EdgeKindTrafficAllow)
}

// A wildcard source would otherwise attach every node in the tailnet to the
// rule — the single largest and least informative edge set in the graph.
func TestTailscaleACLFlow_WildcardNotExpanded(t *testing.T) {
	assets := []core.Asset{
		asset("tailscale", "tailscale.device", "n1", "a", map[string]string{"acl_tags": "tag:eng"}),
		asset("tailscale", "tailscale.device", "n2", "b", map[string]string{"acl_tags": "tag:prod"}),
		asset("tailscale", "tailscale.acl_rule", "acl/0", "catch-all", map[string]string{
			"action": "accept", "src": "*", "dst": "tag:prod",
		}),
		asset("tailscale", "tailscale.acl_rule", "acl/1", "autogroup", map[string]string{
			"action": "accept", "src": "autogroup:member", "dst": "tag:prod",
		}),
	}
	edges := tailscaleACLFlow(newIndex(assets))
	if len(edges) != 0 {
		t.Errorf("wildcard src produced %d edges, want 0: %v", len(edges), edgeKeys(edges))
	}
}

func TestTailscaleACLFlow_UserAndHostSelectors(t *testing.T) {
	assets := []core.Asset{
		asset("tailscale", "tailscale.user", "u1", "Amelie", map[string]string{"login_name": "Amelie@Example.com"}),
		asset("tailscale", "tailscale.acl_host", "-:host/bastion", "bastion", map[string]string{"ip": "100.64.0.9"}),
		asset("tailscale", "tailscale.acl_rule", "acl/0", "r", map[string]string{
			"action": "accept", "src": "amelie@example.com", "dst": "bastion",
		}),
	}
	edges := tailscaleACLFlow(newIndex(assets))
	// Login matching must be case-insensitive — the policy file and the user
	// record disagree on case in practice.
	hasEdge(t, edges, "u1", "acl/0", core.EdgeKindTrafficAllow)
	hasEdge(t, edges, "acl/0", "-:host/bastion", core.EdgeKindTrafficAllow)
}

func TestSplitSelectorPort(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantBase string
		wantPort int
	}{
		{"tag:prod:22", "tag:prod", 22},
		// A non-numeric final segment is not a port: "tag:prod" must not be
		// read as host "tag" on port "prod".
		{"tag:prod", "tag:prod", 0},
		{"host:*", "host:*", 0},
		{"web", "web", 0},
		{"10.0.0.1:443", "10.0.0.1", 443},
		{"tag:prod:80-90", "tag:prod:80-90", 0},
		{"tag:prod:99999", "tag:prod:99999", 0},
	} {
		base, port := splitSelectorPort(tc.in)
		if base != tc.wantBase || port != tc.wantPort {
			t.Errorf("splitSelectorPort(%q) = (%q, %d), want (%q, %d)", tc.in, base, port, tc.wantBase, tc.wantPort)
		}
	}
}

// --- Kubernetes NetworkPolicy --------------------------------------------

func TestKubeNetworkPolicyFlow_IngressPodSelector(t *testing.T) {
	assets := []core.Asset{
		pod("c1", "shop", "api-1", map[string]string{"app": "api"}),
		pod("c1", "shop", "web-1", map[string]string{"app": "web"}),
		pod("c1", "shop", "other-1", map[string]string{"app": "other"}),
		netpol("c1", "shop", "api-allow", map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]string{"app": "api"}},
			"ingress": []any{map[string]any{
				"from":  []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]string{"app": "web"}}}},
				"ports": []any{map[string]any{"port": 8080, "protocol": "TCP"}},
			}},
		}),
	}
	edges := kubeNetworkPolicyFlow(newIndex(assets))

	// web ──▶ policy ──▶ api
	e := hasEdge(t, edges, "c1/shop/web-1", "c1/shop/api-allow", core.EdgeKindTrafficAllow)
	if e.Port != 8080 {
		t.Errorf("edge Port = %d, want 8080", e.Port)
	}
	hasEdge(t, edges, "c1/shop/api-allow", "c1/shop/api-1", core.EdgeKindTrafficAllow)
	// The unrelated pod is neither a source nor a target.
	noEdge(t, edges, "c1/shop/other-1", "c1/shop/api-allow", core.EdgeKindTrafficAllow)
	noEdge(t, edges, "c1/shop/api-allow", "c1/shop/other-1", core.EdgeKindTrafficAllow)
}

// An empty spec.podSelector selects EVERY pod in the namespace — the
// default-deny idiom. Unlike a Service selector it must not be skipped.
func TestKubeNetworkPolicyFlow_EmptyPodSelectorSelectsNamespace(t *testing.T) {
	assets := []core.Asset{
		pod("c1", "shop", "a", map[string]string{"app": "a"}),
		pod("c1", "shop", "b", map[string]string{"app": "b"}),
		pod("c1", "other", "c", map[string]string{"app": "c"}),
		netpol("c1", "shop", "default-deny", map[string]any{
			"podSelector": map[string]any{},
			"ingress": []any{map[string]any{
				"from": []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]string{"app": "a"}}}},
			}},
		}),
	}
	edges := kubeNetworkPolicyFlow(newIndex(assets))
	hasEdge(t, edges, "c1/shop/default-deny", "c1/shop/a", core.EdgeKindTrafficAllow)
	hasEdge(t, edges, "c1/shop/default-deny", "c1/shop/b", core.EdgeKindTrafficAllow)
	// A pod in another namespace is out of scope.
	noEdge(t, edges, "c1/shop/default-deny", "c1/other/c", core.EdgeKindTrafficAllow)
}

func TestKubeNetworkPolicyFlow_EgressReversesDirection(t *testing.T) {
	assets := []core.Asset{
		pod("c1", "shop", "api-1", map[string]string{"app": "api"}),
		pod("c1", "shop", "db-1", map[string]string{"app": "db"}),
		netpol("c1", "shop", "api-egress", map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]string{"app": "api"}},
			"egress": []any{map[string]any{
				"to": []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]string{"app": "db"}}}},
			}},
		}),
	}
	edges := kubeNetworkPolicyFlow(newIndex(assets))
	// api ──▶ policy ──▶ db (the source is the selected pod, not the peer)
	hasEdge(t, edges, "c1/shop/api-1", "c1/shop/api-egress", core.EdgeKindTrafficAllow)
	hasEdge(t, edges, "c1/shop/api-egress", "c1/shop/db-1", core.EdgeKindTrafficAllow)
}

// A policy must never reach into a different cluster that happens to reuse a
// namespace name.
func TestKubeNetworkPolicyFlow_ClusterScoped(t *testing.T) {
	assets := []core.Asset{
		pod("c1", "shop", "api-1", map[string]string{"app": "api"}),
		pod("c2", "shop", "api-1", map[string]string{"app": "api"}),
		netpol("c1", "shop", "p", map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]string{"app": "api"}},
			"ingress":     []any{map[string]any{"from": []any{map[string]any{"podSelector": map[string]any{}}}}},
		}),
	}
	edges := kubeNetworkPolicyFlow(newIndex(assets))
	for _, e := range edges {
		if e.From.ID == "c2/shop/api-1" || e.To.ID == "c2/shop/api-1" {
			t.Errorf("policy in cluster c1 reached a pod in cluster c2: %+v", e)
		}
	}
}

// An ipBlock peer denotes addresses, not assets — there is nothing in the
// inventory to link it to, and inventing a node would put a non-asset in the
// graph.
func TestKubeNetworkPolicyFlow_IPBlockPeerIsSkipped(t *testing.T) {
	assets := []core.Asset{
		pod("c1", "shop", "api-1", map[string]string{"app": "api"}),
		netpol("c1", "shop", "p", map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]string{"app": "api"}},
			"ingress":     []any{map[string]any{"from": []any{map[string]any{"ipBlock": map[string]any{"cidr": "10.0.0.0/8"}}}}},
		}),
	}
	if edges := kubeNetworkPolicyFlow(newIndex(assets)); len(edges) != 0 {
		t.Errorf("ipBlock peer produced %d edges, want 0: %v", len(edges), edgeKeys(edges))
	}
}

// matchExpressions needs a real label-selector evaluator; honouring only the
// matchLabels half of a selector that has both would over-match and invent
// flows that the API server does not allow.
func TestKubeNetworkPolicyFlow_MatchExpressionsPeerIsSkipped(t *testing.T) {
	assets := []core.Asset{
		pod("c1", "shop", "api-1", map[string]string{"app": "api"}),
		pod("c1", "shop", "web-1", map[string]string{"app": "web"}),
		netpol("c1", "shop", "p", map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]string{"app": "api"}},
			"ingress": []any{map[string]any{"from": []any{map[string]any{
				"podSelector": map[string]any{
					"matchLabels":      map[string]string{"app": "web"},
					"matchExpressions": []any{map[string]any{"key": "tier", "operator": "In", "values": []string{"x"}}},
				},
			}}}},
		}),
	}
	edges := kubeNetworkPolicyFlow(newIndex(assets))
	noEdge(t, edges, "c1/shop/web-1", "c1/shop/p", core.EdgeKindTrafficAllow)
}

// Without --include-raw there is no spec to read; the resolver must degrade
// to a no-op rather than panic or guess.
func TestKubeNetworkPolicyFlow_NoRawIsNoOp(t *testing.T) {
	assets := []core.Asset{
		pod("c1", "shop", "api-1", map[string]string{"app": "api"}),
		{
			Provider: "kubernetes", AccountID: "c1",
			Type: "networking.k8s.io/v1.NetworkPolicy", ID: "c1/shop/p", Name: "p",
			Tags: map[string]string{"namespace": "shop"},
		},
	}
	if edges := kubeNetworkPolicyFlow(newIndex(assets)); len(edges) != 0 {
		t.Errorf("policy without Raw produced %d edges, want 0", len(edges))
	}
}

// --- integration through Build -------------------------------------------

// The traffic resolvers must be registered, and their edges must survive
// dedup alongside the request-path edges.
func TestBuild_IncludesTrafficEdges(t *testing.T) {
	assets := []core.Asset{
		asset("tailscale", "tailscale.device", "n1", "laptop", map[string]string{"acl_tags": "tag:eng"}),
		asset("tailscale", "tailscale.device", "n2", "db", map[string]string{"acl_tags": "tag:prod"}),
		asset("tailscale", "tailscale.acl_rule", "acl/0", "r", map[string]string{
			"action": "accept", "src": "tag:eng", "dst": "tag:prod",
		}),
	}
	got := Build(assets)
	var traffic int
	for _, e := range got.Edges {
		if e.Kind == core.EdgeKindTrafficAllow {
			traffic++
		}
	}
	if traffic != 2 {
		t.Errorf("Build produced %d traffic-allow edges, want 2 (src→rule, rule→dst); edges=%v", traffic, edgeKeys(got.Edges))
	}
}

func TestTrafficEdgeKind(t *testing.T) {
	for _, tc := range []struct{ action, want string }{
		{"accept", core.EdgeKindTrafficAllow},
		{"allow", core.EdgeKindTrafficAllow},
		{"check", core.EdgeKindTrafficAllow},
		{"", core.EdgeKindTrafficAllow},
		{"drop", core.EdgeKindTrafficDeny},
		{"deny", core.EdgeKindTrafficDeny},
		{"reject", core.EdgeKindTrafficDeny},
	} {
		if got := core.TrafficEdgeKind(tc.action); got != tc.want {
			t.Errorf("TrafficEdgeKind(%q) = %q, want %q", tc.action, got, tc.want)
		}
	}
}
