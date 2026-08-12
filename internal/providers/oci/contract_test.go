package oci

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/containerengine"
	"github.com/oracle/oci-go-sdk/v65/containerinstances"
	occore "github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/database"
	"github.com/oracle/oci-go-sdk/v65/functions"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/keymanagement"
	"github.com/oracle/oci-go-sdk/v65/loadbalancer"
	"github.com/oracle/oci-go-sdk/v65/networkloadbalancer"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"

	icore "github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// ---------------------------------------------------------------------------
// Cross-package tag contracts.
//
// These tags are not decoration — other packages join on them. A mapper that
// stops emitting one still produces a perfectly valid-looking asset, and the
// damage shows up somewhere else entirely (a missing topology edge, a missing
// spreadsheet tab), which is exactly why it needs a test here at the source.
// ---------------------------------------------------------------------------

// TestTags_TopologyNetworkContainmentJoinKeys mirrors
// internal/topology/resolvers.go::ociContainmentRules. Every rule there looks
// up Tags[tagKey] in the asset index to draw a network-containment edge; if a
// mapper drops the tag, the resolver finds "" and silently skips the edge, so
// the whole OCI network backbone disappears from the diagram without a single
// error anywhere. This table is the source-side half of that contract and must
// stay in sync with ociContainmentRules.
func TestTags_TopologyNetworkContainmentJoinKeys(t *testing.T) {
	const (
		vcnOCID    = "ocid1.vcn.oc1..v1"
		subnetOCID = "ocid1.subnet.oc1..s1"
	)
	p := testProvider()

	cases := []struct {
		name     string
		asset    icore.Asset
		wantType string
		tagKey   string
		wantVal  string
	}{
		{
			name: "subnet points at its VCN",
			asset: p.subnetToAsset(occore.Subnet{
				Id: ptrString(subnetOCID), VcnId: ptrString(vcnOCID),
			}, "me-jeddah-1"),
			wantType: "oci.subnet", tagKey: "vcn_id", wantVal: vcnOCID,
		},
		{
			name: "nat gateway points at its VCN",
			asset: p.natGatewayToAsset(occore.NatGateway{
				Id: ptrString("ocid1.natgateway.oc1..n1"), VcnId: ptrString(vcnOCID),
			}, "me-jeddah-1"),
			wantType: "oci.nat_gateway", tagKey: "vcn_id", wantVal: vcnOCID,
		},
		{
			name: "internet gateway points at its VCN",
			asset: p.internetGatewayToAsset(occore.InternetGateway{
				Id: ptrString("ocid1.internetgateway.oc1..i1"), VcnId: ptrString(vcnOCID),
			}, "me-jeddah-1"),
			wantType: "oci.internet_gateway", tagKey: "vcn_id", wantVal: vcnOCID,
		},
		{
			name: "service gateway points at its VCN",
			asset: p.serviceGatewayToAsset(occore.ServiceGateway{
				Id: ptrString("ocid1.servicegateway.oc1..s1"), VcnId: ptrString(vcnOCID),
			}, "me-jeddah-1"),
			wantType: "oci.service_gateway", tagKey: "vcn_id", wantVal: vcnOCID,
		},
		{
			name: "local peering gateway points at its VCN",
			asset: p.localPeeringGatewayToAsset(occore.LocalPeeringGateway{
				Id: ptrString("ocid1.localpeeringgateway.oc1..l1"), VcnId: ptrString(vcnOCID),
			}, "me-jeddah-1"),
			wantType: "oci.local_peering_gateway", tagKey: "vcn_id", wantVal: vcnOCID,
		},
		{
			// This is the edge that anchors the entire Kubernetes subgraph
			// inside the OCI VCN hosting it — the only authoritative (non-IP-
			// guess) link between the two providers.
			name: "oke cluster points at its VCN",
			asset: p.okeClusterToAsset(containerengine.ClusterSummary{
				Id: ptrString("ocid1.cluster.oc1..c1"), VcnId: ptrString(vcnOCID),
			}, "me-jeddah-1"),
			wantType: "oci.oke.cluster", tagKey: "vcn_id", wantVal: vcnOCID,
		},
		{
			name: "network load balancer points at its subnet",
			asset: p.networkLoadBalancerToAsset(networkloadbalancer.NetworkLoadBalancerSummary{
				Id: ptrString("ocid1.networkloadbalancer.oc1..n1"), SubnetId: ptrString(subnetOCID),
			}, "me-jeddah-1"),
			wantType: "oci.network_load_balancer", tagKey: "subnet_id", wantVal: subnetOCID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.asset.Type != tc.wantType {
				t.Fatalf("Type = %q, want %q — ociContainmentRules keys on the exact type string, so a rename here silently drops every edge of this kind",
					tc.asset.Type, tc.wantType)
			}
			if got := tc.asset.Tags[tc.tagKey]; got != tc.wantVal {
				t.Errorf("Tags[%q] = %q, want %q — topology's ociNetworkContainment resolver joins on this exact tag; without it the containment edge vanishes with no error",
					tc.tagKey, got, tc.wantVal)
			}
		})
	}
}

