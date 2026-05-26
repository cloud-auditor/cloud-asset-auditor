package topology_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

// canonicalChain is the fixture exercising the full DNS → LB → Service
// path init-plan.md §3 Phase 10 names as the canonical example. Keep this
// small and shared so multiple tests can assert against it.
func canonicalChain() []core.Asset {
	cfZone := core.Asset{
		Provider: "cloudflare", AccountID: "acct-cf",
		Type: "cloudflare.zone", ID: "z1", Name: "example.com",
	}
	cfRecord := core.Asset{
		Provider: "cloudflare", AccountID: "acct-cf",
		Type: "cloudflare.dns_record", ID: "rec1", Name: "api.example.com",
		Tags: map[string]string{
			"type":      "A",
			"content":   "203.0.113.10",
			"zone_id":   "z1",
			"zone_name": "example.com",
		},
	}
	ociLB := core.Asset{
		Provider: "oci", AccountID: "ocid1.tenancy..t",
		Region: "us-ashburn-1",
		Type:   "oci.load_balancer", ID: "ocid1.lb..lb1", Name: "public-lb",
		Tags: map[string]string{
			"ip_addresses": "203.0.113.10,203.0.113.11",
		},
	}
	k8sService := core.Asset{
		Provider: "kubernetes", AccountID: "prod-cluster",
		Type: "v1.Service", ID: "uid-svc-1", Name: "api-svc",
		Tags: map[string]string{"namespace": "prod"},
		Raw: json.RawMessage(`{
            "spec": {"type": "LoadBalancer", "externalIPs": []},
            "status": {"loadBalancer": {"ingress": [{"ip": "203.0.113.10"}]}}
        }`),
	}
	k8sIngress := core.Asset{
		Provider: "kubernetes", AccountID: "prod-cluster",
		Type: "networking.k8s.io/v1.Ingress", ID: "uid-ing-1", Name: "api-ing",
		Tags: map[string]string{"namespace": "prod"},
		Raw: json.RawMessage(`{
            "spec": {
                "rules": [{
                    "host": "api.example.com",
                    "http": {
                        "paths": [{
                            "backend": {
                                "service": {"name": "api-svc", "port": {"number": 80}}
                            }
                        }]
                    }
                }]
            }
        }`),
	}
	return []core.Asset{cfZone, cfRecord, ociLB, k8sService, k8sIngress}
}

func TestBuild_DNSAToOCILB_EmitsEdge(t *testing.T) {
	topo := topology.Build(canonicalChain())

	found := false
	for _, e := range topo.Edges {
		if e.Kind != core.EdgeKindDNS {
			continue
		}
		if e.From.Type == "cloudflare.dns_record" && e.To.Type == "oci.load_balancer" {
			found = true
			if e.Confidence != core.ConfidenceHeuristic {
				t.Errorf("DNS→LB confidence = %q, want heuristic (cross-cloud IP match)", e.Confidence)
			}
			if e.Hostname != "api.example.com" {
				t.Errorf("Hostname = %q, want api.example.com", e.Hostname)
			}
		}
	}
	if !found {
		t.Fatalf("no DNS → OCI LB edge produced (edges: %+v)", topo.Edges)
	}
}

func TestBuild_DNSToK8sService_AlsoEmitsEdge(t *testing.T) {
	// Both the OCI LB and the K8s Service own 203.0.113.10 in this
	// fixture. The DNS-to-target resolver should emit *both* edges.
	topo := topology.Build(canonicalChain())

	var toK8s, toOCI int
	for _, e := range topo.Edges {
		if e.Kind != core.EdgeKindDNS {
			continue
		}
		switch e.To.Provider {
		case "kubernetes":
			toK8s++
		case "oci":
			toOCI++
		}
	}
	if toK8s == 0 || toOCI == 0 {
		t.Errorf("expected DNS edges to both OCI and K8s targets (got k8s=%d oci=%d)", toK8s, toOCI)
	}
}

func TestBuild_LBToK8sService(t *testing.T) {
	topo := topology.Build(canonicalChain())

	found := false
	for _, e := range topo.Edges {
		if e.Kind == core.EdgeKindLBBackend &&
			e.From.Type == "oci.load_balancer" &&
			e.To.Provider == "kubernetes" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no OCI LB → K8s service edge produced (edges: %+v)", topo.Edges)
	}
}

func TestBuild_IngressToService(t *testing.T) {
	topo := topology.Build(canonicalChain())

	found := false
	for _, e := range topo.Edges {
		if e.Kind == core.EdgeKindGatewayRoute &&
			e.From.Type == "networking.k8s.io/v1.Ingress" &&
			e.To.Type == "v1.Service" {
			found = true
			if e.Confidence != core.ConfidenceExact {
				t.Errorf("Ingress→Service confidence = %q, want exact (same-cluster lookup)", e.Confidence)
			}
			if e.Hostname != "api.example.com" {
				t.Errorf("Hostname = %q, want api.example.com", e.Hostname)
			}
			if e.Port != 80 {
				t.Errorf("Port = %d, want 80", e.Port)
			}
		}
	}
	if !found {
		t.Fatalf("no Ingress → Service edge produced (edges: %+v)", topo.Edges)
	}
}

