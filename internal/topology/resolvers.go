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
		switch rec.Tags["type"] {
		case "A", "AAAA":
			matches = idx.byIP[content]
		case "CNAME":
			matches = idx.byHostname[normalizeHost(content)]
		default:
			continue
		}
		for _, m := range matches {
			if m.ID == rec.ID {
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

// wafBinding ties CF security resources (Rulesets, Access apps, Tunnels)
// back to the zones they protect. Confidence is "exact" because the
// resource itself carries the zone_id — there's no cross-cloud join.
//
// This resolver produces zero edges today because Phase 2 left those
// resources stubbed. Wired in advance so filling them in is a one-file
// change.
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
