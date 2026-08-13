package insight

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// The network insights are tested against one estate rather than a fixture per
// case, for the same reason internal/topology tests its resolvers against a
// single cross-cloud chain: the interesting failures are joins between two
// providers, and a fixture built per test drifts into asserting what that test
// already knows.
//
// netFixture is that estate. Every asset in it is there to make one specific
// row appear — or, in three cases, to make sure one does NOT appear, which is
// the half of this feature that is hard to get right.

const (
	netVCNProd  = "ocid1.vcn.oc1..prod"
	netVCNStage = "ocid1.vcn.oc1..stage"
	netSubApp   = "ocid1.subnet.oc1..prodapp"
	netSubPub   = "ocid1.subnet.oc1..prodpub"
	netSubStage = "ocid1.subnet.oc1..stageapp"
)

func netFixture() []core.Asset {
	tags := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	oci := func(typ, id, name string, tg map[string]string) core.Asset {
		return core.Asset{
			Provider: "oci", AccountID: "ocid1.tenancy..t", Region: "eu-frankfurt-1",
			Type: typ, ID: id, Name: name, Status: "AVAILABLE", Tags: tg,
		}
	}
	cf := func(name, typ, content string) core.Asset {
		return core.Asset{
			Provider: "cloudflare", AccountID: "acct-cf",
			Type: "cloudflare.dns_record", ID: "rec-" + name, Name: name,
			Tags: tags("type", typ, "content", content, "zone_id", "z1", "zone_name", "example.com"),
		}
	}

	return []core.Asset{
		// --- OCI network backbone. prod and stage declare the SAME /16, which
		// is the overlap; stage holds nothing but plumbing, which is the idle
		// gateway.
		oci("oci.vcn", netVCNProd, "nw-prod-vcn", tags("cidr_blocks", "10.20.0.0/16")),
		oci("oci.vcn", netVCNStage, "nw-stage-vcn", tags("cidr_blocks", "10.20.0.0/16")),
		oci("oci.subnet", netSubPub, "nw-prod-vcn-public", tags("vcn_id", netVCNProd, "cidr_block", "10.20.0.0/24")),
		oci("oci.subnet", netSubApp, "nw-prod-vcn-app", tags("vcn_id", netVCNProd, "cidr_block", "10.20.16.0/24")),
		oci("oci.subnet", netSubStage, "nw-stage-vcn-app", tags("vcn_id", netVCNStage, "cidr_block", "10.20.16.0/24")),
		oci("oci.nat_gateway", "ocid1.natgateway.oc1..prod", "nw-prod-nat",
			tags("vcn_id", netVCNProd, "nat_ip", "203.0.113.40")),
		oci("oci.internet_gateway", "ocid1.internetgateway.oc1..prod", "nw-prod-igw", tags("vcn_id", netVCNProd)),
		oci("oci.nat_gateway", "ocid1.natgateway.oc1..stage", "nw-stage-nat", tags("vcn_id", netVCNStage)),
		oci("oci.local_peering_gateway", "ocid1.lpg.oc1..prod", "nw-prod-lpg",
			tags("vcn_id", netVCNProd, "peering_status", "PEERED", "peer_advertised_cidr", "10.20.0.0/16")),
		// An OKE cluster carries vcn_id, so prod counts as occupied and its NAT
		// gateway must NOT be reported as idle.
		oci("oci.oke.cluster", "ocid1.cluster.oc1..prod", "nw-prod-oke", tags("vcn_id", netVCNProd)),
		oci("oci.load_balancer", "ocid1.loadbalancer.oc1..prod", "nw-prod-lb",
			tags("ip_addresses", "203.0.113.10", "is_private", "false")),

		// --- Cloudflare.
		{Provider: "cloudflare", AccountID: "acct-cf", Type: "cloudflare.zone", ID: "z1", Name: "example.com"},
		cf("api.example.com", "A", "203.0.113.10"),                  // resolves to the LB
		cf("old.example.com", "A", "10.20.16.55"),                   // inside nw-prod-vcn-app, held by nothing
		cf("gone.example.com", "CNAME", "retired.example.com"),      // a name in a zone we hold
		cf("ext.example.com", "CNAME", "cdn.thirdparty.net"),        // outside the audit: must NOT be a row
		cf("shard.apps.example.com", "CNAME", "x.apps.example.com"), // covered by the wildcard below
		cf("*.apps.example.com", "A", "203.0.113.10"),

		// --- Kubernetes.
		{
			Provider: "kubernetes", AccountID: "prod-cluster", Type: "v1.Service",
			ID: "uid-svc", Name: "api-svc", Tags: tags("namespace", "prod"),
			Raw: json.RawMessage(`{
				"spec": {"type": "LoadBalancer", "selector": {"app": "api"}},
				"status": {"loadBalancer": {"ingress": [{"ip": "203.0.113.10"}]}}
			}`),
		},
		{
			Provider: "kubernetes", AccountID: "prod-cluster", Type: "networking.k8s.io/v1.Ingress",
			ID: "uid-ing", Name: "shop-ingress", Tags: tags("namespace", "prod"),
			Raw: json.RawMessage(`{
				"spec": {"rules": [{"host": "shop.example.com", "http": {"paths": [
					{"backend": {"service": {"name": "api-svc", "port": {"number": 80}}}}
				]}}]}
			}`),
		},
		{
			// An HTTPRoute keeps its hostnames in spec.hostnames, which the
			// topology index does not read — the ingress finding supplements it.
			Provider: "kubernetes", AccountID: "prod-cluster", Type: "gateway.networking.k8s.io/v1.HTTPRoute",
			ID: "uid-route", Name: "api-route", Tags: tags("namespace", "prod"),
			Raw: json.RawMessage(`{
				"spec": {
					"hostnames": ["api.example.com"],
					"rules": [{"backendRefs": [{"name": "api-svc", "port": 80}]}]
				}
			}`),
		},
		{
			Provider: "kubernetes", AccountID: "prod-cluster", Type: "v1.Pod",
			ID: "uid-pod", Name: "api-7c9f", Tags: tags("namespace", "prod", "app", "api"),
			Raw: json.RawMessage(`{"status": {"phase": "Running", "podIP": "10.244.3.7"}}`),
		},
		{
			Provider: "kubernetes", AccountID: "prod-cluster", Type: "v1.Node",
			ID: "uid-node", Name: "oke-node-01",
			Raw: json.RawMessage(`{"status": {"addresses": [
				{"type": "InternalIP", "address": "10.20.16.7"},
				{"type": "Hostname", "address": "oke-node-01"}
			]}}`),
		},

		// --- Mesh. The overlay address is CGNAT: private plan, not public.
		{
			Provider: "tailscale", AccountID: "-", Type: "tailscale.device",
			ID: "dev1", Name: "bastion", Tags: tags("ip", "100.64.0.9", "dns_name", "bastion.tail.ts.net"),
		},
	}
}