// TestTags_CompartmentIDOnEveryAssetType: compartment_id is what
// `--sheet-by tag:compartment_id` partitions XLSX worksheets on (and what the
// XLSX renderer resolves back to a compartment *name* by matching the value
// against a compartment asset's ID). A mapper that omits it silently dumps
// those resources into an unlabelled bucket.
func TestTags_CompartmentIDOnEveryAssetType(t *testing.T) {
	const cid = "ocid1.compartment.oc1..app"
	p := testProvider()

	assets := map[string]icore.Asset{
		"compute":            p.computeInstanceToAsset(occore.Instance{Id: ptrString("i"), CompartmentId: ptrString(cid)}, "r"),
		"load_balancer":      p.loadBalancerToAsset(loadbalancer.LoadBalancer{Id: ptrString("lb"), CompartmentId: ptrString(cid)}, "r"),
		"nlb":                p.networkLoadBalancerToAsset(networkloadbalancer.NetworkLoadBalancerSummary{Id: ptrString("nlb"), CompartmentId: ptrString(cid)}, "r"),
		"block_volume":       p.blockVolumeToAsset(occore.Volume{Id: ptrString("bv"), CompartmentId: ptrString(cid)}, "r"),
		"boot_volume":        p.bootVolumeToAsset(occore.BootVolume{Id: ptrString("boot"), CompartmentId: ptrString(cid)}, "r"),
		"vcn":                p.vcnToAsset(occore.Vcn{Id: ptrString("v"), CompartmentId: ptrString(cid)}, "r"),
		"subnet":             p.subnetToAsset(occore.Subnet{Id: ptrString("s"), CompartmentId: ptrString(cid)}, "r"),
		"nat_gateway":        p.natGatewayToAsset(occore.NatGateway{Id: ptrString("n"), CompartmentId: ptrString(cid)}, "r"),
		"internet_gateway":   p.internetGatewayToAsset(occore.InternetGateway{Id: ptrString("ig"), CompartmentId: ptrString(cid)}, "r"),
		"service_gateway":    p.serviceGatewayToAsset(occore.ServiceGateway{Id: ptrString("sg"), CompartmentId: ptrString(cid)}, "r"),
		"local_peering_gw":   p.localPeeringGatewayToAsset(occore.LocalPeeringGateway{Id: ptrString("lpg"), CompartmentId: ptrString(cid)}, "r"),
		"drg":                p.drgToAsset(occore.Drg{Id: ptrString("drg"), CompartmentId: ptrString(cid)}, "r"),
		"bucket":             p.bucketToAsset(objectstorage.BucketSummary{Name: ptrString("b"), CompartmentId: ptrString(cid)}, "ns", "r"),
		"autonomous_db":      p.autonomousDatabaseToAsset(database.AutonomousDatabaseSummary{Id: ptrString("adb"), CompartmentId: ptrString(cid)}, "r"),
		"db_system":          p.dbSystemToAsset(database.DbSystemSummary{Id: ptrString("dbs"), CompartmentId: ptrString(cid)}, "r"),
		"fn_application":     p.functionsApplicationToAsset(functions.ApplicationSummary{Id: ptrString("app"), CompartmentId: ptrString(cid)}, "r"),
		"fn_function":        p.functionToAsset(functions.FunctionSummary{Id: ptrString("fn"), CompartmentId: ptrString(cid)}, "r"),
		"container_instance": p.containerInstanceToAsset(containerinstances.ContainerInstanceSummary{Id: ptrString("ci"), CompartmentId: ptrString(cid)}, "r"),
		"oke_cluster":        p.okeClusterToAsset(containerengine.ClusterSummary{Id: ptrString("c"), CompartmentId: ptrString(cid)}, "r"),
		"vault":              p.vaultToAsset(keymanagement.VaultSummary{Id: ptrString("vlt"), CompartmentId: ptrString(cid)}, "r"),
		"iam_policy":         p.policyToAsset(identity.Policy{Id: ptrString("pol"), CompartmentId: ptrString(cid)}),
		"iam_user":           p.userToAsset(identity.User{Id: ptrString("u"), CompartmentId: ptrString(cid)}),
		"iam_group":          p.groupToAsset(identity.Group{Id: ptrString("g"), CompartmentId: ptrString(cid)}),
		"iam_dynamic_group":  p.dynamicGroupToAsset(identity.DynamicGroup{Id: ptrString("dg"), CompartmentId: ptrString(cid)}),
	}

	for name, a := range assets {
		t.Run(name, func(t *testing.T) {
			if got := a.Tags["compartment_id"]; got != cid {
				t.Errorf("Tags[compartment_id] = %q, want %q — XLSX --sheet-by tag:compartment_id groups on this, and an absent tag drops the resource into an unlabelled sheet",
					got, cid)
			}
			// Every OCI asset must be attributable to the tenancy, since
			// AccountID is what the CLI/UI facet and group by.
			if a.AccountID != testProvider().tenancyOCID {
				t.Errorf("AccountID = %q, want the tenancy OCID %q", a.AccountID, testProvider().tenancyOCID)
			}
			if a.Provider != providerName {
				t.Errorf("Provider = %q, want %q", a.Provider, providerName)
			}
			if !strings.HasPrefix(a.Type, "oci.") {
				t.Errorf("Type = %q, want an \"oci.\"-prefixed type", a.Type)
			}
		})
	}
}

