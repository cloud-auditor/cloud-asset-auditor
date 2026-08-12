package topology

import (
	"encoding/json"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// resolver derives edges from a built index. Each resolver is responsible
// for one edge kind so adding a new kind is one function — and the
// orchestrator is unchanged.
type resolver func(*index) []core.Edge

// resolvers is the registered order. Order is also a *priority* order:
// dedupEdges keeps the first occurrence of any (From, To, Kind) triple,
// so a high-precedence resolver listed first wins ties.
var resolvers = []resolver{
	dnsToTarget,
	wafBinding,
	lbToGateway,
	gatewayToService,
	serviceToWorkload,
	ociNetworkContainment,
	// Traffic-flow resolvers (traffic.go) run last: they answer "who may talk
	// to whom" rather than "what points at what", and listing them after the
	// request-path resolvers means a request-path edge wins any dedup tie.
	kubeNetworkPolicyFlow,
	tailscaleACLFlow,
	netbirdPolicyFlow,
}

// ----------------------------------------------------------------------
// dnsToTarget
// ----------------------------------------------------------------------

// dnsToTarget walks every Cloudflare DNS record and matches its content
// to anything in the IP/hostname index. Confidence is always heuristic
// — we're joining across cloud boundaries on data the providers can't
// authoritatively cross-reference.
func dnsToTarget(idx *index) []core.Edge {
	var out []core.Edge
	for _, rec := range idx.byType["cloudflare.dns_record"] {
		content := rec.Tags["content"]
		if content == "" {
			continue
		}
		var matches []core.Asset
		// siblingsAreTargets distinguishes the two lookup modes. An A/AAAA
		// record is matched against the IP index, which also contains every
		// *other* A record carrying the same address — and a record does not
		// resolve to a sibling record, it resolves to whatever answers at that
		// address. Emitting those pairs invented a mutual edge between every
		// pair of records sharing an IP (a very common setup: apex + www), which
		// showed up as a spurious cycle in reachability analysis.
		//
		// A CNAME is different: it is matched against the hostname index, and a
		// record for the name it points at is a genuine resolution chain, so
		// those matches are kept.
		siblingsAreTargets := false
		switch rec.Tags["type"] {
		case "A", "AAAA":
			matches = idx.byIP[content]
		case "CNAME":
			matches = idx.byHostname[normalizeHost(content)]
			siblingsAreTargets = true
		default:
			continue
		}
		for _, m := range matches {
			if m.ID == rec.ID {
				continue
			}
			if !siblingsAreTargets && m.Type == "cloudflare.dns_record" {
				continue
			}
			out = append(out, core.Edge{
				From:       rec.AsRef(),
				To:         m.AsRef(),
				Kind:       core.EdgeKindDNS,
				Hostname:   rec.Name,
				Confidence: core.ConfidenceHeuristic,
			})
		}
	}
	return out
}

// ----------------------------------------------------------------------
// wafBinding
// ----------------------------------------------------------------------

// wafBinding ties CF security resources (Rulesets, Access apps, Page Rules)
// back to the zones they protect. Confidence is "exact" because the
// resource itself carries the zone_id — there's no cross-cloud join.
//
// Live since the Cloudflare collectors shipped: zone-scoped assets carry a
// zone_id tag that joins to the cloudflare.zone asset's ID. (Tunnels are in
// the candidate list but are account-scoped and carry no zone_id, so they
// simply never match — a tunnel has no per-zone binding.)
func wafBinding(idx *index) []core.Edge {
	var out []core.Edge
	candidates := []string{
		"cloudflare.ruleset",
		"cloudflare.access_app",
		"cloudflare.tunnel",
		"cloudflare.page_rule",
	}
	for _, t := range candidates {
		for _, a := range idx.byType[t] {
			zoneID := a.Tags["zone_id"]
			if zoneID == "" {
				continue
			}
			zone, ok := idx.byID[zoneID]
			if !ok {
				continue
			}
			out = append(out, core.Edge{
				From:       a.AsRef(),
				To:         zone.AsRef(),
				Kind:       core.EdgeKindWAF,
				Confidence: core.ConfidenceExact,
			})
		}
	}
	return out
}

// ----------------------------------------------------------------------
// lbToGateway
// ----------------------------------------------------------------------

// lbToGateway matches OCI Load Balancer IPs to Kubernetes Service or
// Ingress assets whose external IPs include any of those. The chain
// here is `OCI LB → K8s Service.LoadBalancer external IP`, which is
// what happens when an OCI LB fronts an OKE cluster.
func lbToGateway(idx *index) []core.Edge {
	var out []core.Edge
	for _, lb := range idx.byType["oci.load_balancer"] {
		ips := lb.Tags["ip_addresses"]
		if ips == "" {
			continue
		}
		for _, ip := range splitCSV(ips) {
			for _, target := range idx.byIP[ip] {
				if target.ID == lb.ID {
					continue
				}
				if target.Provider != "kubernetes" {
					continue
				}
				out = append(out, core.Edge{
					From:       lb.AsRef(),
					To:         target.AsRef(),
					Kind:       core.EdgeKindLBBackend,
					Confidence: core.ConfidenceHeuristic,
				})
			}
		}
	}
	return out
}

// ----------------------------------------------------------------------
// gatewayToService
// ----------------------------------------------------------------------

// gatewayToService parses K8s Ingress / Gateway Raw payloads to find the
// backing Service they route to. Requires --include-raw (the CLI's
// topology subcommand forces it on); without Raw, this resolver is a no-op.
//
// Supports:
//   - networking.k8s.io/v1.Ingress (spec.rules[].http.paths[].backend.service.name)
//   - gateway.networking.k8s.io/v1*.HTTPRoute (spec.rules[].backendRefs[].name)
func gatewayToService(idx *index) []core.Edge {
	var out []core.Edge

	// Build a (namespace, service-name) → Service asset lookup so we can
	// resolve backendRefs in one pass. The K8s provider stores namespace
	// in Tags["namespace"]; Name is the resource name.
	svcByNsName := map[string]core.Asset{}
	for _, a := range idx.all {
		if a.Type != "v1.Service" {
			continue
		}
		k := a.Tags["namespace"] + "/" + a.Name
		svcByNsName[k] = a
	}

	for _, a := range idx.all {
		if a.Provider != "kubernetes" {
			continue
		}
		switch a.Type {
		case "networking.k8s.io/v1.Ingress":
			out = append(out, ingressBackendEdges(a, svcByNsName)...)
		case "gateway.networking.k8s.io/v1.HTTPRoute",
			"gateway.networking.k8s.io/v1beta1.HTTPRoute":
			out = append(out, httpRouteBackendEdges(a, svcByNsName)...)
		}
	}
	return out
}

func ingressBackendEdges(ing core.Asset, svcs map[string]core.Asset) []core.Edge {
	var obj struct {
		Spec struct {
			Rules []struct {
				Host string `json:"host"`
				HTTP struct {
					Paths []struct {
						Backend struct {
							Service struct {
								Name string `json:"name"`
								Port struct {
									Number int `json:"number"`
								} `json:"port"`
							} `json:"service"`
						} `json:"backend"`
					} `json:"paths"`
				} `json:"http"`
			} `json:"rules"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(ing.Raw, &obj); err != nil {
		return nil
	}
	ns := ing.Tags["namespace"]
	var edges []core.Edge
	for _, rule := range obj.Spec.Rules {
		for _, p := range rule.HTTP.Paths {
			svc, ok := svcs[ns+"/"+p.Backend.Service.Name]
			if !ok {
				continue
			}
			edges = append(edges, core.Edge{
				From:       ing.AsRef(),
				To:         svc.AsRef(),
				Kind:       core.EdgeKindGatewayRoute,
				Hostname:   rule.Host,
				Port:       p.Backend.Service.Port.Number,
				Confidence: core.ConfidenceExact,
			})
		}
	}
	return edges
}

func httpRouteBackendEdges(rt core.Asset, svcs map[string]core.Asset) []core.Edge {
	var obj struct {
		Spec struct {
			Hostnames []string `json:"hostnames"`
			Rules     []struct {
				BackendRefs []struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
					Port      int    `json:"port"`
				} `json:"backendRefs"`
			} `json:"rules"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(rt.Raw, &obj); err != nil {
		return nil
	}
	defaultNS := rt.Tags["namespace"]
	hostname := ""
	if len(obj.Spec.Hostnames) > 0 {
		hostname = obj.Spec.Hostnames[0]
	}

	var edges []core.Edge
	for _, rule := range obj.Spec.Rules {
		for _, br := range rule.BackendRefs {
			ns := br.Namespace
			if ns == "" {
				ns = defaultNS
			}
			svc, ok := svcs[ns+"/"+br.Name]
			if !ok {
				continue
			}
			edges = append(edges, core.Edge{
				From:       rt.AsRef(),
				To:         svc.AsRef(),
				Kind:       core.EdgeKindGatewayRoute,
				Hostname:   hostname,
				Port:       br.Port,
				Confidence: core.ConfidenceExact,
			})
		}
	}
	return edges
}

// ----------------------------------------------------------------------
// serviceToWorkload
// ----------------------------------------------------------------------

// serviceToWorkload resolves a Kubernetes Service to the Pods it selects —
// the service-backend edge (core.EdgeKindServiceBackend) the gateway chain
// stops short of, giving the diagram its within-cluster depth. The Service's
// label selector lives in Raw (spec.selector); Pod labels are surfaced as
// Tags by the provider (mapping.go::collapseTags), so the match is: every
// selector key=value must be present in the Pod's Tags, in the same cluster
// (AccountID) and namespace. Exact — an authoritative same-cluster join, not
// a cross-cloud guess.
//
// Requires --include-raw for the Service payload (the topology CLI forces it
// on). Services with an empty selector (headless / ExternalName) select
// nothing and are skipped — an empty selector must NOT match every Pod.
func serviceToWorkload(idx *index) []core.Edge {
	pods := idx.byType["v1.Pod"]
	if len(pods) == 0 {
		return nil
	}
	// Bucket pods by cluster+namespace once so each Service scans only its own
	// namespace — a full Service×Pod cross product would defeat the index's
	// whole point (topology.go: "naive O(n²) joins fall over fast against real
	// inventories" — clusters reach 50k+ objects). \x00 can't appear in an
	// AccountID or namespace, so it's a safe composite-key separator.
	const sep = "\x00"
	podsByNS := make(map[string][]core.Asset, len(pods))
	for _, pod := range pods {
		k := pod.AccountID + sep + pod.Tags["namespace"]
		podsByNS[k] = append(podsByNS[k], pod)
	}

	var out []core.Edge
	for _, svc := range idx.byType["v1.Service"] {
		selector := serviceSelector(svc.Raw)
		// The provider injects a synthetic "namespace" pseudo-tag
		// (mapping.go::collapseTags) that shadows any real label keyed
		// "namespace"; drop it from the selector — namespace is already
		// enforced by the bucket key, so matching it against the pseudo-tag
		// would be both redundant and wrong. A selector left empty afterward
		// (headless / ExternalName / namespace-only) selects nothing.
		delete(selector, "namespace")
		if len(selector) == 0 {
			continue
		}
		for _, pod := range podsByNS[svc.AccountID+sep+svc.Tags["namespace"]] {
			if !labelsMatch(selector, pod.Tags) {
				continue
			}
			out = append(out, core.Edge{
				From:       svc.AsRef(),
				To:         pod.AsRef(),
				Kind:       core.EdgeKindServiceBackend,
				Confidence: core.ConfidenceExact,
			})
		}
	}
	return out
}

func serviceSelector(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var obj struct {
		Spec struct {
			Selector map[string]string `json:"selector"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	return obj.Spec.Selector
}

// labelsMatch reports whether tags contains every key=value pair in sel.
func labelsMatch(sel, tags map[string]string) bool {
	for k, v := range sel {
		if tags[k] != v {
			return false
		}
	}
	return true
}

// ----------------------------------------------------------------------
// ociNetworkContainment
// ----------------------------------------------------------------------

// ociContainmentRules maps an OCI asset Type to the Tag holding the OCID of
// the network resource that contains it. Each tag value is looked up in
// idx.byID, joining to whichever VCN / subnet asset carries that OCID. Tag
// names verified against the OCI collectors (network.go, oke.go,
// network_load_balancer.go).
var ociContainmentRules = []struct {
	fromType string
	tagKey   string
}{
	{"oci.subnet", "vcn_id"},
	{"oci.nat_gateway", "vcn_id"},
	{"oci.internet_gateway", "vcn_id"},
	{"oci.service_gateway", "vcn_id"},
	{"oci.local_peering_gateway", "vcn_id"},
	{"oci.oke.cluster", "vcn_id"},
	{"oci.network_load_balancer", "subnet_id"},
}

// ociNetworkContainment builds the OCI network backbone: subnets, gateways,
// the OKE control plane, and network load balancers each point at the VCN or
// subnet that contains them via an OCID tag. The OKE→VCN link in particular
// anchors the Kubernetes subgraph inside the OCI VCN that hosts it, tying the
// two providers' clusters together with an authoritative OCI identifier (not
// a cross-cloud IP guess). All exact — the OCID is authoritative within the
// tenancy.
//
// Note: instance→subnet is deliberately absent — the compute collector does
// not record a VNIC/subnet OCID, so there is no self-contained join for it.
func ociNetworkContainment(idx *index) []core.Edge {
	var out []core.Edge
	for _, rule := range ociContainmentRules {
		for _, a := range idx.byType[rule.fromType] {
			parentID := a.Tags[rule.tagKey]
			if parentID == "" {
				continue
			}
			parent, ok := idx.byID[parentID]
			if !ok || parent.ID == a.ID {
				continue
			}
			out = append(out, core.Edge{
				From:       a.AsRef(),
				To:         parent.AsRef(),
				Kind:       core.EdgeKindNetworkContainment,
				Confidence: core.ConfidenceExact,
			})
		}
	}
	return out
}

// ----------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	cur := ""
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if c == ' ' || c == '\t' {
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
