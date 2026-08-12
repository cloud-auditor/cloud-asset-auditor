package oci

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"

	icore "github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// ---------------------------------------------------------------------------
// Wire-level test harness.
//
// These tests drive the *real* oci-go-sdk clients against a local httptest
// server. That is deliberate and it is the whole point: the SDK — not our code
// — owns the pagination query encoding, the response decoding, and the error
// typing. A hand-written fake client would mirror those behaviours and would
// therefore test the fake, letting a genuine SDK-contract break through.
//
// The one production affordance is Provider.endpoint (see oci.go::retarget).
// The SDK exposes no endpoint env var and no client option for this, and
// BaseClient.SetRegion unconditionally recomputes Host from the region name, so
// an override applied *after* SetRegion is the only available seam. endpoint is
// "" in production, so retarget is a no-op there.
//
// Nothing here touches the network: httptest listens on loopback, and the
// signing key is generated in-process.
// ---------------------------------------------------------------------------

const testTenancyOCID = "ocid1.tenancy.oc1..root"

// OCI resource paths, discovered by running every collector against a
// recording server. Each carries a dated per-service API version prefix
// (/20160918, /20200501, ...), so tests match on the trailing segment.
const (
	pathInstances      = "/instances"
	pathLoadBalancers  = "/loadBalancers"
	pathNLBs           = "/networkLoadBalancers"
	pathVolumes        = "/volumes"
	pathBootVolumes    = "/bootVolumes"
	pathVCNs           = "/vcns"
	pathSubnets        = "/subnets"
	pathNATGateways    = "/natGateways"
	pathIGWs           = "/internetGateways"
	pathSGWs           = "/serviceGateways"
	pathLPGs           = "/localPeeringGateways"
	pathDRGs           = "/drgs"
	pathNamespace      = "/n"
	pathBuckets        = "/b"
	pathAutonomousDBs  = "/autonomousDatabases"
	pathDBSystems      = "/dbSystems"
	pathApplications   = "/applications"
	pathFunctions      = "/functions"
	pathContainerInsts = "/containerInstances"
	pathClusters       = "/clusters"
	pathVaults         = "/vaults"
	pathCompartments   = "/compartments"
	pathPolicies       = "/policies"
	pathUsers          = "/users"
	pathGroups         = "/groups"
	pathDynamicGroups  = "/dynamicGroups"
	pathRegionSubs     = "/regionSubscriptions"
)

var (
	testAuthOnce sync.Once
	testAuthCfg  common.ConfigurationProvider
)

// testAuth builds a request signer from a throwaway in-process RSA key. It is
// not a credential — the fake server never verifies the signature — but the SDK
// refuses to build a request without a usable key, so one is required to reach
// the transport at all. Generated exactly once: RSA keygen is by far the most
// expensive operation in this file.
func testAuth(t *testing.T) common.ConfigurationProvider {
	t.Helper()
	testAuthOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate test signing key: %v", err)
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		})
		testAuthCfg = common.NewRawConfigurationProvider(
			testTenancyOCID, "ocid1.user.oc1..tester", "us-ashburn-1",
			"aa:bb:cc:dd", string(pemBytes), nil,
		)
	})
	return testAuthCfg
}

type wireReq struct {
	Path  string
	Query url.Values
}

// fakeOCI is an httptest server standing in for the OCI API. It records every
// request the SDK makes so tests can assert on call counts and query params —
// which is how "did we actually follow the cursor / did we scan this once per
// compartment" is proven rather than assumed.
type fakeOCI struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []wireReq
}

func newFakeOCI(t *testing.T, h http.HandlerFunc) *fakeOCI {
	t.Helper()
	f := &fakeOCI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, wireReq{Path: r.URL.Path, Query: r.URL.Query()})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

// hits counts requests whose path ends in suffix.
func (f *fakeOCI) hits(suffix string) int {
	return len(f.queriesFor(suffix))
}

// queriesFor returns the query params of every request whose path ends in
// suffix, in call order.
func (f *fakeOCI) queriesFor(suffix string) []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []url.Values
	for _, r := range f.reqs {
		if strings.HasSuffix(r.Path, suffix) {
			out = append(out, r.Query)
		}
	}
	return out
}

// wiredProvider builds a Provider pointed at the fake server, with auth
// injected. It pre-trips authOnce so ensureAuth hands back the injected values
// instead of walking the real credential chain (which would probe IMDS and read
// ~/.oci/config — neither is acceptable in a unit test).
func wiredProvider(t *testing.T, f *fakeOCI, cfg Config) *Provider {
	t.Helper()
	p := New(cfg)
	p.auth = testAuth(t)
	p.tenancyOCID = testTenancyOCID
	p.homeRegion = "us-ashburn-1"
	p.endpoint = f.URL
	p.authOnce.Do(func() {})
	return p
}