// TestTags_IdentityAssetsCarryNoRegion: identity is a global service. Stamping
// a region onto a user/group/policy would make the same principal appear once
// per scanned region in the output and in `auditor diff`.
func TestTags_IdentityAssetsCarryNoRegion(t *testing.T) {
	p := testProvider()
	for name, a := range map[string]icore.Asset{
		"policy":        p.policyToAsset(identity.Policy{Id: ptrString("pol")}),
		"user":          p.userToAsset(identity.User{Id: ptrString("u")}),
		"group":         p.groupToAsset(identity.Group{Id: ptrString("g")}),
		"dynamic_group": p.dynamicGroupToAsset(identity.DynamicGroup{Id: ptrString("dg")}),
		"compartment":   p.compartmentToAsset(identity.Compartment{Id: ptrString("c")}),
	} {
		if a.Region != "" {
			t.Errorf("%s Region = %q, want empty — identity resources are tenancy-global and a region here duplicates them per scanned region", name, a.Region)
		}
	}
}

// ---------------------------------------------------------------------------
// filterCompartments: subtree walk depth and ambiguity.
// ---------------------------------------------------------------------------

// TestFilterCompartments_DeepSubtreeAndDuplicateNames extends the existing
// two-level coverage. Two behaviours matter here and neither is obvious:
//
//   - selection must reach *arbitrarily* deep, not just one level. The walk is
//     upward through parent pointers, so a bug that stops after one hop still
//     passes a two-level test while silently dropping grandchildren.
//   - compartment names are NOT unique across an OCI tree. A name selector must
//     select every match and every match's subtree; picking only the first would
//     under-scope the audit, which is the dangerous direction.
func TestFilterCompartments_DeepSubtreeAndDuplicateNames(t *testing.T) {
	// root
	// ├── Production
	// │   └── Web
	// │       └── Edge            (3 levels below the selector)
	// ├── Sandbox
	// │   └── Web                 (duplicate name, different subtree)
	// │       └── Preview
	// └── Retired
	c := func(id, name, parent string) identity.Compartment {
		out := identity.Compartment{Id: ptrString(id), Name: ptrString(name)}
		if parent != "" {
			out.CompartmentId = ptrString(parent)
		}
		return out
	}
	const (
		root      = "ocid1.tenancy.oc1..root"
		prod      = "ocid1.compartment.oc1..prod"
		prodWeb   = "ocid1.compartment.oc1..prodweb"
		prodEdge  = "ocid1.compartment.oc1..prodedge"
		sandbox   = "ocid1.compartment.oc1..sandbox"
		sbxWeb    = "ocid1.compartment.oc1..sbxweb"
		sbxPrev   = "ocid1.compartment.oc1..sbxpreview"
		retired   = "ocid1.compartment.oc1..retired"
		unrelated = "ocid1.compartment.oc1..nope"
	)
	all := []identity.Compartment{
		c(root, "(tenancy root)", ""),
		c(prod, "Production", root),
		c(prodWeb, "Web", prod),
		c(prodEdge, "Edge", prodWeb),
		c(sandbox, "Sandbox", root),
		c(sbxWeb, "Web", sandbox),
		c(sbxPrev, "Preview", sbxWeb),
		c(retired, "Retired", root),
	}

	ids := func(cs []identity.Compartment) []string {
		out := make([]string, len(cs))
		for i, x := range cs {
			out[i] = derefStr(x.Id)
		}
		return out
	}

	cases := []struct {
		name string
		want []string // selectors
		ids  []string // expected OCIDs, in input order
		why  string
	}{
		{
			name: "selection reaches three levels deep",
			want: []string{"Production"},
			ids:  []string{prod, prodWeb, prodEdge},
			why:  "a parent-pointer walk that stops after one hop would drop Edge",
		},
		{
			name: "a duplicated name selects every match and both subtrees",
			want: []string{"Web"},
			ids:  []string{prodWeb, prodEdge, sbxWeb, sbxPrev},
			why:  "compartment names are not unique in OCI; selecting only the first match under-scopes the audit",
		},
		{
			name: "OCID selector is subtree-inclusive too",
			want: []string{sandbox},
			ids:  []string{sandbox, sbxWeb, sbxPrev},
			why:  "an OCID selector must behave exactly like a name selector",
		},
		{
			name: "mixed OCID and name selectors union",
			want: []string{prodEdge, "Retired"},
			ids:  []string{prodEdge, retired},
			why:  "selector kinds must compose",
		},
		{
			name: "an unknown OCID selects nothing rather than everything",
			want: []string{unrelated},
			ids:  nil,
			why:  "a no-match must not fall through to \"keep all\" — that would scan the whole tenancy when the operator asked for one compartment",
		},
		// Case-insensitive name matching is already covered by
		// TestFilterCompartments's "by name is case-insensitive" case; it is not
		// repeated here.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(filterCompartments(all, tc.want))
			if len(got) != len(tc.ids) {
				t.Fatalf("got %v, want %v (%s)", got, tc.ids, tc.why)
			}
			for i := range tc.ids {
				if got[i] != tc.ids[i] {
					t.Errorf("[%d] got %q, want %q (%s)", i, got[i], tc.ids[i], tc.why)
				}
			}
		})
	}
}