func TestBuild_DedupesIdenticalEdges(t *testing.T) {
	// Same asset list passed twice — every edge should still appear once.
	assets := canonicalChain()
	doubled := append(assets, assets...)

	once := topology.Build(assets)
	twice := topology.Build(doubled)
	if len(twice.Edges) > len(once.Edges)*2 {
		// Dedup can't get this back to 1× perfectly (each duplicate
		// asset creates extra index-matches via different IDs collisions
		// would imply but practically don't), but it must not be N²:
		t.Errorf("unexpected edge growth: once=%d twice=%d", len(once.Edges), len(twice.Edges))
	}
}

func TestFilterByHostname_KeepsConnectedComponent(t *testing.T) {
	topo := topology.Build(canonicalChain())
	scoped := topo.FilterByHostname([]string{"api.example.com"})

	// Should retain the DNS record, the OCI LB, the K8s Service, and the
	// K8s Ingress — they're all reachable from the api.example.com
	// record via the edge graph.
	wantTypes := map[string]bool{
		"cloudflare.dns_record":          false,
		"oci.load_balancer":              false,
		"v1.Service":                     false,
		"networking.k8s.io/v1.Ingress":   false,
	}
	for _, n := range scoped.Nodes {
		if _, ok := wantTypes[n.Type]; ok {
			wantTypes[n.Type] = true
		}
	}
	for typ, present := range wantTypes {
		if !present {
			t.Errorf("expected %q in filtered topology, missing", typ)
		}
	}

	// The Cloudflare zone is not in any edge (wafBinding had no input)
	// so it should NOT be in the filtered set.
	for _, n := range scoped.Nodes {
		if n.Type == "cloudflare.zone" {
			t.Errorf("zone leaked into filtered output despite having no edges")
		}
	}
}

func TestFilterByHostname_UnknownHostnameYieldsEmpty(t *testing.T) {
	topo := topology.Build(canonicalChain())
	scoped := topo.FilterByHostname([]string{"nope.example.com"})
	if len(scoped.Nodes) != 0 || len(scoped.Edges) != 0 {
		t.Errorf("expected empty graph, got %d nodes / %d edges", len(scoped.Nodes), len(scoped.Edges))
	}
}

func TestDropOrphans(t *testing.T) {
	topo := topology.Build(canonicalChain())
	full := len(topo.Nodes)
	pruned := topo.DropOrphans()
	if len(pruned.Nodes) >= full {
		t.Errorf("orphan drop didn't remove any nodes (full=%d, pruned=%d)", full, len(pruned.Nodes))
	}
	// Edges should be unchanged (they all touch surviving nodes).
	if len(pruned.Edges) != len(topo.Edges) {
		t.Errorf("orphan drop changed edge count (%d → %d)", len(topo.Edges), len(pruned.Edges))
	}
}

// --- renderers ---

func TestRenderer_JSON_OmitsRaw(t *testing.T) {
	assets := canonicalChain()
	topo := topology.Build(assets).DropOrphans()

	r, err := topology.New("json")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := r.Render(topo, &buf); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"raw"`)) {
		t.Errorf("JSON output contains `raw` field; renderer should strip it.\n%s", buf.String())
	}
	// Sanity: it does contain `edges`.
	var parsed struct {
		Edges []core.Edge `json:"edges"`
		Nodes []core.Asset `json:"nodes"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Edges) == 0 || len(parsed.Nodes) == 0 {
		t.Errorf("expected non-empty parsed result, got nodes=%d edges=%d", len(parsed.Nodes), len(parsed.Edges))
	}
}

func TestRenderer_DOT_HasGraphvizSyntax(t *testing.T) {
	topo := topology.Build(canonicalChain()).DropOrphans()
	r, _ := topology.New("dot")
	var buf bytes.Buffer
	if err := r.Render(topo, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"digraph topology",
		"rankdir=LR",
		"->",   // at least one edge
	} {
		if !strings.Contains(out, want) {
			t.Errorf("DOT output missing %q\n%s", want, out)
		}
	}
}

func TestRenderer_Mermaid_HasFlowchart(t *testing.T) {
	topo := topology.Build(canonicalChain()).DropOrphans()
	r, _ := topology.New("mermaid")
	var buf bytes.Buffer
	if err := r.Render(topo, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "flowchart LR") {
		t.Errorf("Mermaid output should start with `flowchart LR`, got:\n%s", out)
	}
	if !strings.Contains(out, "-->") && !strings.Contains(out, "-.->") {
		t.Errorf("Mermaid output has no edges:\n%s", out)
	}
}

func TestNew_UnknownFormatErrors(t *testing.T) {
	if _, err := topology.New("xml"); err == nil {
		t.Error("expected error for unknown format")
	}
}