func netInput(tb testing.TB, extra ...core.Asset) *Input {
	tb.Helper()
	return NewInput(append(netFixture(), extra...), WithNow(fixedNow))
}

// netRun runs one insight and enforces the house rule on everything it emits,
// so no test below can pass with a finding that states more than it knows.
func netRun(tb testing.TB, ins Insight, in *Input) []Finding {
	tb.Helper()
	findings := ins.Run(context.Background(), in)
	for i, f := range findings {
		f.Family = ins.Family()
		if err := ValidateFinding(f); err != nil {
			tb.Fatalf("%s finding %d: %v", ins.ID(), i, err)
		}
	}
	return findings
}

func netFinding(tb testing.TB, findings []Finding, id string) Finding {
	tb.Helper()
	for _, f := range findings {
		if f.ID == id {
			return f
		}
	}
	tb.Fatalf("no finding %q in %v", id, netIDs(findings))
	return Finding{}
}

func netIDs(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.ID)
	}
	return out
}

func netRowLabels(f Finding) []string {
	out := make([]string, 0, len(f.Rows))
	for _, r := range f.Rows {
		out = append(out, r.Label)
	}
	return out
}

func netRow(tb testing.TB, f Finding, label string) Row {
	tb.Helper()
	for _, r := range f.Rows {
		if r.Label == label {
			return r
		}
	}
	tb.Fatalf("finding %q has no row %q; rows: %v", f.ID, label, netRowLabels(f))
	return Row{}
}

