package cloudflare

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// fullInventoryAPI stands up a fake account/zone with exactly one resource of
// every type the provider knows how to collect.
func fullInventoryAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := newFakeAPI(t)
	registerFullInventory(f)
	return f
}

// registerFullInventory adds the happy-path fixtures to f. It is separate from
// fullInventoryAPI so a test can register a failing route FIRST (routes match in
// registration order) and let these fill in everything else.
func registerFullInventory(f *fakeAPI) {
	f.static("/accounts", accountsJSON("acct-1"))
	f.static("/zones", zonesJSON("zone-1", "example.com", "acct-1"))

	// account-scoped
	f.static("/r2/buckets", r2BucketsJSON("bkt-1"))
	f.static("/storage/kv/namespaces", v4List(`[{"id":"kv-1","title":"my-kv","supports_url_encoding":true}]`))
	f.static("/workers/scripts", v4List(
		`[{"id":"worker-1","usage_model":"standard","logpush":false,
		   "created_on":"2025-01-01T00:00:00Z","placement":{"mode":"smart"}}]`))
	f.static("/d1/database", v4List(
		`[{"uuid":"d1-1","name":"prod-db","version":"production","created_at":"2025-01-01T00:00:00Z"}]`))
	f.static("/pages/projects", v4List(
		`[{"id":"pages-1","name":"marketing-site","subdomain":"marketing-site.pages.dev",
		   "production_branch":"main","domains":["www.example.com"],"created_on":"2025-01-01T00:00:00Z"}]`))
	f.static("/access/apps", v4List(
		`[{"id":"app-1","name":"internal-tool","domain":"tool.example.com","type":"self_hosted",
		   "aud":"aud-hash","session_duration":"24h","created_at":"2025-01-01T00:00:00Z"}]`))
	f.static("/cfd_tunnel", v4List(
		`[{"id":"tun-1","name":"prod-tunnel","status":"healthy","tun_type":"cfd_tunnel",
		   "remote_config":true,"created_at":"2025-01-01T00:00:00Z"}]`))
	f.static("/mtls_certificates", v4List(mtlsCertsJSON))

	// zone-scoped
	f.static("/dns_records", v4List(
		`[{"id":"rec-1","name":"www.example.com","type":"A","content":"192.0.2.1","proxied":true}]`))
	f.static("/pagerules", v4List(
		`[{"id":"pr-1","status":"active","priority":1,"actions":[],
		   "targets":[{"target":"url","constraint":{"operator":"matches","value":"example.com/*"}}],
		   "created_on":"2025-01-01T00:00:00Z","modified_on":"2025-01-01T00:00:00Z"}]`))
	f.static("/load_balancers", v4List(
		`[{"id":"lb-1","name":"lb.example.com","enabled":true,"proxied":true,
		   "steering_policy":"dynamic_latency","session_affinity":"none",
		   "fallback_pool":"pool-1","default_pools":["pool-1"],"created_on":"2025-01-01T00:00:00Z"}]`))
	f.static("/ssl/certificate_packs", v4List(certPacksJSON))
	f.static("/custom_certificates", v4List(customCertsJSON))
	f.static("/rulesets", v4List(
		`[{"id":"rs-1","name":"managed","kind":"managed","phase":"http_request_firewall_managed",
		   "version":"1","last_updated":"2025-02-03T04:05:06Z"}]`))
}

// TestRun_WiresEveryCollectorIntoTheFanOut checks that run() actually dispatches
// all fourteen collectors.
//
// The failure it guards is quiet by construction: a new collector whose
// `collect("name", ...)` line is missing from run() still compiles, still has
// its own passing unit tests, and simply never contributes a single asset to a
// real audit. The same is true in reverse for a collector accidentally deleted
// from the list. Only an end-to-end assertion on the emitted Types catches it.
func TestRun_WiresEveryCollectorIntoTheFanOut(t *testing.T) {
	f := fullInventoryAPI(t)
	p := f.provider(t, Config{})

	assets, errs := runCollect(t, context.Background(), p)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errorStrings(errs))
	}

	want := []string{
		"cloudflare.access_app",
		"cloudflare.account",
		"cloudflare.certificate_pack",
		"cloudflare.custom_certificate",
		"cloudflare.d1_database",
		"cloudflare.dns_record",
		"cloudflare.kv_namespace",
		"cloudflare.load_balancer",
		"cloudflare.mtls_certificate",
		"cloudflare.page_rule",
		"cloudflare.pages_project",
		"cloudflare.r2_bucket",
		"cloudflare.ruleset",
		"cloudflare.tunnel",
		"cloudflare.worker_script",
		"cloudflare.zone",
	}
	seen := map[string]int{}
	for _, a := range assets {
		seen[a.Type]++
	}
	for _, typ := range want {
		if seen[typ] == 0 {
			t.Errorf("no %s asset was emitted — is its collector wired into run()?", typ)
		}
	}
	got := make([]string, 0, len(seen))
	for typ := range seen {
		got = append(got, typ)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Errorf("emitted types = %v,\n            want %v", got, want)
	}

	// Rulesets are collected at both scopes from one endpoint, so that type
	// appears twice; everything else is one-per-fixture.
	if n := seen["cloudflare.ruleset"]; n != 2 {
		t.Errorf("got %d ruleset assets, want 2 (account scope + zone scope)", n)
	}

	// Every asset must be attributable — the XLSX --sheet-by grouping and the
	// topology's account clustering both key off AccountID.
	for _, a := range assets {
		if a.Provider != providerName {
			t.Errorf("%s %s: Provider = %q, want %q", a.Type, a.ID, a.Provider, providerName)
		}
		if a.AccountID != "acct-1" {
			t.Errorf("%s %s: AccountID = %q, want acct-1", a.Type, a.ID, a.AccountID)
		}
		if a.ID == "" {
			t.Errorf("%s: empty ID — assets are keyed by (provider, id) in auditor diff", a.Type)
		}
	}
}