// TestFilterCompartments_CyclicParentChainTerminates: the upward walk is bounded
// by a step counter. A real tree never cycles, but a malformed API response (or
// a future bug that mis-populates parents) would otherwise hang the audit inside
// an infinite loop with no diagnostic at all. The test would hang, not fail, if
// the guard were removed — which is exactly the failure it prevents in prod.
func TestFilterCompartments_CyclicParentChainTerminates(t *testing.T) {
	a := identity.Compartment{Id: ptrString("ocid1.compartment.oc1..a"), Name: ptrString("A"), CompartmentId: ptrString("ocid1.compartment.oc1..b")}
	b := identity.Compartment{Id: ptrString("ocid1.compartment.oc1..b"), Name: ptrString("B"), CompartmentId: ptrString("ocid1.compartment.oc1..a")}

	done := make(chan []identity.Compartment, 1)
	go func() { done <- filterCompartments([]identity.Compartment{a, b}, []string{"Nonexistent"}) }()

	select {
	case got := <-done:
		if len(got) != 0 {
			t.Errorf("got %d compartments, want 0 for a non-matching selector", len(got))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("filterCompartments did not terminate on a cyclic parent chain — the step-count guard is gone")
	}
}

// TestFilterCompartments_UnsetVersusBlankSelector pins a distinction that is
// easy to erase while "simplifying" the blank-skipping loop:
//
//   - no selector at all (nil/empty slice) means "scan everything";
//   - a selector list that contains only blanks — what `--oci-compartments ""`
//     or an unexpanded CI variable produces — selects *nothing*.
//
// Collapsing the second case into the first would make a misconfigured flag be
// silently ignored and quietly scan the entire tenancy. Keeping it empty is
// loud instead: run() reports "matched no accessible compartment" and the
// operator finds out. Under-scoping loudly beats over-scoping silently.
func TestFilterCompartments_UnsetVersusBlankSelector(t *testing.T) {
	all := []identity.Compartment{
		{Id: ptrString("ocid1.tenancy.oc1..root"), Name: ptrString("(tenancy root)")},
		{Id: ptrString("ocid1.compartment.oc1..a"), Name: ptrString("A")},
	}

	for _, sel := range [][]string{nil, {}} {
		if got := filterCompartments(all, sel); len(got) != len(all) {
			t.Errorf("filterCompartments(all, %v) returned %d compartments, want all %d — an unset flag must not shrink the audit",
				sel, len(got), len(all))
		}
	}
	for _, sel := range [][]string{{""}, {"  "}, {"", " "}} {
		if got := filterCompartments(all, sel); len(got) != 0 {
			t.Errorf("filterCompartments(all, %v) returned %d compartments, want 0 — an all-blank selector must not be treated as \"scan everything\", or a misconfigured flag silently scans the whole tenancy",
				sel, len(got))
		}
	}
}

// ---------------------------------------------------------------------------
// Value helpers — small, but each guards a specific downstream lie.
// ---------------------------------------------------------------------------

// TestDerefTime_ZeroSDKTimeIsNilNotEpoch: an unset SDK timestamp must map to
// nil, never to Go's zero time. A 0001-01-01 CreatedAt would serialise into
// every snapshot and make `auditor diff` report the same phantom drift forever
// — noise that trains operators to ignore the tool.
func TestDerefTime_ZeroSDKTimeIsNilNotEpoch(t *testing.T) {
	if got := derefTime(&common.SDKTime{}); got != nil {
		t.Errorf("derefTime(zero) = %v, want nil (a zero timestamp in output produces permanent phantom drift in auditor diff)", got)
	}
	if got := derefTime(nil); got != nil {
		t.Errorf("derefTime(nil) = %v, want nil", got)
	}
	want := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	got := derefTime(&common.SDKTime{Time: want})
	if got == nil || !got.Equal(want) {
		t.Errorf("derefTime = %v, want %v", got, want)
	}
	// The returned pointer must not alias the SDK's value in a way that lets a
	// caller mutate the source struct.
	src := common.SDKTime{Time: want}
	out := derefTime(&src)
	*out = out.Add(24 * time.Hour)
	if !src.Equal(want) {
		t.Errorf("mutating the returned *time.Time changed the SDK struct: %v", src.Time)
	}
}

// TestOptionalScalarFormatters_UnsetIsEmptyNotZero: an unset *bool/*int must
// render as "" rather than "false"/"0". The distinction is real — "is this
// internet gateway disabled?" and "we don't know" are different findings, and a
// fabricated "false" is worse than a blank because it reads as authoritative.
func TestOptionalScalarFormatters_UnsetIsEmptyNotZero(t *testing.T) {
	if got := boolStr(nil); got != "" {
		t.Errorf("boolStr(nil) = %q, want \"\" — a fabricated \"false\" reads as an authoritative answer", got)
	}
	if got := i64Str(nil); got != "" {
		t.Errorf("i64Str(nil) = %q, want \"\" — \"0\" would read as a real zero-byte volume", got)
	}
	if got := iStr(nil); got != "" {
		t.Errorf("iStr(nil) = %q, want \"\"", got)
	}
	if got := boolStr(ptrBool(false)); got != "false" {
		t.Errorf("boolStr(&false) = %q, want \"false\"", got)
	}
	if got := i64Str(ptrInt64(0)); got != "0" {
		t.Errorf("i64Str(&0) = %q, want \"0\" — an explicit zero must survive", got)
	}
	if got := iStr(ptrIntVal(0)); got != "0" {
		t.Errorf("iStr(&0) = %q, want \"0\"", got)
	}
}

// TestMergeFreeformTags_ResultIsIndependentOfSDKMap: the returned map must be a
// fresh copy in both directions. Aliasing the SDK's map means later mutations to
// the asset's Tags corrupt the struct that Raw is marshalled from (and vice
// versa) — a data-dependent bug that would only appear with --include-raw.
func TestMergeFreeformTags_ResultIsIndependentOfSDKMap(t *testing.T) {
	sdk := map[string]string{"env": "prod"}
	got := mergeFreeformTags(sdk, [2]string{"compartment_id", "ocid1.compartment.oc1..a"})

	got["env"] = "MUTATED"
	got["injected"] = "yes"
	if sdk["env"] != "prod" {
		t.Errorf("mutating the merged map changed the SDK's map: env = %q, want \"prod\"", sdk["env"])
	}
	if _, leaked := sdk["injected"]; leaked {
		t.Error("a key added to the merged map appeared in the SDK's map — the two alias the same storage")
	}

	sdk["env"] = "CHANGED-LATER"
	if got["env"] != "MUTATED" {
		t.Errorf("mutating the SDK map changed the merged map: env = %q", got["env"])
	}

	// An extra must win over a freeform tag of the same name, so a
	// user-controlled label cannot spoof a structural tag the topology and
	// XLSX layers join on.
	spoof := mergeFreeformTags(map[string]string{"vcn_id": "attacker-controlled"},
		[2]string{"vcn_id", "ocid1.vcn.oc1..real"})
	if spoof["vcn_id"] != "ocid1.vcn.oc1..real" {
		t.Errorf("Tags[vcn_id] = %q, want the structural value — a freeform tag must not override a join key", spoof["vcn_id"])
	}
}

// TestRawOf_OmittedUnlessIncludeRaw: Raw is opt-in. It carries the entire SDK
// response, so leaking it by default would bloat every snapshot and widen the
// blast radius of any field the SDK adds later.
func TestRawOf_OmittedUnlessIncludeRaw(t *testing.T) {
	off := &Provider{cfg: Config{IncludeRaw: false}}
	if raw := off.rawOf(occore.Instance{Id: ptrString("i")}); raw != nil {
		t.Errorf("rawOf with IncludeRaw=false returned %s, want nil", raw)
	}
	on := &Provider{cfg: Config{IncludeRaw: true}}
	if raw := on.rawOf(occore.Instance{Id: ptrString("i")}); raw == nil {
		t.Error("rawOf with IncludeRaw=true returned nil")
	}
	// A value json.Marshal cannot handle must degrade to nil, not panic — a
	// single unmarshalable field must not abort a whole audit.
	if raw := on.rawOf(make(chan int)); raw != nil {
		t.Errorf("rawOf of an unmarshalable value = %s, want nil", raw)
	}
}

// TestSendAsset_RespectsCancellation is the streaming contract's cancel path:
// a blocked send on an unread channel must abandon rather than pin the
// goroutine forever after Ctrl+C.
func TestSendAsset_RespectsCancellation(t *testing.T) {
	full := make(chan icore.Asset) // unbuffered, nobody reading
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sendAsset(ctx, full, icore.Asset{ID: "x"}) {
		t.Error("sendAsset reported success on a cancelled context")
	}

	buffered := make(chan icore.Asset, 1)
	if !sendAsset(context.Background(), buffered, icore.Asset{ID: "x"}) {
		t.Error("sendAsset failed on a live context with a free buffer slot")
	}
}