// drain runs a Collect and returns everything both channels produced. It fails
// the test if either channel is still open after the deadline, which is how the
// "both channels MUST close exactly once" provider contract is enforced.
func drain(t *testing.T, ctx context.Context, p *Provider) ([]icore.Asset, []error) {
	t.Helper()
	assets, errs := p.Collect(ctx)

	var (
		gotAssets []icore.Asset
		gotErrs   []error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for assets != nil || errs != nil {
			select {
			case a, ok := <-assets:
				if !ok {
					assets = nil
					continue
				}
				gotAssets = append(gotAssets, a)
			case e, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				gotErrs = append(gotErrs, e)
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Collect did not close both channels; a collector is stuck or a channel was never closed")
	}
	return gotAssets, gotErrs
}

// jsonArray/jsonItems build the two body shapes the OCI list APIs use: most
// return a bare array, a few newer services wrap it in {"items": [...]}.
func jsonArray(items ...string) string { return "[" + strings.Join(items, ",") + "]" }
func jsonItems(items ...string) string { return `{"items":` + jsonArray(items...) + `}` }

// ---------------------------------------------------------------------------
// Compartment recursion — the canonical OCI mistake.
// ---------------------------------------------------------------------------

// TestListCompartments_RecursesSubtreeAndPaginates pins the single most
// consequential request in this provider. Two independent ways to silently
// halve a tenancy's inventory are guarded here:
//
//  1. dropping CompartmentIdInSubtree=true, which limits the listing to the
//     root's direct children — the mistake CLAUDE.md calls "the canonical OCI
//     mistake"; and
//  2. dropping the opc-next-page cursor, which truncates at page 1.
//
// Both fail with a successful-looking, error-free, smaller result — the worst
// possible failure mode for an audit tool.
func TestListCompartments_RecursesSubtreeAndPaginates(t *testing.T) {
	f := newFakeOCI(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, pathCompartments) {
			http.Error(w, `{"code":"NotFound"}`, http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("opc-next-page", "CURSOR-2")
			fmt.Fprint(w, jsonArray(`{"id":"ocid1.compartment.oc1..prod","name":"Production","compartmentId":"`+testTenancyOCID+`","lifecycleState":"ACTIVE"}`))
			return
		}
		fmt.Fprint(w, jsonArray(`{"id":"ocid1.compartment.oc1..web","name":"Web","compartmentId":"ocid1.compartment.oc1..prod","lifecycleState":"ACTIVE"}`))
	})

	p := wiredProvider(t, f, Config{})
	got, err := p.listCompartments(context.Background())
	if err != nil {
		t.Fatalf("listCompartments: %v", err)
	}

	q := f.queriesFor(pathCompartments)
	if len(q) != 2 {
		t.Fatalf("made %d ListCompartments calls, want 2 (page 2 was never requested — the opc-next-page cursor was dropped)", len(q))
	}
	if q[0].Get("compartmentIdInSubtree") != "true" {
		t.Errorf("compartmentIdInSubtree = %q, want \"true\" — without it OCI returns only the root's direct children and every nested compartment vanishes from the audit",
			q[0].Get("compartmentIdInSubtree"))
	}
	if q[0].Get("compartmentId") != testTenancyOCID {
		t.Errorf("compartmentId = %q, want the tenancy root %q — recursion must start at the tenancy, not an arbitrary compartment",
			q[0].Get("compartmentId"), testTenancyOCID)
	}
	if got := q[0].Get("accessLevel"); got != "ACCESSIBLE" {
		t.Errorf("accessLevel = %q, want ACCESSIBLE", got)
	}
	if got := q[0].Get("lifecycleState"); got != "ACTIVE" {
		t.Errorf("lifecycleState = %q, want ACTIVE", got)
	}
	if q[1].Get("page") != "CURSOR-2" {
		t.Errorf("page 2 sent page=%q, want the server's opc-next-page value \"CURSOR-2\"", q[1].Get("page"))
	}

	// Root is synthesized (ListCompartments never returns the tenancy itself)
	// and must come first so callers can rely on a uniform slice.
	wantIDs := []string{testTenancyOCID, "ocid1.compartment.oc1..prod", "ocid1.compartment.oc1..web"}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d compartments, want %d (%v)", len(got), len(wantIDs), wantIDs)
	}
	for i, want := range wantIDs {
		if id := derefStr(got[i].Id); id != want {
			t.Errorf("compartment[%d] = %q, want %q", i, id, want)
		}
	}
	if got[0].CompartmentId != nil {
		t.Errorf("synthesized tenancy root has parent %q, want nil — a parent pointer here would make filterCompartments' upward walk chase a non-existent compartment",
			derefStr(got[0].CompartmentId))
	}
}