// TestRun_EveryCollectorFailureIsNamedAndNonFatal walks each collector's
// endpoint in turn, makes only that one return 403, and checks two things at
// once: the error is attributed to the right collector (an operator has to know
// WHICH scope to grant), and every other collector still delivered its asset.
//
// This is invariant 5 applied fourteen ways. An errgroup misuse — returning the
// error from g.Go instead of pushing it to errs — would cancel gctx and turn any
// single 403 into a near-empty audit, and nothing else in the suite would notice.
func TestRun_EveryCollectorFailureIsNamedAndNonFatal(t *testing.T) {
	cases := []struct {
		endpoint string // suffix that returns 403
		wantErr  string // collector name run() prefixes the error with
		wantType string // asset type that consequently goes missing
	}{
		{endpoint: "/r2/buckets", wantErr: "cloudflare r2:", wantType: "cloudflare.r2_bucket"},
		{endpoint: "/storage/kv/namespaces", wantErr: "cloudflare kv:", wantType: "cloudflare.kv_namespace"},
		{endpoint: "/workers/scripts", wantErr: "cloudflare workers:", wantType: "cloudflare.worker_script"},
		{endpoint: "/d1/database", wantErr: "cloudflare d1:", wantType: "cloudflare.d1_database"},
		{endpoint: "/pages/projects", wantErr: "cloudflare pages:", wantType: "cloudflare.pages_project"},
		{endpoint: "/access/apps", wantErr: "cloudflare access:", wantType: "cloudflare.access_app"},
		{endpoint: "/cfd_tunnel", wantErr: "cloudflare tunnels:", wantType: "cloudflare.tunnel"},
		{endpoint: "/dns_records", wantErr: "cloudflare dns/example.com:", wantType: "cloudflare.dns_record"},
		{endpoint: "/pagerules", wantErr: "cloudflare page-rules/example.com:", wantType: "cloudflare.page_rule"},
		{endpoint: "/load_balancers", wantErr: "cloudflare load-balancers/example.com:", wantType: "cloudflare.load_balancer"},
	}

	for _, tc := range cases {
		t.Run(strings.TrimPrefix(tc.endpoint, "/"), func(t *testing.T) {
			f := newFakeAPI(t)
			// Routes match in registration order, so the failing one goes first
			// and the happy-path fixtures fill in the rest.
			f.fail(tc.endpoint, 403, 9109, "Unauthorized to access requested resource")
			registerFullInventory(f)

			p := f.provider(t, Config{})
			assets, errs := runCollect(t, context.Background(), p)

			e := errorContaining(errs, tc.wantErr)
			if e == nil {
				t.Fatalf("no error attributed to %q; an operator can't tell which scope to grant.\n got: %v",
					tc.wantErr, errorStrings(errs))
			}
			if !strings.Contains(e.Error(), "missing the matching Read scope") {
				t.Errorf("error %q should carry the scope hint", e)
			}
			if n := len(assetsOfType(assets, tc.wantType)); n != 0 {
				t.Errorf("got %d %s assets from a 403 endpoint, want 0", n, tc.wantType)
			}
			// Everything else must have survived: one 403 is not an outage.
			for _, other := range cases {
				if other.wantType == tc.wantType {
					continue
				}
				if n := len(assetsOfType(assets, other.wantType)); n == 0 {
					t.Errorf("a 403 on %s also wiped out %s — the errgroup must not cancel siblings "+
						"(init-plan.md invariant 5)", tc.endpoint, other.wantType)
				}
			}
		})
	}
}

