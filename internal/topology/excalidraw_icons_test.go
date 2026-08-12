package topology

import (
	"strings"
	"testing"
)

func TestIconKeyForType(t *testing.T) {
	cases := map[string]string{
		"cloudflare.dns_record":                  "dns",
		"cloudflare.zone":                        "dns",
		"cloudflare.ruleset":                     "waf",
		"cloudflare.access_app":                  "waf",
		"cloudflare.tunnel":                      "tunnel",
		"cloudflare.certificate_pack":            "certificate",
		"cloudflare.worker_script":               "function",
		"oci.load_balancer":                      "loadbalancer",
		"oci.network_load_balancer":              "loadbalancer",
		"oci.autonomous_database":                "database",
		"oci.object_storage.bucket":              "storage",
		"oci.block_volume":                       "storage",
		"oci.vcn":                                "network",
		"oci.subnet":                             "network",
		"oci.nat_gateway":                        "network",
		"oci.compute.instance":                   "compute",
		"oci.container_instance":                 "compute",
		"oci.functions.function":                 "function",
		"oci.iam.user":                           "account",
		"oci.compartment":                        "account",
		"v1.Service":                             "service",
		"networking.k8s.io/v1.Ingress":           "gateway",
		"gateway.networking.k8s.io/v1.HTTPRoute": "gateway",
		"apps/v1.Deployment":                     "workload",
		"v1.Pod":                                 "workload",
		"oci.oke.cluster":                        "service",
		"v1.Namespace":                           "account",
		"something.unknown":                      "generic",
	}
	for typ, want := range cases {
		if got := iconKeyForType("", typ); got != want {
			t.Errorf("iconKeyForType(%q) = %q, want %q", typ, got, want)
		}
		// Every mapped key must resolve to a real icon in the catalogue.
		if _, ok := iconSet[want]; !ok {
			t.Errorf("iconSet missing key %q (mapped from %q)", want, typ)
		}
	}
}

func TestIconDataURL_DistinctPerKey(t *testing.T) {
	seen := map[string]string{}
	for key, ic := range iconSet {
		if ic.key != key {
			t.Errorf("iconSet[%q].key = %q (mismatch)", key, ic.key)
		}
		url := ic.dataURL()
		if !strings.HasPrefix(url, "data:image/svg+xml;base64,") {
			t.Errorf("icon %q dataURL malformed: %q", key, url)
		}
		if prev, ok := seen[url]; ok {
			t.Errorf("icons %q and %q produced identical data URLs", key, prev)
		}
		seen[url] = key
		if id := ic.fileID(); id == "" {
			t.Errorf("icon %q empty fileID", key)
		}
	}
}
