package topology_test

import (
	"encoding/json"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

// ----------------------------------------------------------------------
// serviceToWorkload (Service → Pod via label selector)
// ----------------------------------------------------------------------

func k8sServiceWorkloadChain() []core.Asset {
	return []core.Asset{
		{
			Provider: "kubernetes", AccountID: "cluster-a",
			Type: "v1.Service", ID: "svc-uid", Name: "api-svc",
			Tags: map[string]string{"namespace": "prod"},
			Raw:  json.RawMessage(`{"spec":{"selector":{"app":"api"}}}`),
		},
		{ // matches: same cluster, namespace, superset of selector labels
			Provider: "kubernetes", AccountID: "cluster-a",
			Type: "v1.Pod", ID: "pod-match", Name: "api-xyz",
			Tags: map[string]string{"namespace": "prod", "app": "api", "pod-template-hash": "abc"},
		},
		{ // wrong label
			Provider: "kubernetes", AccountID: "cluster-a",
			Type: "v1.Pod", ID: "pod-other-label", Name: "web-xyz",
			Tags: map[string]string{"namespace": "prod", "app": "web"},
		},
		{ // right label, wrong namespace
			Provider: "kubernetes", AccountID: "cluster-a",
			Type: "v1.Pod", ID: "pod-other-ns", Name: "api-stg",
			Tags: map[string]string{"namespace": "staging", "app": "api"},
		},
		{ // right label/namespace, different cluster — must not cross-link
			Provider: "kubernetes", AccountID: "cluster-b",
			Type: "v1.Pod", ID: "pod-other-cluster", Name: "api-b",
			Tags: map[string]string{"namespace": "prod", "app": "api"},
		},
	}
}

func TestBuild_ServiceToWorkload_SelectorMatch(t *testing.T) {
	topo := topology.Build(k8sServiceWorkloadChain())

	var to []string
	for _, e := range topo.Edges {
		if e.Kind != core.EdgeKindServiceBackend {
			continue
		}
		if e.From.ID != "svc-uid" {
			t.Errorf("service-backend From = %q, want svc-uid", e.From.ID)
		}
		if e.Confidence != core.ConfidenceExact {
			t.Errorf("service-backend confidence = %q, want exact", e.Confidence)
		}
		to = append(to, e.To.ID)
	}
	if len(to) != 1 || to[0] != "pod-match" {
		t.Errorf("service-backend edges To = %v, want exactly [pod-match] (namespace/cluster/label scoping)", to)
	}
}

func TestBuild_ServiceToWorkload_EmptySelectorMatchesNothing(t *testing.T) {
	assets := []core.Asset{
		{
			Provider: "kubernetes", AccountID: "c", Type: "v1.Service", ID: "s",
			Tags: map[string]string{"namespace": "p"},
			Raw:  json.RawMessage(`{"spec":{}}`),
		},
		{
			Provider: "kubernetes", AccountID: "c", Type: "v1.Pod", ID: "p",
			Tags: map[string]string{"namespace": "p", "app": "x"},
		},
	}
	for _, e := range topology.Build(assets).Edges {
		if e.Kind == core.EdgeKindServiceBackend {
			t.Errorf("empty selector must select nothing, got edge %+v", e)
		}
	}
}

// ----------------------------------------------------------------------
// ociNetworkContainment (subnet/gateway/OKE → VCN, NLB → subnet)
// ----------------------------------------------------------------------

func ociNetworkChain() []core.Asset {
	const tenancy = "ocid1.tenancy..t"
	mk := func(typ, id string, tags map[string]string) core.Asset {
		return core.Asset{Provider: "oci", AccountID: tenancy, Region: "us-ashburn-1", Type: typ, ID: id, Tags: tags}
	}
	return []core.Asset{
		mk("oci.vcn", "ocid1.vcn..v1", nil),
		mk("oci.subnet", "ocid1.subnet..s1", map[string]string{"vcn_id": "ocid1.vcn..v1"}),
		mk("oci.nat_gateway", "ocid1.natgateway..n1", map[string]string{"vcn_id": "ocid1.vcn..v1"}),
		mk("oci.oke.cluster", "ocid1.cluster..k1", map[string]string{"vcn_id": "ocid1.vcn..v1"}),
		mk("oci.network_load_balancer", "ocid1.nlb..nlb1", map[string]string{"subnet_id": "ocid1.subnet..s1"}),
	}
}

func TestBuild_OCINetworkContainment(t *testing.T) {
	topo := topology.Build(ociNetworkChain())

	type pair struct{ from, to string }
	got := map[pair]bool{}
	for _, e := range topo.Edges {
		if e.Kind != core.EdgeKindNetworkContainment {
			continue
		}
		if e.Confidence != core.ConfidenceExact {
			t.Errorf("containment confidence = %q, want exact", e.Confidence)
		}
		got[pair{e.From.ID, e.To.ID}] = true
	}

	for _, want := range []pair{
		{"ocid1.subnet..s1", "ocid1.vcn..v1"},
		{"ocid1.natgateway..n1", "ocid1.vcn..v1"},
		{"ocid1.cluster..k1", "ocid1.vcn..v1"},
		{"ocid1.nlb..nlb1", "ocid1.subnet..s1"},
	} {
		if !got[want] {
			t.Errorf("missing network-containment edge %s -> %s", want.from, want.to)
		}
	}
	if len(got) != 4 {
		t.Errorf("got %d containment edges, want 4: %v", len(got), got)
	}
}

// ----------------------------------------------------------------------
// index fix: CNAME → Service via status.loadBalancer.ingress[].hostname
// ----------------------------------------------------------------------

func TestBuild_DNSCNAMEToK8sServiceHostname(t *testing.T) {
	assets := []core.Asset{
		{
			Provider: "cloudflare", AccountID: "a",
			Type: "cloudflare.dns_record", ID: "rec", Name: "api.example.com",
			Tags: map[string]string{"type": "CNAME", "content": "abc123.elb.amazonaws.com"},
		},
		{
			Provider: "kubernetes", AccountID: "cluster-a",
			Type: "v1.Service", ID: "svc", Name: "api",
			Tags: map[string]string{"namespace": "prod"},
			Raw:  json.RawMessage(`{"status":{"loadBalancer":{"ingress":[{"hostname":"abc123.elb.amazonaws.com"}]}}}`),
		},
	}
	found := false
	for _, e := range topology.Build(assets).Edges {
		if e.Kind == core.EdgeKindDNS && e.From.ID == "rec" && e.To.ID == "svc" {
			found = true
		}
	}
	if !found {
		t.Fatal("CNAME pointing at a Service's loadBalancer hostname produced no DNS edge")
	}
}

// A NetBird peer carries its addresses as tags; a DNS record pointing at the
// peer's public IP (A) or DNS label (CNAME) must produce a cross-provider edge
// via the existing dnsToTarget resolver.
func TestBuild_DNSToNetbirdPeer(t *testing.T) {
	assets := []core.Asset{
		{
			Provider: "cloudflare", AccountID: "a",
			Type: "cloudflare.dns_record", ID: "rec", Name: "vpn.example.com",
			Tags: map[string]string{"type": "A", "content": "35.0.0.1"},
		},
		{
			Provider: "cloudflare", AccountID: "a",
			Type: "cloudflare.dns_record", ID: "rec2", Name: "alias.example.com",
			Tags: map[string]string{"type": "CNAME", "content": "gw.netbird.cloud"},
		},
		{
			Provider: "netbird", AccountID: "acct1",
			Type: "netbird.peer", ID: "p1", Name: "gw",
			Tags: map[string]string{"connection_ip": "35.0.0.1", "ip": "100.64.0.1", "dns_label": "gw.netbird.cloud"},
		},
	}
	var byIP, byHost bool
	for _, e := range topology.Build(assets).Edges {
		if e.Kind != core.EdgeKindDNS || e.To.Type != "netbird.peer" {
			continue
		}
		if e.From.ID == "rec" {
			byIP = true
		}
		if e.From.ID == "rec2" {
			byHost = true
		}
	}
	if !byIP {
		t.Error("A record pointing at the peer's connection_ip produced no edge")
	}
	if !byHost {
		t.Error("CNAME pointing at the peer's dns_label produced no edge")
	}
}

// A GCP address only reaches the index through the ip_addresses tag the
// provider flattens out of additionalAttributes. Before that tag existed the
// join silently produced nothing, which reads identically to "there is no
// such relationship" — the failure mode an inventory tool can least afford.
func TestBuild_DNSToGCPAddress(t *testing.T) {
	assets := []core.Asset{
		{
			Provider: "cloudflare", AccountID: "a",
			Type: "cloudflare.dns_record", ID: "rec", Name: "api.example.com",
			Tags: map[string]string{"type": "A", "content": "198.51.100.20"},
		},
		{
			Provider: "gcp", AccountID: "proj",
			Type: "compute.googleapis.com/ForwardingRule",
			ID:   "//compute.googleapis.com/projects/p/global/forwardingRules/fe",
			Name: "fe",
			Tags: map[string]string{"ip_addresses": "198.51.100.20"},
		},
	}
	for _, e := range topology.Build(assets).Edges {
		if e.Kind == core.EdgeKindDNS && e.From.ID == "rec" && e.To.Provider == "gcp" {
			return
		}
	}
	t.Error("A record pointing at a GCP forwarding rule's address produced no edge")
}
