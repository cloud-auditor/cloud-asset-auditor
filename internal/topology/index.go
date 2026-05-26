package topology

import (
	"encoding/json"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// index is the shared lookup table every resolver consumes. Built once
// per Build() call so cross-provider joins stay O(matches), not O(assets).
type index struct {
	all []core.Asset

	byID       map[string]core.Asset
	byType     map[string][]core.Asset
	byIP       map[string][]core.Asset // IP literal → assets that expose it
	byHostname map[string][]core.Asset // hostname → assets that own it
}

func newIndex(assets []core.Asset) *index {
	idx := &index{
		all:        assets,
		byID:       make(map[string]core.Asset, len(assets)),
		byType:     map[string][]core.Asset{},
		byIP:       map[string][]core.Asset{},
		byHostname: map[string][]core.Asset{},
	}
	for _, a := range assets {
		idx.byID[a.ID] = a
		idx.byType[a.Type] = append(idx.byType[a.Type], a)
		idx.indexNetwork(a)
	}
	return idx
}

// indexNetwork extracts IPs and hostnames from each asset and adds them
// to the per-key buckets. The extraction is provider/type-aware because
// "where is the IP" lives in different places per resource type — the
// universal Asset shape doesn't have a dedicated IPs field.
func (idx *index) indexNetwork(a core.Asset) {
	switch a.Type {

	case "cloudflare.dns_record":
		// A / AAAA records: Tags["content"] is the IP. CNAMEs:
		// hostname (handled below). All records also expose their own
		// Name as a hostname (so a record pointing AT example.com still
		// looks up correctly via the hostname index).
		if a.Name != "" {
			idx.byHostname[normalizeHost(a.Name)] = append(idx.byHostname[normalizeHost(a.Name)], a)
		}
		content := a.Tags["content"]
		switch a.Tags["type"] {
		case "A", "AAAA":
			if content != "" {
				idx.byIP[content] = append(idx.byIP[content], a)
			}
		case "CNAME":
			if content != "" {
				idx.byHostname[normalizeHost(content)] = append(idx.byHostname[normalizeHost(content)], a)
			}
		}

	case "oci.load_balancer":
		// Tags["ip_addresses"] = "1.2.3.4,5.6.7.8" — see
		// internal/providers/oci/load_balancer.go::joinIPAddresses, which
		// produces this format precisely so the topology resolver can
		// index by IP without parsing the Raw payload.
		if ips := a.Tags["ip_addresses"]; ips != "" {
			for _, ip := range strings.Split(ips, ",") {
				ip = strings.TrimSpace(ip)
				if ip != "" {
					idx.byIP[ip] = append(idx.byIP[ip], a)
				}
			}
		}

	default:
		// Kubernetes Services + Ingresses expose external IPs in Raw —
		// they're indexed lazily below if --include-raw fed us the payload.
		if a.Provider != "kubernetes" || len(a.Raw) == 0 {
			return
		}
		for _, ip := range kubeExternalIPs(a.Raw) {
			idx.byIP[ip] = append(idx.byIP[ip], a)
		}
		for _, host := range kubeIngressHosts(a.Raw) {
			idx.byHostname[normalizeHost(host)] = append(idx.byHostname[normalizeHost(host)], a)
		}
	}
}

// kubeExternalIPs reads Service.status.loadBalancer.ingress[*].ip and
// .spec.externalIPs[*] from the Unstructured payload that the Kubernetes
// provider stashes in Asset.Raw when --include-raw is set.
func kubeExternalIPs(raw json.RawMessage) []string {
	var obj struct {
		Spec struct {
			ExternalIPs []string `json:"externalIPs"`
		} `json:"spec"`
		Status struct {
			LoadBalancer struct {
				Ingress []struct {
					IP       string `json:"ip"`
					Hostname string `json:"hostname"`
				} `json:"ingress"`
			} `json:"loadBalancer"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	out := append([]string(nil), obj.Spec.ExternalIPs...)
	for _, ing := range obj.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			out = append(out, ing.IP)
		}
	}
	return out
}

// kubeIngressHosts reads Ingress.spec.rules[*].host. Returns empty for
// non-Ingress payloads — callers don't have to filter by Kind first.
func kubeIngressHosts(raw json.RawMessage) []string {
	var obj struct {
		Spec struct {
			Rules []struct {
				Host string `json:"host"`
			} `json:"rules"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	out := make([]string, 0, len(obj.Spec.Rules))
	for _, r := range obj.Spec.Rules {
		if r.Host != "" {
			out = append(out, r.Host)
		}
	}
	return out
}

// normalizeHost lower-cases the hostname and strips a trailing dot, so
// "Example.com." and "example.com" hash to the same bucket.
func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}