// TestListCompartments_ErrorSurfacesRatherThanTruncating guards the failure
// direction: a mid-pagination error must be reported, not swallowed into a
// short-but-successful list. Returning nil here would let a permissions problem
// read as "this tenancy has two compartments".
func TestListCompartments_ErrorSurfacesRatherThanTruncating(t *testing.T) {
	f := newFakeOCI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"code":"NotAuthorizedOrNotFound","message":"Authorization failed"}`)
	})

	p := wiredProvider(t, f, Config{})
	_, err := p.listCompartments(context.Background())
	if err == nil {
		t.Fatal("a 403 from ListCompartments returned no error; the audit would silently report only the tenancy root")
	}
	if !strings.Contains(err.Error(), "list compartments") {
		t.Errorf("error = %q, want it to name the failing operation (\"list compartments\")", err)
	}
}

// ---------------------------------------------------------------------------
// Pagination across every collector.
// ---------------------------------------------------------------------------

// TestCollectors_FollowOpcNextPage is the highest-blast-radius test in this
// file. All ~19 collectors hand-roll the same `for { ...; page = resp.OpcNextPage }`
// loop, and each fails identically if the cursor is dropped: the audit
// truncates at the first page, emits no error, and looks completely healthy. A
// compartment with 400 instances would report 100.
//
// Each case serves exactly two pages — page 1 with an opc-next-page header,
// page 2 without — so a collector that ignores the cursor yields one asset
// instead of two and this table catches it.
func TestCollectors_FollowOpcNextPage(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wrapped bool // {"items":[...]} instead of a bare array
		item1   string
		item2   string
		wantIDs []string
		collect func(p *Provider, out chan<- icore.Asset) error
	}{
		{
			name: "compute instances", path: pathInstances,
			item1:   `{"id":"ocid1.instance.oc1..a","displayName":"web-1"}`,
			item2:   `{"id":"ocid1.instance.oc1..b","displayName":"web-2"}`,
			wantIDs: []string{"ocid1.instance.oc1..a", "ocid1.instance.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectComputeInstances(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "load balancers", path: pathLoadBalancers,
			item1:   `{"id":"ocid1.loadbalancer.oc1..a","displayName":"lb-1"}`,
			item2:   `{"id":"ocid1.loadbalancer.oc1..b","displayName":"lb-2"}`,
			wantIDs: []string{"ocid1.loadbalancer.oc1..a", "ocid1.loadbalancer.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectLoadBalancers(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "network load balancers", path: pathNLBs, wrapped: true,
			item1:   `{"id":"ocid1.networkloadbalancer.oc1..a","displayName":"nlb-1"}`,
			item2:   `{"id":"ocid1.networkloadbalancer.oc1..b","displayName":"nlb-2"}`,
			wantIDs: []string{"ocid1.networkloadbalancer.oc1..a", "ocid1.networkloadbalancer.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectNetworkLoadBalancers(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "block volumes", path: pathVolumes,
			item1:   `{"id":"ocid1.volume.oc1..a","displayName":"vol-1"}`,
			item2:   `{"id":"ocid1.volume.oc1..b","displayName":"vol-2"}`,
			wantIDs: []string{"ocid1.volume.oc1..a", "ocid1.volume.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectBlockVolumes(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "boot volumes", path: pathBootVolumes,
			item1:   `{"id":"ocid1.bootvolume.oc1..a","displayName":"boot-1"}`,
			item2:   `{"id":"ocid1.bootvolume.oc1..b","displayName":"boot-2"}`,
			wantIDs: []string{"ocid1.bootvolume.oc1..a", "ocid1.bootvolume.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectBootVolumes(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "vcns", path: pathVCNs,
			item1:   `{"id":"ocid1.vcn.oc1..a","displayName":"vcn-1"}`,
			item2:   `{"id":"ocid1.vcn.oc1..b","displayName":"vcn-2"}`,
			wantIDs: []string{"ocid1.vcn.oc1..a", "ocid1.vcn.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectVCNs(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "subnets", path: pathSubnets,
			item1:   `{"id":"ocid1.subnet.oc1..a","displayName":"sub-1"}`,
			item2:   `{"id":"ocid1.subnet.oc1..b","displayName":"sub-2"}`,
			wantIDs: []string{"ocid1.subnet.oc1..a", "ocid1.subnet.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectSubnets(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "nat gateways", path: pathNATGateways,
			item1:   `{"id":"ocid1.natgateway.oc1..a","displayName":"nat-1"}`,
			item2:   `{"id":"ocid1.natgateway.oc1..b","displayName":"nat-2"}`,
			wantIDs: []string{"ocid1.natgateway.oc1..a", "ocid1.natgateway.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectNATGateways(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "internet gateways", path: pathIGWs,
			item1:   `{"id":"ocid1.internetgateway.oc1..a","displayName":"igw-1"}`,
			item2:   `{"id":"ocid1.internetgateway.oc1..b","displayName":"igw-2"}`,
			wantIDs: []string{"ocid1.internetgateway.oc1..a", "ocid1.internetgateway.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectInternetGateways(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "service gateways", path: pathSGWs,
			item1:   `{"id":"ocid1.servicegateway.oc1..a","displayName":"sgw-1"}`,
			item2:   `{"id":"ocid1.servicegateway.oc1..b","displayName":"sgw-2"}`,
			wantIDs: []string{"ocid1.servicegateway.oc1..a", "ocid1.servicegateway.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectServiceGateways(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "local peering gateways", path: pathLPGs,
			item1:   `{"id":"ocid1.localpeeringgateway.oc1..a","displayName":"lpg-1"}`,
			item2:   `{"id":"ocid1.localpeeringgateway.oc1..b","displayName":"lpg-2"}`,
			wantIDs: []string{"ocid1.localpeeringgateway.oc1..a", "ocid1.localpeeringgateway.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectLocalPeeringGateways(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "drgs", path: pathDRGs,
			item1:   `{"id":"ocid1.drg.oc1..a","displayName":"drg-1"}`,
			item2:   `{"id":"ocid1.drg.oc1..b","displayName":"drg-2"}`,
			wantIDs: []string{"ocid1.drg.oc1..a", "ocid1.drg.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectDRGs(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			// Buckets have no OCID in the list response, so Name is the ID.
			name: "object storage buckets", path: pathBuckets,
			item1:   `{"name":"bkt-1","compartmentId":"cid"}`,
			item2:   `{"name":"bkt-2","compartmentId":"cid"}`,
			wantIDs: []string{"bkt-1", "bkt-2"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectObjectStorageBuckets(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "autonomous databases", path: pathAutonomousDBs,
			item1:   `{"id":"ocid1.autonomousdatabase.oc1..a","displayName":"adb-1"}`,
			item2:   `{"id":"ocid1.autonomousdatabase.oc1..b","displayName":"adb-2"}`,
			wantIDs: []string{"ocid1.autonomousdatabase.oc1..a", "ocid1.autonomousdatabase.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectAutonomousDatabases(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "db systems", path: pathDBSystems,
			item1:   `{"id":"ocid1.dbsystem.oc1..a","displayName":"dbs-1"}`,
			item2:   `{"id":"ocid1.dbsystem.oc1..b","displayName":"dbs-2"}`,
			wantIDs: []string{"ocid1.dbsystem.oc1..a", "ocid1.dbsystem.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectDBSystems(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			// Functions nests a second paginated call inside the application
			// loop; the inner /functions endpoint returns [] here so this case
			// isolates the outer applications cursor.
			name: "functions applications", path: pathApplications,
			item1:   `{"id":"ocid1.fnapp.oc1..a","displayName":"app-1"}`,
			item2:   `{"id":"ocid1.fnapp.oc1..b","displayName":"app-2"}`,
			wantIDs: []string{"ocid1.fnapp.oc1..a", "ocid1.fnapp.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectFunctions(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "container instances", path: pathContainerInsts, wrapped: true,
			item1:   `{"id":"ocid1.containerinstance.oc1..a","displayName":"ci-1"}`,
			item2:   `{"id":"ocid1.containerinstance.oc1..b","displayName":"ci-2"}`,
			wantIDs: []string{"ocid1.containerinstance.oc1..a", "ocid1.containerinstance.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectContainerInstances(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			// OKE ClusterSummary uses Name, not DisplayName.
			name: "oke clusters", path: pathClusters,
			item1:   `{"id":"ocid1.cluster.oc1..a","name":"oke-1"}`,
			item2:   `{"id":"ocid1.cluster.oc1..b","name":"oke-2"}`,
			wantIDs: []string{"ocid1.cluster.oc1..a", "ocid1.cluster.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectOKEClusters(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "vaults", path: pathVaults,
			item1:   `{"id":"ocid1.vault.oc1..a","displayName":"vault-1"}`,
			item2:   `{"id":"ocid1.vault.oc1..b","displayName":"vault-2"}`,
			wantIDs: []string{"ocid1.vault.oc1..a", "ocid1.vault.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectVaults(context.Background(), "us-ashburn-1", "cid", out)
			},
		},
		{
			name: "iam policies", path: pathPolicies,
			item1:   `{"id":"ocid1.policy.oc1..a","name":"pol-1"}`,
			item2:   `{"id":"ocid1.policy.oc1..b","name":"pol-2"}`,
			wantIDs: []string{"ocid1.policy.oc1..a", "ocid1.policy.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectPolicies(context.Background(), "cid", out)
			},
		},
		{
			name: "iam users", path: pathUsers,
			item1:   `{"id":"ocid1.user.oc1..a","name":"alice"}`,
			item2:   `{"id":"ocid1.user.oc1..b","name":"bob"}`,
			wantIDs: []string{"ocid1.user.oc1..a", "ocid1.user.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectUsers(context.Background(), out)
			},
		},
		{
			name: "iam groups", path: pathGroups,
			item1:   `{"id":"ocid1.group.oc1..a","name":"Admins"}`,
			item2:   `{"id":"ocid1.group.oc1..b","name":"Devs"}`,
			wantIDs: []string{"ocid1.group.oc1..a", "ocid1.group.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectGroups(context.Background(), out)
			},
		},
		{
			name: "iam dynamic groups", path: pathDynamicGroups,
			item1:   `{"id":"ocid1.dynamicgroup.oc1..a","name":"oke-nodes"}`,
			item2:   `{"id":"ocid1.dynamicgroup.oc1..b","name":"fn-nodes"}`,
			wantIDs: []string{"ocid1.dynamicgroup.oc1..a", "ocid1.dynamicgroup.oc1..b"},
			collect: func(p *Provider, out chan<- icore.Asset) error {
				return p.collectDynamicGroups(context.Background(), out)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := func(item string) string {
				if tc.wrapped {
					return jsonItems(item)
				}
				return jsonArray(item)
			}
			f := newFakeOCI(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == pathNamespace:
					// Object Storage resolves the tenancy namespace first.
					fmt.Fprint(w, `"testns"`)
				case strings.HasSuffix(r.URL.Path, tc.path):
					if r.URL.Query().Get("page") == "" {
						w.Header().Set("opc-next-page", "CURSOR-2")
						fmt.Fprint(w, body(tc.item1))
						return
					}
					fmt.Fprint(w, body(tc.item2))
				default:
					// Any nested call (e.g. functions-within-application) gets
					// an empty page so this case isolates one cursor.
					fmt.Fprint(w, `[]`)
				}
			})

			p := wiredProvider(t, f, Config{})
			out := make(chan icore.Asset, 16)
			if err := tc.collect(p, out); err != nil {
				t.Fatalf("collect: %v", err)
			}
			close(out)

			var gotIDs []string
			for a := range out {
				gotIDs = append(gotIDs, a.ID)
			}

			if n := f.hits(tc.path); n != 2 {
				t.Errorf("made %d list calls to %s, want 2 — page 2 was never requested, so this collector silently truncates every result set larger than one page",
					n, tc.path)
			}
			if q := f.queriesFor(tc.path); len(q) == 2 && q[1].Get("page") != "CURSOR-2" {
				t.Errorf("page 2 sent page=%q, want the server's opc-next-page value \"CURSOR-2\"", q[1].Get("page"))
			}
			if len(gotIDs) != len(tc.wantIDs) {
				t.Fatalf("emitted %d assets %v, want %d %v", len(gotIDs), gotIDs, len(tc.wantIDs), tc.wantIDs)
			}
			for i, want := range tc.wantIDs {
				if gotIDs[i] != want {
					t.Errorf("asset[%d].ID = %q, want %q", i, gotIDs[i], want)
				}
			}
		})
	}
}

// TestCollectFunctions_PaginatesFunctionsWithinApplication covers the one
// nested cursor the table above deliberately isolates: functions live under an
// application, so collectFunctions runs an inner paginated loop per app.
// Dropping that inner cursor loses functions while still reporting every
// application, which reads as a correct-looking result.
func TestCollectFunctions_PaginatesFunctionsWithinApplication(t *testing.T) {
	f := newFakeOCI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, pathApplications):
			fmt.Fprint(w, jsonArray(`{"id":"ocid1.fnapp.oc1..a","displayName":"payments"}`))
		case strings.HasSuffix(r.URL.Path, pathFunctions):
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("opc-next-page", "FN-2")
				fmt.Fprint(w, jsonArray(`{"id":"ocid1.fnfunc.oc1..a","displayName":"charge","applicationId":"ocid1.fnapp.oc1..a"}`))
				return
			}
			fmt.Fprint(w, jsonArray(`{"id":"ocid1.fnfunc.oc1..b","displayName":"refund","applicationId":"ocid1.fnapp.oc1..a"}`))
		default:
			fmt.Fprint(w, `[]`)
		}
	})

	p := wiredProvider(t, f, Config{})
	out := make(chan icore.Asset, 16)
	if err := p.collectFunctions(context.Background(), "us-ashburn-1", "cid", out); err != nil {
		t.Fatalf("collectFunctions: %v", err)
	}
	close(out)

	var types, ids []string
	for a := range out {
		types = append(types, a.Type)
		ids = append(ids, a.ID)
	}

	if n := f.hits(pathFunctions); n != 2 {
		t.Errorf("made %d ListFunctions calls, want 2 — the inner cursor was dropped and functions past page 1 are lost", n)
	}
	// One application asset + both functions.
	want := []string{"ocid1.fnapp.oc1..a", "ocid1.fnfunc.oc1..a", "ocid1.fnfunc.oc1..b"}
	if len(ids) != len(want) {
		t.Fatalf("emitted %v (types %v), want %v", ids, types, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("asset[%d].ID = %q, want %q", i, ids[i], want[i])
		}
	}
	// The application is queried by ApplicationId, not compartment — a
	// refactor that passed the compartment here would return every function in
	// the compartment and mis-attribute them.
	if q := f.queriesFor(pathFunctions); len(q) > 0 && q[0].Get("applicationId") != "ocid1.fnapp.oc1..a" {
		t.Errorf("ListFunctions applicationId = %q, want the parent application's OCID", q[0].Get("applicationId"))
	}
}

// ---------------------------------------------------------------------------
// Object Storage namespace caching.
// ---------------------------------------------------------------------------

// TestObjectStorageNamespace_ResolvedOnceAndSharedAcrossCollectors pins the
// sync.Once. The namespace is tenancy-global, and the bucket collector runs
// once per (region × compartment) — dropping the cache would add a GetNamespace
// round-trip to every one of them (a 10-region, 40-compartment tenancy pays 400
// extra calls and risks throttling), while still producing correct output, so
// nothing but a call-count assertion catches it.
func TestObjectStorageNamespace_ResolvedOnceAndSharedAcrossCollectors(t *testing.T) {
	f := newFakeOCI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == pathNamespace:
			fmt.Fprint(w, `"axtenancyns"`)
		case strings.HasSuffix(r.URL.Path, pathBuckets):
			fmt.Fprint(w, jsonArray(`{"name":"bkt","compartmentId":"`+r.URL.Query().Get("compartmentId")+`"}`))
		default:
			fmt.Fprint(w, `[]`)
		}
	})

	p := wiredProvider(t, f, Config{})
	out := make(chan icore.Asset, 32)
	// Three separate collector invocations across two regions and two
	// compartments — exactly the fan-out shape run() produces.
	for _, call := range [][2]string{
		{"us-ashburn-1", "cid-a"},
		{"us-phoenix-1", "cid-a"},
		{"us-phoenix-1", "cid-b"},
	} {
		if err := p.collectObjectStorageBuckets(context.Background(), call[0], call[1], out); err != nil {
			t.Fatalf("collectObjectStorageBuckets(%v): %v", call, err)
		}
	}
	close(out)

	if n := f.hits(pathNamespace); n != 1 {
		t.Errorf("GetNamespace called %d times, want exactly 1 — the sync.Once cache is gone and every bucket collector now pays a tenancy-global lookup", n)
	}
	if n := f.hits(pathBuckets); n != 3 {
		t.Errorf("ListBuckets called %d times, want 3 (one per region×compartment invocation)", n)
	}

	var count int
	for a := range out {
		count++
		if a.Tags["namespace"] != "axtenancyns" {
			t.Errorf("bucket %q namespace tag = %q, want the shared %q", a.ID, a.Tags["namespace"], "axtenancyns")
		}
	}
	if count != 3 {
		t.Errorf("emitted %d buckets, want 3", count)
	}
}

// TestObjectStorageNamespace_FailureSurfacesAsError guards against the worst
// possible handling of a namespace lookup failure: returning an empty bucket
// list with no error, which would report "this tenancy has no object storage"
// — indistinguishable from the truth and catastrophic for an audit.
//
// See TestObjectStorageNamespace_FailureIsNotCached for the other half:
// the failure must not be cached.
func TestObjectStorageNamespace_FailureSurfacesAsError(t *testing.T) {
	f := newFakeOCI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathNamespace {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"code":"NotAuthorizedOrNotFound","message":"denied"}`)
			return
		}
		fmt.Fprint(w, `[]`)
	})

	p := wiredProvider(t, f, Config{})
	out := make(chan icore.Asset, 4)
	err := p.collectObjectStorageBuckets(context.Background(), "us-ashburn-1", "cid", out)
	if err == nil {
		t.Fatal("namespace lookup failed but collectObjectStorageBuckets returned nil; the audit would silently claim the tenancy has no buckets")
	}
	if !strings.Contains(err.Error(), "object storage namespace") {
		t.Errorf("error = %q, want it to name the namespace lookup", err)
	}
	if n := f.hits(pathBuckets); n != 0 {
		t.Errorf("ListBuckets was called %d times despite an unresolved namespace, want 0", n)
	}
}