func netHasRow(f Finding, label string) bool {
	for _, r := range f.Rows {
		if r.Label == label {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------
// classification
// ----------------------------------------------------------------------

func TestPublicAddr_Classification(t *testing.T) {
	cases := map[string]bool{
		"203.0.113.10":    true, // documentation space, but globally scoped
		"8.8.8.8":         true,
		"2001:db8::1":     true,
		"10.20.16.7":      false, // RFC1918
		"172.16.4.4":      false,
		"192.168.1.1":     false,
		"100.64.0.9":      false, // CGNAT — where both mesh VPNs live
		"100.127.255.1":   false,
		"127.0.0.1":       false,
		"169.254.10.1":    false,
		"fd00::1":         false, // IPv6 unique-local
		"::1":             false,
		"224.0.0.1":       false,
		"0.0.0.0":         false,
		"::ffff:10.0.0.1": false, // the v4-mapped form of a private address
	}
	for s, want := range cases {
		addr, ok := parseAddr(s)
		if !ok {
			t.Fatalf("parseAddr(%q) failed", s)
		}
		if got := publicAddr(addr); got != want {
			t.Errorf("publicAddr(%s) = %v, want %v", s, got, want)
		}
	}

	// CGNAT is not public AND is part of an address plan — it must land in the
	// private inventory rather than nowhere.
	cg, _ := parseAddr("100.64.0.9")
	if !privateAddr(cg) {
		t.Error("CGNAT must count as private address space (the mesh overlay lives there)")
	}
	lo, _ := parseAddr("127.0.0.1")
	if privateAddr(lo) {
		t.Error("loopback is a per-host fact, not part of an address plan")
	}
}

// ----------------------------------------------------------------------
// network.public-addresses
// ----------------------------------------------------------------------

func TestPublicAddresses_ListsHeldAddressesAndWhatIsBehindThem(t *testing.T) {
	in := netInput(t)
	f := netFinding(t, netRun(t, publicAddresses{}, in), "network.public-addresses")

	if !netHasRow(f, "203.0.113.10") {
		t.Fatalf("the load balancer's address is missing; rows: %v", netRowLabels(f))
	}
	// A NAT gateway's egress address is public and is one of the few an OCI
	// estate has — the topology index does not join on it, so this is the
	// supplement working.
	if !netHasRow(f, "203.0.113.40") {
		t.Errorf("the NAT gateway's nat_ip is missing; rows: %v", netRowLabels(f))
	}

	for _, private := range []string{"10.20.16.7", "10.244.3.7", "100.64.0.9"} {
		if netHasRow(f, private) {
			t.Errorf("%s is not internet-routable and must not be listed", private)
		}
	}

	// The address the DNS record points at is held by the LB and the Service,
	// and the record itself is not a holder.
	row := netRow(t, f, "203.0.113.10")
	if !strings.Contains(row.Fact, "nw-prod-lb") || !strings.Contains(row.Fact, "api-svc") {
		t.Errorf("row fact %q should name both assets answering on the address", row.Fact)
	}
	if strings.Contains(row.Fact, "api.example.com") && !strings.Contains(row.Fact, "→") {
		t.Errorf("a DNS record must not be reported as holding an address: %q", row.Fact)
	}
	if len(row.Related) == 0 {
		t.Error("what sits behind a public address should be carried as related refs")
	}

	// A network-containment edge points from a gateway to the VCN that
	// contains it, so walking it forwards would report the VCN as something
	// sitting behind the gateway's public address. It is the container, not a
	// backend.
	nat := netRow(t, f, "203.0.113.40")
	for _, rel := range nat.Related {
		if rel.Type == "oci.vcn" {
			t.Errorf("the VCN containing the NAT gateway is not behind its address: %+v", rel)
		}
	}
}

func TestHeldAddresses_ExcludeDNSTargets(t *testing.T) {
	// 198.51.100.9 exists only as a record's content. Nothing answers on it,
	// so it is not an address this estate holds — that distinction is what
	// keeps network.public-addresses from reporting every third-party CDN as
	// an endpoint of this inventory.
	in := netInput(t, core.Asset{
		Provider: "cloudflare", AccountID: "acct-cf", Type: "cloudflare.dns_record",
		ID: "rec-far", Name: "far.example.com",
		Tags: map[string]string{"type": "A", "content": "198.51.100.9", "zone_name": "example.com"},
	})
	for _, h := range heldAddresses(in) {
		if h.addr.String() == "198.51.100.9" {
			t.Fatalf("a DNS record's content was counted as an address %s holds", h.asset.Type)
		}
	}
}

// ----------------------------------------------------------------------
// network.private-addresses
// ----------------------------------------------------------------------

func TestPrivateAddresses_PerSubnetOccupancyAndUndeclaredBuckets(t *testing.T) {
	in := netInput(t)
	f := netFinding(t, netRun(t, privateAddresses{}, in), "network.private-addresses")

	// The node's InternalIP falls in the app subnet, whose CIDR the provider
	// declares, so the row carries a denominator.
	app := netRow(t, f, "10.20.16.0/24")
	if app.Value != "1 / 256" {
		t.Errorf("occupancy of the app subnet = %q, want %q", app.Value, "1 / 256")
	}
	if !strings.Contains(app.Fact, "nw-prod-vcn-app") {
		t.Errorf("row should name the subnet and its VCN, got %q", app.Fact)
	}

	// A declared block with nothing observed in it is still listed: "we see
	// nothing here" is a different statement from "this block does not exist".
	pub := netRow(t, f, "10.20.0.0/24")
	if pub.Value != "0 / 256" {
		t.Errorf("empty declared subnet = %q, want %q", pub.Value, "0 / 256")
	}

	// The pod network is not declared by any collected resource, so it is
	// bucketed and labelled as such rather than attributed to a subnet.
	pods := netRow(t, f, "10.244.0.0/16")
	if !strings.Contains(pods.Fact, "no collected network declares this range") {
		t.Errorf("undeclared bucket must say so, got %q", pods.Fact)
	}

	// The mesh overlay is private space and must appear, not vanish between
	// the public and private findings.
	if !netHasRow(f, "100.64.0.0/16") {
		t.Errorf("the CGNAT mesh address is missing from the private inventory; rows: %v", netRowLabels(f))
	}

	if !strings.Contains(f.Caveat, "floor") {
		t.Errorf("the occupancy caveat must say the figure is a floor, got %q", f.Caveat)
	}
}

func TestPrivateAddresses_VCNBlockDoesNotSwallowItsSubnets(t *testing.T) {
	// The node address is inside both 10.20.0.0/16 (the VCN) and 10.20.16.0/24
	// (the subnet). containingRange must pick the more specific one, or every
	// address in the estate collapses into one VCN-sized row.
	in := netInput(t)
	f := netFinding(t, netRun(t, privateAddresses{}, in), "network.private-addresses")
	if vcn := netRow(t, f, "10.20.0.0/16"); vcn.Value != "0 / 65536" {
		t.Errorf("the VCN block should hold no address of its own, got %q", vcn.Value)
	}
}

// ----------------------------------------------------------------------
// network.gateways
// ----------------------------------------------------------------------

func TestNetworkGateways_GroupedByVCN(t *testing.T) {
	in := netInput(t)
	findings := netRun(t, networkGateways{}, in)
	f := netFinding(t, findings, "network.gateways")

	if f.Count != 4 {
		t.Errorf("gateway count = %d, want 4", f.Count)
	}
	prod := netRow(t, f, "nw-prod-vcn")
	if !strings.Contains(prod.Fact, "203.0.113.40") {
		t.Errorf("the NAT gateway's egress address belongs in the row, got %q", prod.Fact)
	}
	for _, want := range []string{"internet", "NAT", "peering"} {
		if !strings.Contains(prod.Fact, want) {
			t.Errorf("row %q should mention a %s gateway", prod.Fact, want)
		}
	}
}

func TestNetworkGateways_IdleGatewayIsOnlyRaisedWhereNothingIsRecorded(t *testing.T) {
	in := netInput(t)
	f := netFinding(t, netRun(t, networkGateways{}, in), "network.gateway-without-workload")

	if !netHasRow(f, "nw-stage-nat") {
		t.Fatalf("the staging NAT gateway sits in an empty VCN and should be listed; rows: %v", netRowLabels(f))
	}
	// prod holds an OKE cluster tagged with its vcn_id, so its gateways are
	// accounted for. Reporting them would be the false positive that makes
	// this finding worthless.
	if netHasRow(f, "nw-prod-nat") || netHasRow(f, "nw-prod-igw") {
		t.Errorf("a gateway in a VCN with a collected resource must not be raised; rows: %v", netRowLabels(f))
	}
	// Peering gateways are half of a relationship with another VCN, so
	// emptiness on this side says nothing.
	if netHasRow(f, "nw-prod-lpg") {
		t.Error("a local peering gateway is not an egress gateway")
	}

	// The two things this finding must not overclaim.
	for _, want := range []string{"compute instances record no VCN", "not a bill"} {
		if !strings.Contains(f.Caveat, want) {
			t.Errorf("caveat must contain %q, got %q", want, f.Caveat)
		}
	}
	if f.Severity == SeverityRisk || f.Severity == SeverityWarn {
		t.Errorf("severity %q is too strong for an inference this weak", f.Severity)
	}
}

// The summary is the line that travels without its table — into a ticket, a
// Slack paste, a CI log — so it must not claim more than the rows beneath it.
// It read "records no resource at all" while every row said "N collected
// subnets, no resource in either": the occupancy scan skips the VCN and its
// subnets deliberately, because they constitute the network rather than occupy
// it, but that makes "no resource at all" false about assets this audit holds.
func TestNetworkGateways_SummaryDoesNotDenyTheSubnetsItsRowsCount(t *testing.T) {
	in := netInput(t)
	f := netFinding(t, netRun(t, networkGateways{}, in), "network.gateway-without-workload")

	if strings.Contains(f.Summary, "no resource at all") {
		t.Errorf("summary overclaims against its own rows: %q", f.Summary)
	}
	if !strings.Contains(f.Summary, "beyond the network's own subnets") {
		t.Errorf("summary must scope its claim to what the occupancy scan actually tested: %q", f.Summary)
	}
	// The rows are the evidence the summary is being held to; if they stop
	// counting subnets this test is guarding nothing.
	var counted bool
	for _, r := range f.Rows {
		if strings.Contains(r.Fact, "collected subnet") {
			counted = true
		}
	}
	if !counted {
		t.Error("no row reports its collected subnets, so the summary's scoping is untested")
	}
}

// ----------------------------------------------------------------------
// network.ingress-points
// ----------------------------------------------------------------------

func TestIngressPoints_HostnamesAndBackends(t *testing.T) {
	in := netInput(t)
	f := netFinding(t, netRun(t, ingressPoints{}, in), "network.ingress-points")

	for _, want := range []string{"shop-ingress", "api-svc", "nw-prod-lb"} {
		if !netHasRow(f, want) {
			t.Errorf("%s is an ingress point and is missing; rows: %v", want, netRowLabels(f))
		}
	}

	ing := netRow(t, f, "shop-ingress")
	if !strings.Contains(ing.Fact, "shop.example.com") {
		t.Errorf("an Ingress row must name the hostname it answers on, got %q", ing.Fact)
	}
	if !strings.Contains(ing.Fact, "api-svc") {
		t.Errorf("an Ingress row must name what it fronts, got %q", ing.Fact)
	}

	// The Service is an entry point because a controller gave it an address,
	// and the DNS name that resolves to it is part of how it is reached.
	svc := netRow(t, f, "api-svc")
	if !strings.Contains(svc.Fact, "api.example.com") {
		t.Errorf("a published Service should carry the DNS name pointing at it, got %q", svc.Fact)
	}

	// An HTTPRoute keeps its hostnames somewhere the topology index does not
	// look, so this row exists only because the finding reads spec.hostnames
	// itself.
	route := netRow(t, f, "api-route")
	if !strings.Contains(route.Fact, "api.example.com") {
		t.Errorf("an HTTPRoute's spec.hostnames must be read, got %q", route.Fact)
	}
}

func TestIngressPoints_UnpublishedServiceIsNotAnIngressPoint(t *testing.T) {
	// spec.type: LoadBalancer with no address means no controller has
	// published it. Declaring intent is not the same as answering on an
	// address, and topology.EntryPoints draws the same line.
	in := netInput(t, core.Asset{
		Provider: "kubernetes", AccountID: "prod-cluster", Type: "v1.Service",
		ID: "uid-pending", Name: "pending-svc", Tags: map[string]string{"namespace": "prod"},
		Raw: json.RawMessage(`{"spec": {"type": "LoadBalancer"}, "status": {"loadBalancer": {}}}`),
	})
	f := netFinding(t, netRun(t, ingressPoints{}, in), "network.ingress-points")
	if netHasRow(f, "pending-svc") {
		t.Error("a Service with no published address is not an ingress point")
	}
}

// ----------------------------------------------------------------------
// network.dangling-dns — the finding whose false positives matter most
// ----------------------------------------------------------------------

func TestDanglingDNS_RaisesOnlyTargetsInsideTheEstate(t *testing.T) {
	in := netInput(t)
	f := netFinding(t, netRun(t, danglingDNS{}, in), "network.dangling-dns")

	if !netHasRow(f, "gone.example.com") {
		t.Errorf("a CNAME into a zone this audit holds, answered by nothing, is the finding; rows: %v",
			netRowLabels(f))
	}
	if !netHasRow(f, "old.example.com") {
		t.Errorf("an A record into a declared subnet, held by nothing, is the finding; rows: %v",
			netRowLabels(f))
	}

	// The three that must never be raised.
	if netHasRow(f, "api.example.com") {
		t.Error("a record the graph resolved is not dangling")
	}
	if netHasRow(f, "ext.example.com") {
		t.Error("a target outside every range and zone this audit describes is indistinguishable " +
			"from a deleted one and must not be raised")
	}
	if netHasRow(f, "shard.apps.example.com") {
		t.Error("a wildcard record in the same zone answers for the target")
	}

	// The unlisted ones are counted rather than silently dropped.
	if !strings.Contains(f.Caveat, "1 further record") {
		t.Errorf("the caveat must count the records it declined to list, got %q", f.Caveat)
	}
	// The blind spot that produces this finding's own false positives.
	if !strings.Contains(f.Caveat, "OCI compute instances publish no address") {
		t.Errorf("the caveat must name the address blind spot, got %q", f.Caveat)
	}

	// The strongest evidence first: the table prints twelve rows and the CNAME
	// into a zone we enumerate is better evidence than an address in a range
	// whose occupants are partly invisible.
	if f.Rows[0].Label != "gone.example.com" {
		t.Errorf("rows should lead with the zone-scoped tier, got %q", f.Rows[0].Label)
	}
}

func TestDanglingDNS_DefaultRouteDoesNotMakeTheInternetOurs(t *testing.T) {
	// A netbird.route carrying 0.0.0.0/0 is a routing statement, not a claim of
	// ownership. Accepting it as a declared range would put every address on
	// the internet "inside a range this estate owns", and every unresolved
	// record in the audit would become a subdomain-takeover claim.
	in := netInput(t, core.Asset{
		Provider: "netbird", AccountID: "nb", Type: "netbird.route", ID: "rt1", Name: "default",
		Tags: map[string]string{"network": "0.0.0.0/0", "network_type": "IPv4"},
	})
	f := netFinding(t, netRun(t, danglingDNS{}, in), "network.dangling-dns")
	for _, r := range f.Rows {
		if strings.Contains(r.Fact, "0.0.0.0/0") {
			t.Fatalf("a default route was treated as a declared range: %q", r.Fact)
		}
	}
	if netHasRow(f, "ext.example.com") {
		t.Fatal("a default route must not pull an out-of-scope target into the finding")
	}
}

func TestDanglingDNS_NotRunWithoutAGraph(t *testing.T) {
	// With no edges, "the graph resolved this to nothing" is true of every
	// record in the audit. Skipping says so; running would report the whole
	// zone as dangling.
	records := []core.Asset{
		{
			Provider: "cloudflare", AccountID: "acct-cf", Type: "cloudflare.dns_record",
			ID: "rec-lonely", Name: "lonely.example.com",
			Tags: map[string]string{"type": "A", "content": "10.20.16.9", "zone_name": "example.com"},
		},
	}
	in := NewInput(records, WithNow(fixedNow))
	reason, ok := unmet(danglingDNS{}, in)
	if ok {
		t.Fatalf("the insight should have been skipped, not run")
	}
	if !strings.Contains(reason, "no edges") {
		t.Errorf("skip reason = %q, want it to name the empty graph", reason)
	}
}

// ----------------------------------------------------------------------
// network.overlapping-cidrs
// ----------------------------------------------------------------------

func TestOverlappingCIDRs_AcrossNetworksOnly(t *testing.T) {
	in := netInput(t)
	f := netFinding(t, netRun(t, overlappingCIDRs{}, in), "network.overlapping-cidrs")

	if !netHasRow(f, "nw-prod-vcn ↔ nw-stage-vcn") {
		t.Fatalf("the two VCNs declare the same /16; rows: %v", netRowLabels(f))
	}
	// A VCN contains its own subnets and two subnets of one VCN cannot
	// overlap: comparing them would report every estate that exists.
	for _, label := range netRowLabels(f) {
		if strings.Contains(label, "nw-prod-vcn ↔ nw-prod-vcn") {
			t.Errorf("blocks inside one network must never be compared: %q", label)
		}
	}

	// The one exact signal: the peering gateway's own peer_advertised_cidr.
	peerFact := ""
	for _, r := range f.Rows {
		if strings.Contains(r.Label, "peer of nw-prod-lpg") {
			peerFact = r.Fact
		}
	}
	if peerFact == "" {
		t.Fatalf("a peer advertising an overlapping block should be reported; rows: %v", netRowLabels(f))
	}
	if !strings.Contains(peerFact, "peered") {
		t.Errorf("the peering state belongs in the row, got %q", peerFact)
	}

	if !strings.Contains(f.Caveat, "routing") {
		t.Errorf("the caveat must say routing is not collected, got %q", f.Caveat)
	}
}

func TestDeclaredRanges_IgnoreBlocksTooLargeToBeOwnership(t *testing.T) {
	in := netInput(t, core.Asset{
		Provider: "netbird", AccountID: "nb", Type: "netbird.route", ID: "rt0", Name: "default",
		Tags: map[string]string{"network": "0.0.0.0/0"},
	}, core.Asset{
		Provider: "netbird", AccountID: "nb", Type: "netbird.route", ID: "rt1", Name: "corp",
		Tags: map[string]string{"network": "10.60.0.0/16"},
	})
	var sawDefault, sawCorp bool
	for _, r := range declaredRanges(in) {
		if r.prefix.Bits() == 0 {
			sawDefault = true
		}
		if r.prefix.String() == "10.60.0.0/16" {
			sawCorp = true
		}
	}
	if sawDefault {
		t.Error("0.0.0.0/0 is a route, not an owned range")
	}
	if !sawCorp {
		t.Error("a route to a real private block is an owned range")
	}
}

// ----------------------------------------------------------------------
// whole-family properties
// ----------------------------------------------------------------------

// netInsights is every insight this file registers. Listed explicitly so a new
// one has to be added here deliberately — and so these tests do not silently
// start asserting things about another family's insights.
func netInsights() []Insight {
	return []Insight{
		publicAddresses{}, privateAddresses{}, networkGateways{},
		ingressPoints{}, danglingDNS{}, overlappingCIDRs{},
	}
}

func TestNetworkInsights_AreRegistered(t *testing.T) {
	for _, want := range netInsights() {
		got, ok := Lookup(want.ID())
		if !ok {
			t.Errorf("%s is not registered", want.ID())
			continue
		}
		if got.Family() != FamilyNetwork {
			t.Errorf("%s has family %q, want %q", want.ID(), got.Family(), FamilyNetwork)
		}
	}
}

func TestNetworkInsights_EveryFindingNamesWhatItCannotKnow(t *testing.T) {
	in := netInput(t)
	for _, ins := range netInsights() {
		findings := netRun(t, ins, in) // netRun runs ValidateFinding on each
		for _, f := range findings {
			// Beyond the framework's floor: a caveat that does not mention the
			// inventory's own limits is boilerplate. Every one of these should
			// name a specific thing this finding cannot see.
			if len(strings.Fields(f.Caveat)) < 12 {
				t.Errorf("%s: caveat is too short to say anything specific: %q", f.ID, f.Caveat)
			}
			if len(strings.Fields(f.Basis)) < 8 {
				t.Errorf("%s: basis must name what was joined: %q", f.ID, f.Basis)
			}
		}
	}
}

func TestNetworkInsights_AreDeterministic(t *testing.T) {
	// Two Inputs, not one run twice: the second Input re-sorts the assets and
	// rebuilds the graph, which is where a map-iteration order would leak into
	// the output.
	encode := func(in *Input) string {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, ins := range netInsights() {
			if err := enc.Encode(ins.Run(context.Background(), in)); err != nil {
				t.Fatal(err)
			}
		}
		return buf.String()
	}
	shuffled := netFixture()
	for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	first := encode(netInput(t))
	second := encode(NewInput(shuffled, WithNow(fixedNow)))
	if first != second {
		t.Error("network findings are not deterministic across two runs")
	}
}

func TestNetworkInsights_QuietOnAnEmptyInventory(t *testing.T) {
	// Nothing to say is the normal case and must not be dressed up as a
	// finding. (Requirements are checked by the runner, so an insight that
	// would be skipped is exercised here directly and must still say nothing.)
	in := NewInput(nil, WithNow(fixedNow))
	for _, ins := range netInsights() {
		if got := ins.Run(context.Background(), in); len(got) != 0 {
			t.Errorf("%s invented %d findings from an empty inventory", ins.ID(), len(got))
		}
	}
}

func TestNetworkInsights_RenderThroughTheReport(t *testing.T) {
	// An end-to-end pass: the runner stamps families, validates, and the table
	// renderer prints the caveat in the same visual unit as the number.
	rep := Run(context.Background(), netInput(t), Options{Insights: netInsights()})
	if len(rep.Suppressed) != 0 {
		t.Fatalf("the framework refused findings: %+v", rep.Suppressed)
	}
	var buf bytes.Buffer
	if err := Render(rep, "table", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"NETWORK", "cannot know", "203.0.113.10", "gone.example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report is missing %q", want)
		}
	}
}

// ----------------------------------------------------------------------
// the shared parser
// ----------------------------------------------------------------------

func TestOccupancy_NoDenominatorWhereItWouldBeMeaningless(t *testing.T) {
	cases := []struct {
		prefix string
		n      int
		want   string
	}{
		{"10.20.16.0/24", 3, "3 / 256"},
		{"10.20.0.0/16", 12, "12 / 65536"},
		{"10.0.0.0/8", 12, "12"}, // a /8 denominator is a rounding error
		{"fd00::/48", 4, "4"},    // and an IPv6 one is a joke
	}
	for _, c := range cases {
		p := netip.MustParsePrefix(c.prefix)
		if got := occupancy(p, c.n); got != c.want {
			t.Errorf("occupancy(%s, %d) = %q, want %q", c.prefix, c.n, got, c.want)
		}
	}
}

func TestNamesOf_SamplesRatherThanFillsTheCell(t *testing.T) {
	// The table truncates a detail cell at about 44 columns, so a fact listing
	// nine names loses the count as well as the names.
	assets := []core.Asset{
		{Provider: "oci", ID: "a", Name: "alpha"},
		{Provider: "oci", ID: "b", Name: "bravo"},
		{Provider: "oci", ID: "c", Name: "charlie"},
	}
	if got, want := namesOf(assets, 2), "alpha, bravo +1 more"; got != want {
		t.Errorf("namesOf = %q, want %q", got, want)
	}
	if got := namesOf(assets, 5); got != "alpha, bravo, charlie" {
		t.Errorf("namesOf under the cap should list everything, got %q", got)
	}
	if got := namesOf(nil, 2); got != "" {
		t.Errorf("namesOf(nil) = %q, want empty", got)
	}

	// An unnamed asset falls back to its id: a blank cell reads as a bug in
	// the tool rather than as a missing name upstream.
	if got := namesOf([]core.Asset{{Provider: "oci", ID: "ocid1.x"}}, 2); got != "ocid1.x" {
		t.Errorf("namesOf on an unnamed asset = %q", got)
	}

	dedup := dedupeAssets(append(assets, assets...), 2)
	if len(dedup) != 2 || dedup[0].ID != "a" || dedup[1].ID != "b" {
		t.Errorf("dedupeAssets should keep the first n distinct assets in order, got %v", dedup)
	}
}