// TestRun_DecodesQuirkyFieldsFromTheWire covers the four mappings that are not
// a straight struct-field copy. Each has a comment in the production code
// explaining a workaround; each is only actually exercised against real wire
// JSON, which is why the pure-mapper tests can't cover them.
func TestRun_DecodesQuirkyFieldsFromTheWire(t *testing.T) {
	f := fullInventoryAPI(t)
	p := f.provider(t, Config{})

	assets, errs := runCollect(t, context.Background(), p)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errorStrings(errs))
	}
	first := func(typ string) core.Asset {
		t.Helper()
		got := assetsOfType(assets, typ)
		if len(got) == 0 {
			t.Fatalf("no %s asset emitted", typ)
		}
		return got[0]
	}

	// Pages: the v4 SDK types GET /pages/projects as []pages.Deployment, a
	// generator quirk — the wire JSON is project objects. pagesProjectFromListItem
	// re-decodes from the item's raw JSON, and Name/Subdomain/ProductionBranch
	// exist ONLY on that path. A "cleanup" that trusts the SDK's type blanks them.
	pagesProject := first("cloudflare.pages_project")
	if pagesProject.Name != "marketing-site" {
		t.Errorf("pages project Name = %q, want marketing-site — the raw-JSON re-decode "+
			"in pagesProjectFromListItem is what recovers project-only fields", pagesProject.Name)
	}
	if pagesProject.Tags["subdomain"] != "marketing-site.pages.dev" {
		t.Errorf("pages project Tags[subdomain] = %q, want marketing-site.pages.dev", pagesProject.Tags["subdomain"])
	}
	if pagesProject.Tags["production_branch"] != "main" {
		t.Errorf("pages project Tags[production_branch] = %q, want main", pagesProject.Tags["production_branch"])
	}

	// Load balancer: created_on is a plain string in the generated SDK, so
	// timePtr can't be used and loadBalancerCreatedOn parses it by hand. Name is
	// the LB's DNS hostname and must stay verbatim — the topology dnsToTarget
	// resolver joins CNAME targets to it by hostname.
	lb := first("cloudflare.load_balancer")
	if lb.Name != "lb.example.com" {
		t.Errorf("load balancer Name = %q, want the DNS hostname lb.example.com "+
			"(topology resolvers join on it)", lb.Name)
	}
	if lb.CreatedAt == nil {
		t.Error("load balancer CreatedAt is nil; the string created_on was not parsed")
	}
	if lb.Status != "enabled" {
		t.Errorf("load balancer Status = %q, want enabled", lb.Status)
	}

	// Worker scripts: the script name IS the id, and the nested placement.mode
	// must win over the deprecated top-level placement_mode.
	ws := first("cloudflare.worker_script")
	if ws.ID != "worker-1" || ws.Name != ws.ID {
		t.Errorf("worker ID/Name = %q/%q, want both worker-1", ws.ID, ws.Name)
	}
	if ws.Tags["placement_mode"] != "smart" {
		t.Errorf("worker Tags[placement_mode] = %q, want smart (from the nested placement object)",
			ws.Tags["placement_mode"])
	}

	// Access apps: the AUD is the token audience an operator needs to configure
	// a service token, and the domain is what topology can join on.
	app := first("cloudflare.access_app")
	if app.Tags["aud"] != "aud-hash" || app.Tags["domain"] != "tool.example.com" {
		t.Errorf("access app tags = %v, want aud=aud-hash and domain=tool.example.com", app.Tags)
	}
}

// TestCollectTunnels_ExcludesDeletedTunnels pins both halves of the deleted
// filter: is_deleted=false must go on the wire, and the defensive DeletedAt
// check must still drop a deleted tunnel if the API ignores the filter. A
// deleted tunnel in the inventory is a false positive an operator would chase.
func TestCollectTunnels_ExcludesDeletedTunnels(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))
	f.static("/cfd_tunnel", v4List(`[
	  {"id":"tun-live","name":"prod","status":"healthy","created_at":"2025-01-01T00:00:00Z"},
	  {"id":"tun-gone","name":"old","status":"inactive","created_at":"2024-01-01T00:00:00Z",
	   "deleted_at":"2025-06-01T00:00:00Z"}]`))
	p := f.provider(t, Config{})

	got, err := collectVia(t, p.collectTunnels)
	if err != nil {
		t.Fatalf("collectTunnels() = %v, want nil", err)
	}
	if len(got) != 1 || got[0].ID != "tun-live" {
		t.Fatalf("got %v, want only tun-live — a tunnel with deleted_at must be dropped even when "+
			"the API ignores is_deleted=false", assetIDs(got))
	}
	qs := f.queries("/cfd_tunnel")
	if len(qs) == 0 {
		t.Fatal("no request reached /cfd_tunnel")
	}
	if v := qs[0].Get("is_deleted"); v != "false" {
		t.Errorf("is_deleted on the wire = %q, want false (filter deleted tunnels server-side)", v)
	}
}