// A namespace lookup failure must NOT be cached. The namespace is required to
// list any bucket, so caching one error — a permissions blip, a timeout, a
// 5xx — used to drop Object Storage from every region for the rest of the
// process, while handing each later caller the stale error as though it were
// freshly observed. Silently losing a whole resource type mid-audit is the
// failure mode an inventory tool can least afford.
//
// The first response is a 403 rather than a 500 on purpose: the SDK retries
// 5xx internally, so a 500 would resolve inside the first call and this would
// measure the SDK's backoff instead of our caching.
func TestObjectStorageNamespace_FailureIsNotCached(t *testing.T) {
	var nsCalls int
	f := newFakeOCI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathNamespace {
			nsCalls++
			if nsCalls == 1 {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"code":"NotAuthorizedOrNotFound","message":"denied"}`)
				return
			}
			fmt.Fprint(w, `"acmens"`)
			return
		}
		fmt.Fprint(w, `[]`)
	})

	p := wiredProvider(t, f, Config{})
	out := make(chan icore.Asset, 4)

	if err := p.collectObjectStorageBuckets(context.Background(), "us-ashburn-1", "cid", out); err == nil {
		t.Fatal("first call: want the failure surfaced, got nil")
	}
	if err := p.collectObjectStorageBuckets(context.Background(), "us-ashburn-1", "cid", out); err != nil {
		t.Fatalf("second call: the failure was cached, so bucket collection stayed dead: %v", err)
	}
	if nsCalls != 2 {
		t.Errorf("GetNamespace called %d times, want 2 (the failure must not be cached)", nsCalls)
	}
	if n := f.hits(pathBuckets); n != 1 {
		t.Errorf("ListBuckets called %d times, want 1 once the namespace resolved", n)
	}
}

// ---------------------------------------------------------------------------
// Region resolution over the real SDK path.
// ---------------------------------------------------------------------------

// TestListSubscribedRegions_OverTheWire exercises the real identity call. The
// existing tests only drive the injected listSubscribed seam, so the actual SDK
// request/decoding path — the thing that runs in production — was unproven.
func TestListSubscribedRegions_OverTheWire(t *testing.T) {
	f := newFakeOCI(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, jsonArray(
			`{"regionKey":"IAD","regionName":"us-ashburn-1","status":"READY","isHomeRegion":true}`,
			// A blank name must be skipped, not turned into an empty region
			// that every regional collector would then try to reach.
			`{"regionKey":"XXX","regionName":"","status":"READY","isHomeRegion":false}`,
			`{"regionKey":"LHR","regionName":"uk-london-1","status":"READY","isHomeRegion":false}`,
		))
	})

	p := wiredProvider(t, f, Config{})
	got, err := p.listSubscribedRegions(context.Background())
	if err != nil {
		t.Fatalf("listSubscribedRegions: %v", err)
	}
	want := []string{"us-ashburn-1", "uk-london-1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got[i], want[i])
		}
	}
	if q := f.queriesFor(pathRegionSubs); len(q) != 1 {
		t.Errorf("made %d ListRegionSubscriptions calls, want 1", len(q))
	}
}

// TestListSubscribedRegions_EmptyIsAnError: an empty subscription list is not a
// valid "scan nothing" answer — every tenancy is subscribed to at least its
// home region, so an empty response means something is wrong upstream. Silently
// returning zero regions would produce an empty audit with no diagnostic.
func TestListSubscribedRegions_EmptyIsAnError(t *testing.T) {
	f := newFakeOCI(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	p := wiredProvider(t, f, Config{})
	if _, err := p.listSubscribedRegions(context.Background()); err == nil {
		t.Fatal("empty subscription list returned no error; the audit would scan zero regions and report success")
	}
}

// TestResolveRegions_RealListerFailureFallsBackToHome proves the home-region
// fallback works through the *production* code path (listSubscribedRegions
// hitting a 403), not just through the injected listSubscribed test seam. A
// missing identity permission is common and must degrade to "scan the home
// region" rather than abort the whole OCI audit.
func TestResolveRegions_RealListerFailureFallsBackToHome(t *testing.T) {
	f := newFakeOCI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"code":"NotAuthorizedOrNotFound","message":"denied"}`)
	})

	p := wiredProvider(t, f, Config{})
	if p.listSubscribed != nil {
		t.Fatal("this test must exercise the real SDK path, but the listSubscribed seam is set")
	}
	got, err := p.resolveRegions(context.Background())
	if err != nil {
		t.Fatalf("a subscription lookup failure must not abort the audit: %v", err)
	}
	if len(got) != 1 || got[0] != "us-ashburn-1" {
		t.Errorf("got %v, want [us-ashburn-1] (the tenancy home region)", got)
	}
}

// ---------------------------------------------------------------------------
// Validate.
// ---------------------------------------------------------------------------

func TestValidate_OverTheWire(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string // "" means success expected
	}{
		{
			name: "reachable tenancy validates", status: http.StatusOK,
			body: `{"id":"` + testTenancyOCID + `","name":"acme","description":"Acme"}`,
		},
		{
			// A wrong/typo'd tenancy OCID or a token scoped elsewhere. Must be
			// a clear error, never a silent success that lets Collect run and
			// return nothing.
			name: "404 fails validation", status: http.StatusNotFound,
			body: `{"code":"TenantNotFound","message":"tenancy not found"}`, wantErr: "get tenancy",
		},
		{
			name: "403 fails validation", status: http.StatusForbidden,
			body: `{"code":"NotAuthorizedOrNotFound","message":"denied"}`, wantErr: "get tenancy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOCI(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			p := wiredProvider(t, f, Config{})
			err := p.Validate(context.Background())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate returned nil for HTTP %d; an unusable tenancy must fail fast, not produce an empty audit", tc.status)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate error = %q, want it to contain %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "oci:") {
				t.Errorf("Validate error = %q, want it prefixed with the provider name", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// run(): fan-out shape and partial failure.
// ---------------------------------------------------------------------------

// runFixture serves a small tenancy: the root plus two child compartments, two
// subscribed regions, and an empty page for every resource list.
func runFixture(t *testing.T, override func(w http.ResponseWriter, r *http.Request) bool) *fakeOCI {
	t.Helper()
	return newFakeOCI(t, func(w http.ResponseWriter, r *http.Request) {
		if override != nil && override(w, r) {
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, pathCompartments):
			fmt.Fprint(w, jsonArray(
				`{"id":"ocid1.compartment.oc1..prod","name":"Production","compartmentId":"`+testTenancyOCID+`","lifecycleState":"ACTIVE"}`,
				`{"id":"ocid1.compartment.oc1..web","name":"Web","compartmentId":"ocid1.compartment.oc1..prod","lifecycleState":"ACTIVE"}`,
			))
		case strings.HasSuffix(r.URL.Path, pathRegionSubs):
			fmt.Fprint(w, jsonArray(
				`{"regionKey":"IAD","regionName":"us-ashburn-1","status":"READY","isHomeRegion":true}`,
				`{"regionKey":"PHX","regionName":"us-phoenix-1","status":"READY","isHomeRegion":false}`,
			))
		case r.URL.Path == pathNamespace:
			fmt.Fprint(w, `"testns"`)
		case strings.HasSuffix(r.URL.Path, pathNLBs), strings.HasSuffix(r.URL.Path, pathContainerInsts):
			fmt.Fprint(w, jsonItems())
		default:
			fmt.Fprint(w, `[]`)
		}
	})
}

// TestRun_IdentityCollectorsDoNotMultiplyByRegion is the fan-out contract.
// Identity is a global service: policies are compartment-scoped but
// region-independent, and users/groups/dynamic-groups exist only at the tenancy
// root. Hoisting any of them into the region loop is an easy and invisible
// refactor — the audit still "works", it just emits N duplicate copies of every
// user and burns N× the identity API quota. Only call counts catch it.
func TestRun_IdentityCollectorsDoNotMultiplyByRegion(t *testing.T) {
	f := runFixture(t, nil)
	p := wiredProvider(t, f, Config{MaxConcurrency: 8})

	assets, errs := drain(t, context.Background(), p)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	const (
		wantRegions      = 2 // us-ashburn-1, us-phoenix-1
		wantCompartments = 3 // tenancy root + Production + Web
	)

	// A regional collector runs once per (region × compartment).
	if n := f.hits(pathInstances); n != wantRegions*wantCompartments {
		t.Errorf("ListInstances called %d times, want %d (%d regions × %d compartments)",
			n, wantRegions*wantCompartments, wantRegions, wantCompartments)
	}
	// Policies: once per compartment, NOT per region.
	if n := f.hits(pathPolicies); n != wantCompartments {
		t.Errorf("ListPolicies called %d times, want %d (once per compartment). %d would mean policies were pulled into the region loop, duplicating every policy asset per region",
			n, wantCompartments, wantRegions*wantCompartments)
	}
	// Tenancy-global IAM: exactly once, full stop.
	for _, path := range []string{pathUsers, pathGroups, pathDynamicGroups} {
		if n := f.hits(path); n != 1 {
			t.Errorf("%s called %d times, want exactly 1 — users/groups/dynamic groups live only at the tenancy root, so any repetition duplicates them across regions or compartments",
				path, n)
		}
	}

	// The compartment tree itself is emitted as assets, once each.
	var compartmentAssets int
	for _, a := range assets {
		if a.Type == "oci.compartment" {
			compartmentAssets++
		}
	}
	if compartmentAssets != wantCompartments {
		t.Errorf("emitted %d oci.compartment assets, want %d (one per compartment, not one per region×compartment)", compartmentAssets, wantCompartments)
	}
}

// TestRun_PartialFailureDoesNotCancelSiblings is invariant 5 (init-plan.md §6):
// one compartment's 403 must not abort the rest of the audit. The trap is the
// errgroup — returning the collector error from g.Go instead of routing it to
// errs would cancel the shared gctx and silently truncate every collector that
// had not yet run. That refactor looks like a cleanup and would pass any test
// that only checked "an error was reported".
func TestRun_PartialFailureDoesNotCancelSiblings(t *testing.T) {
	const badCompartment = "ocid1.compartment.oc1..web"

	f := runFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, pathInstances) {
			if r.URL.Query().Get("compartmentId") == badCompartment {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"code":"NotAuthorizedOrNotFound","message":"denied"}`)
				return true
			}
			fmt.Fprint(w, jsonArray(`{"id":"ocid1.instance.oc1..`+r.URL.Query().Get("compartmentId")+`","displayName":"web-1"}`))
			return true
		}
		if strings.HasSuffix(r.URL.Path, pathVCNs) {
			fmt.Fprint(w, jsonArray(`{"id":"ocid1.vcn.oc1..`+r.URL.Query().Get("compartmentId")+`","displayName":"vcn"}`))
			return true
		}
		return false
	})

	p := wiredProvider(t, f, Config{MaxConcurrency: 8})
	assets, errs := drain(t, context.Background(), p)

	const wantRegions, wantCompartments = 2, 3

	// Every (region × compartment) pair was still attempted for both the
	// failing collector and an unrelated one. A cancelled gctx would abort the
	// queued tasks before they ever reached the server.
	if n := f.hits(pathInstances); n != wantRegions*wantCompartments {
		t.Errorf("ListInstances reached the server %d times, want %d — a sibling compartment's 403 cancelled the rest of the fan-out",
			n, wantRegions*wantCompartments)
	}
	if n := f.hits(pathVCNs); n != wantRegions*wantCompartments {
		t.Errorf("ListVcns reached the server %d times, want %d — an unrelated collector was cancelled by the compute 403",
			n, wantRegions*wantCompartments)
	}

	// One error per (region × failing compartment), all naming the collector.
	if len(errs) != wantRegions {
		t.Errorf("got %d errors, want %d (one per region for the forbidden compartment): %v", len(errs), wantRegions, errs)
	}
	for _, err := range errs {
		if !strings.Contains(err.Error(), "oci compute") {
			t.Errorf("error %q does not name the failing collector; operators cannot act on it", err)
		}
	}

	// The healthy compartments' assets still arrived.
	var instances, vcns int
	for _, a := range assets {
		switch a.Type {
		case "oci.compute.instance":
			instances++
			if strings.Contains(a.ID, badCompartment) {
				t.Errorf("asset %q came from the forbidden compartment", a.ID)
			}
		case "oci.vcn":
			vcns++
		}
	}
	// 2 regions × 2 readable compartments (root + Production).
	if instances != 4 {
		t.Errorf("collected %d instances, want 4 (2 regions × 2 readable compartments) — a partial failure must not cost the healthy compartments", instances)
	}
	if vcns != wantRegions*wantCompartments {
		t.Errorf("collected %d vcns, want %d", vcns, wantRegions*wantCompartments)
	}
}

// TestRun_CompartmentFilterMatchingNothingStillCollectsTenancyIAM: a typo'd
// --oci-compartments selector must be loud (a non-fatal error) but must not
// suppress tenancy-global IAM, which is not compartment-scoped and is still
// perfectly collectable. Bailing out entirely would hide users, groups, and
// dynamic groups behind an unrelated flag typo.
func TestRun_CompartmentFilterMatchingNothingStillCollectsTenancyIAM(t *testing.T) {
	f := runFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, pathUsers) {
			fmt.Fprint(w, jsonArray(`{"id":"ocid1.user.oc1..a","name":"alice"}`))
			return true
		}
		return false
	})

	p := wiredProvider(t, f, Config{MaxConcurrency: 4, Compartments: []string{"Nonexistent"}})
	assets, errs := drain(t, context.Background(), p)

	if len(errs) != 1 {
		t.Fatalf("got %d errors, want exactly 1 telling the operator the selector matched nothing: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "matched no accessible compartment") {
		t.Errorf("error = %q, want it to explain that --oci-compartments matched nothing", errs[0])
	}
	if !strings.Contains(errs[0].Error(), "Nonexistent") {
		t.Errorf("error = %q, want it to echo the offending selector so the typo is obvious", errs[0])
	}

	// Tenancy-global IAM is not compartment-scoped, so it must still run.
	for _, path := range []string{pathUsers, pathGroups, pathDynamicGroups} {
		if n := f.hits(path); n != 1 {
			t.Errorf("%s called %d times, want 1 — tenancy-global IAM is not compartment-scoped and must survive an empty compartment filter", path, n)
		}
	}
	var users, compartments int
	for _, a := range assets {
		switch a.Type {
		case "oci.iam.user":
			users++
		case "oci.compartment":
			compartments++
		}
	}
	if users != 1 {
		t.Errorf("collected %d users, want 1", users)
	}
	if compartments != 0 {
		t.Errorf("emitted %d compartment assets, want 0 (none matched the filter)", compartments)
	}
	// No compartment survived the filter, so no compartment-scoped collector
	// should have run at all.
	if n := f.hits(pathInstances); n != 0 {
		t.Errorf("ListInstances called %d times despite zero selected compartments, want 0", n)
	}
	if n := f.hits(pathPolicies); n != 0 {
		t.Errorf("ListPolicies called %d times despite zero selected compartments, want 0", n)
	}
}

// TestRun_CompartmentFilterScansOnlyTheSelectedSubtree is filterCompartments
// wired end-to-end: selecting a parent by name must scan the parent *and* its
// descendants, and nothing else. Under-scoping an audit (quietly skipping child
// compartments) is the dangerous direction — it reports "clean" for resources
// it never looked at.
func TestRun_CompartmentFilterScansOnlyTheSelectedSubtree(t *testing.T) {
	f := runFixture(t, nil)
	p := wiredProvider(t, f, Config{MaxConcurrency: 4, Compartments: []string{"production"}}) // lower-case on purpose

	assets, errs := drain(t, context.Background(), p)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	scanned := map[string]bool{}
	for _, q := range f.queriesFor(pathInstances) {
		scanned[q.Get("compartmentId")] = true
	}
	if !scanned["ocid1.compartment.oc1..prod"] {
		t.Error("selected compartment Production was never scanned")
	}
	if !scanned["ocid1.compartment.oc1..web"] {
		t.Error("Web (a child of the selected Production) was never scanned — the subtree walk regressed and the audit silently under-scopes")
	}
	if scanned[testTenancyOCID] {
		t.Error("the tenancy root was scanned even though only Production was selected — the filter over-scopes")
	}

	var compartments []string
	for _, a := range assets {
		if a.Type == "oci.compartment" {
			compartments = append(compartments, a.ID)
		}
	}
	if len(compartments) != 2 {
		t.Errorf("emitted compartment assets %v, want exactly Production and Web", compartments)
	}
}

// ---------------------------------------------------------------------------
// Streaming / cancellation contract.
// ---------------------------------------------------------------------------

// TestCollect_CancelledContextClosesBothChannels is the Ctrl+C contract
// (init-plan.md §6 invariant 2, plus the core.Provider channel rule). Both
// channels MUST close exactly once even when the context dies mid-flight —
// a leaked open channel hangs the CLI forever, and a double close panics.
func TestCollect_CancelledContextClosesBothChannels(t *testing.T) {
	f := runFixture(t, nil)
	p := wiredProvider(t, f, Config{MaxConcurrency: 2})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead before the first SDK call

	assets, errs := drain(t, ctx, p)
	// The exact counts are timing-dependent; what must hold is that drain
	// returned at all (both channels closed) without a panic.
	t.Logf("cancelled run produced %d assets and %d errors", len(assets), len(errs))
}

// TestCollect_AuthFailureClosesChannelsAndReportsOnce: when credentials cannot
// be resolved there is nothing to stream, but the channels must still close and
// the reason must reach the caller. A silent empty result would look like an
// empty tenancy.
func TestCollect_AuthFailureClosesChannelsAndReportsOnce(t *testing.T) {
	p := New(Config{})
	p.authErr = fmt.Errorf("no credentials")
	p.authOnce.Do(func() {})

	assets, errs := drain(t, context.Background(), p)
	if len(assets) != 0 {
		t.Errorf("got %d assets from an unauthenticated provider, want 0", len(assets))
	}
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want exactly 1: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "oci auth") {
		t.Errorf("error = %q, want it to identify an auth failure", errs[0])
	}
}

// TestCollect_IncludeRawOffEmitsNoRaw guards both payload size and secret
// exposure: --include-raw defaults to false, and the full SDK response (which
// carries far more than the mapped fields) must not ride along by accident.
func TestCollect_IncludeRawOffEmitsNoRaw(t *testing.T) {
	f := runFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, pathInstances) {
			fmt.Fprint(w, jsonArray(`{"id":"ocid1.instance.oc1..a","displayName":"web-1","metadata":{"ssh_authorized_keys":"ssh-rsa SECRET"}}`))
			return true
		}
		return false
	})

	p := wiredProvider(t, f, Config{MaxConcurrency: 4}) // IncludeRaw defaults to false
	assets, _ := drain(t, context.Background(), p)

	var checked int
	for _, a := range assets {
		if a.Raw != nil {
			t.Fatalf("asset %s (%s) carries Raw despite --include-raw=false: %s", a.ID, a.Type, a.Raw)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no assets were produced, so this test proved nothing")
	}
}
