package cloudflare

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// TestRun_ZeroAccountsEmitsTheNoAccountsHint covers the single most confusing
// failure this provider has.
//
// A token scoped to Zone + Zone.DNS only does NOT get a 403 from GET /accounts.
// It gets HTTP 200 with an empty result list. Every account-scoped collector
// (R2, KV, Workers, D1, Pages, Access, Tunnels, mTLS certs, account rulesets)
// then iterates zero accounts, emits nothing, and returns nil — so the operator
// sees DNS records and nothing else, with no error anywhere to explain why.
// noAccountsHint is the entire explanation, so it must reach the error channel.
func TestRun_ZeroAccountsEmitsTheNoAccountsHint(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", v4List(`[]`)) // HTTP 200, success:true, zero accounts
	f.static("/zones", zonesJSON("zone-1", "example.com", "acct-1"))
	p := f.provider(t, Config{})

	assets, errs := runCollect(t, context.Background(), p)

	if errorContaining(errs, "0 accounts") == nil {
		t.Fatalf("no noAccountsHint on the error channel; a Zone-only token would look like a silent success.\n got errors: %v", errorStrings(errs))
	}
	if len(errs) != 1 {
		t.Errorf("got %d errors, want exactly 1 (only the hint): %v", len(errs), errorStrings(errs))
	}
	// Zone-scoped collection is unaffected — that asymmetry is the symptom the
	// hint describes, so pin both halves.
	if zs := assetsOfType(assets, "cloudflare.zone"); len(zs) != 1 {
		t.Errorf("got %d zone assets, want 1 — zone-scoped collection must be unaffected", len(zs))
	}
	// And the account-scoped endpoints genuinely were never reached.
	for _, suffix := range []string{"/r2/buckets", "/storage/kv/namespaces", "/mtls_certificates", "/access/apps"} {
		if n := f.hits(suffix); n != 0 {
			t.Errorf("%s was called %d times with zero accounts, want 0", suffix, n)
		}
	}
}

// TestRun_ZoneListFailureStillCollectsAccountScoped is invariant 5 (partial
// failure is normal) at the run() level: a 403 listing zones must not abort the
// audit, because none of the ten account-scoped collectors depend on the zone
// list. It also pins withScopeHint firing on a *real* SDK error rather than a
// synthetic string.
func TestRun_ZoneListFailureStillCollectsAccountScoped(t *testing.T) {
	f := newFakeAPI(t)
	f.fail("/zones", 403, 9109, "Unauthorized to access requested resource")
	f.static("/accounts", accountsJSON("acct-1"))
	f.static("/r2/buckets", r2BucketsJSON("survivor"))
	p := f.provider(t, Config{})

	assets, errs := runCollect(t, context.Background(), p)

	if got := assetsOfType(assets, "cloudflare.r2_bucket"); len(got) != 1 {
		t.Errorf("got %d r2 assets after a zone-list 403, want 1 — account-scoped collectors "+
			"must not be cancelled by an unrelated failure (invariant 5). assets=%v",
			len(got), assetIDs(assets))
	}
	if got := assetsOfType(assets, "cloudflare.account"); len(got) != 1 {
		t.Errorf("got %d account assets, want 1", len(got))
	}
	if errorContaining(errs, "cloudflare zones:") == nil {
		t.Errorf("the zone-list failure must still be reported; got %v", errorStrings(errs))
	}
	// The certificates collector re-lists zones and its error goes through the
	// collect() wrapper, so this is withScopeHint applied to a genuine v4 SDK
	// 403 — the contract that breaks silently if the SDK reformats its errors.
	if errorContaining(errs, "missing the matching Read scope") == nil {
		t.Errorf("a real SDK 403 should have picked up the scope hint; got %v", errorStrings(errs))
	}
}

// TestRun_DisabledServiceIsDroppedButRealFailuresSurface pins the asymmetry in
// filterServiceDisabled. R2 answers 403 code 10042 ("enable R2 in the
// dashboard") for accounts that simply never adopted R2 — noise, not a finding.
// A 403 code 9109 on another collector is a genuine scope gap and must survive.
// Collapsing the two (or "simplifying" the join) silences real failures.
func TestRun_DisabledServiceIsDroppedButRealFailuresSurface(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))
	f.static("/zones", v4List(`[]`))
	f.fail("/r2/buckets", 403, 10042, "Please enable R2 through the Cloudflare Dashboard.")
	f.fail("/storage/kv/namespaces", 403, 9109, "Unauthorized to access requested resource")
	p := f.provider(t, Config{})

	_, errs := runCollect(t, context.Background(), p)

	if e := errorContaining(errs, "cloudflare r2"); e != nil {
		t.Errorf("R2's disabled-service 403 (code 10042) should be dropped silently, got %q", e)
	}
	kv := errorContaining(errs, "cloudflare kv")
	if kv == nil {
		t.Fatalf("KV's genuine scope gap (code 9109) was swallowed; got %v", errorStrings(errs))
	}
	if !strings.Contains(kv.Error(), "missing the matching Read scope") {
		t.Errorf("KV scope-gap error should carry the scope hint, got %q", kv)
	}
}

// TestRun_ZoneScopedAssetsCarryZoneTags protects a cross-package contract: the
// topology wafBinding resolver joins zone-scoped assets to their zone purely on
// Tags["zone_id"]. Drop that tag from any of these mappers and the graph
// silently loses every WAF edge — no test outside this package would notice,
// because the assets still exist and still look plausible.
func TestRun_ZoneScopedAssetsCarryZoneTags(t *testing.T) {
	const (
		zoneID   = "zone-1"
		zoneName = "example.com"
	)
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))
	f.static("/zones", zonesJSON(zoneID, zoneName, "acct-1"))
	f.static("/dns_records", v4List(
		`[{"id":"rec-1","name":"www.example.com","type":"A","content":"192.0.2.1","proxied":true}]`))
	f.static("/pagerules", v4List(
		`[{"id":"pr-1","status":"active","priority":1,"actions":[],
		   "targets":[{"target":"url","constraint":{"operator":"matches","value":"example.com/*"}}],
		   "created_on":"2024-01-01T00:00:00Z","modified_on":"2024-01-01T00:00:00Z"}]`))
	f.static("/ssl/certificate_packs", v4List(
		`[{"id":"cp-1","type":"advanced","status":"active","certificate_authority":"lets_encrypt",
		   "hosts":["example.com","*.example.com"]}]`))
	f.static("/custom_certificates", v4List(
		`[{"id":"cc-1","hosts":["secure.example.com"],"issuer":"DigiCert","status":"active",
		   "bundle_method":"ubiquitous","priority":1,"signature":"SHA256WithRSA","zone_id":"zone-1",
		   "expires_on":"2026-01-01T00:00:00Z","uploaded_on":"2025-01-01T00:00:00Z",
		   "modified_on":"2025-01-01T00:00:00Z"}]`))
	// One suffix serves both ruleset scopes; branch on the path to prove the
	// two are actually distinguished.
	f.route("/rulesets", func(w http.ResponseWriter, r *http.Request, _ int) {
		id, scope := "rs-zone", zoneID
		if strings.Contains(r.URL.Path, "/accounts/") {
			id, scope = "rs-account", "acct-1"
		}
		fmt.Fprint(w, v4List(fmt.Sprintf(
			`[{"id":%q,"name":"ruleset for %s","kind":"zone","phase":"http_request_firewall_managed",
			   "version":"1","last_updated":"2025-02-03T04:05:06Z"}]`, id, scope)))
	})
	p := f.provider(t, Config{})

	assets, errs := runCollect(t, context.Background(), p)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errorStrings(errs))
	}

	// Every zone-scoped type must carry the join key.
	for _, typ := range []string{
		"cloudflare.dns_record",
		"cloudflare.page_rule",
		"cloudflare.certificate_pack",
		"cloudflare.custom_certificate",
	} {
		got := assetsOfType(assets, typ)
		if len(got) != 1 {
			t.Errorf("%s: got %d assets, want 1 (assets=%v)", typ, len(got), assetIDs(assets))
			continue
		}
		if got[0].Tags["zone_id"] != zoneID {
			t.Errorf("%s: Tags[zone_id] = %q, want %q — the topology wafBinding resolver joins on this",
				typ, got[0].Tags["zone_id"], zoneID)
		}
		if got[0].Tags["zone_name"] != zoneName {
			t.Errorf("%s: Tags[zone_name] = %q, want %q", typ, got[0].Tags["zone_name"], zoneName)
		}
	}

	// Rulesets share one Type across both scopes; "scope" is the discriminator
	// and only the zone-scoped one may carry zone_id.
	rulesets := assetsOfType(assets, "cloudflare.ruleset")
	if len(rulesets) != 2 {
		t.Fatalf("got %d ruleset assets, want 2 (one per scope): %v", len(rulesets), assetIDs(rulesets))
	}
	byID := map[string]core.Asset{}
	for _, a := range rulesets {
		byID[a.ID] = a
	}
	zoneRS, ok := byID["rs-zone"]
	if !ok {
		t.Fatalf("zone-scoped ruleset missing: %v", assetIDs(rulesets))
	}
	if zoneRS.Tags["scope"] != "zone" || zoneRS.Tags["zone_id"] != zoneID {
		t.Errorf("zone ruleset tags = %v, want scope=zone and zone_id=%s", zoneRS.Tags, zoneID)
	}
	acctRS, ok := byID["rs-account"]
	if !ok {
		t.Fatalf("account-scoped ruleset missing: %v", assetIDs(rulesets))
	}
	if acctRS.Tags["scope"] != "account" {
		t.Errorf("account ruleset Tags[scope] = %q, want account", acctRS.Tags["scope"])
	}
	if v, present := acctRS.Tags["zone_id"]; present {
		t.Errorf("account-scoped ruleset carries zone_id=%q; it belongs to no zone and would "+
			"produce a bogus wafBinding edge", v)
	}
}

// TestRun_IncludeRawGate checks the --include-raw default end to end rather
// than per-mapper. Raw is unbounded provider JSON: leaking it by default bloats
// every snapshot and widens the blast radius of anything sensitive the API
// returns.
func TestRun_IncludeRawGate(t *testing.T) {
	for _, includeRaw := range []bool{false, true} {
		t.Run(fmt.Sprintf("include_raw=%t", includeRaw), func(t *testing.T) {
			f := newFakeAPI(t)
			f.static("/accounts", accountsJSON("acct-1"))
			f.static("/zones", zonesJSON("zone-1", "example.com", "acct-1"))
			f.static("/r2/buckets", r2BucketsJSON("b1"))
			f.static("/dns_records", v4List(`[{"id":"rec-1","name":"www.example.com","type":"A","content":"192.0.2.1"}]`))
			p := f.provider(t, Config{IncludeRaw: includeRaw})

			assets, errs := runCollect(t, context.Background(), p)
			if len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errorStrings(errs))
			}
			if len(assets) == 0 {
				t.Fatal("no assets collected")
			}
			for _, a := range assets {
				switch {
				case includeRaw && a.Raw == nil:
					t.Errorf("%s %s: Raw is nil with --include-raw=true", a.Type, a.ID)
				case !includeRaw && a.Raw != nil:
					t.Errorf("%s %s: Raw = %s with --include-raw=false, want nil", a.Type, a.ID, a.Raw)
				}
			}
		})
	}
}

// TestCollect_CancelledContextClosesBothChannels is invariant 2 / the
// core.Provider contract: Ctrl+C must stop work quickly and BOTH channels must
// close exactly once. A collector that returns early without the fan-out
// unwinding leaves the CLI's range loop hanging forever.
func TestCollect_CancelledContextClosesBothChannels(t *testing.T) {
	t.Run("cancelled before collect", func(t *testing.T) {
		f := newFakeAPI(t)
		f.static("/accounts", accountsJSON("acct-1"))
		f.static("/zones", zonesJSON("zone-1", "example.com", "acct-1"))
		p := f.provider(t, Config{})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		assets, _ := runCollect(t, ctx, p) // runCollect fails the test if a channel never closes
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("Collect() took %s after a pre-cancelled context, want well under 1s", d)
		}
		if len(assets) != 0 {
			t.Errorf("got %d assets from a cancelled context, want 0: %v", len(assets), assetIDs(assets))
		}
	})

	t.Run("cancelled mid-stream", func(t *testing.T) {
		f := newFakeAPI(t)
		f.static("/accounts", accountsJSON("acct-1"))
		f.static("/zones", zonesJSON("zone-1", "example.com", "acct-1"))
		f.static("/r2/buckets", r2BucketsJSON(bucketNames("bkt", 200)...))
		p := f.provider(t, Config{})

		ctx, cancel := context.WithCancel(context.Background())
		assetCh, errCh := p.Collect(ctx)

		// Take one asset, then pull the plug and keep draining.
		if _, ok := <-assetCh; !ok {
			t.Fatal("asset channel closed before emitting anything")
		}
		cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			for range assetCh { //nolint:revive // draining
			}
			for range errCh { //nolint:revive // draining
			}
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("channels did not close after cancel — Collect() leaks a goroutine and hangs the CLI")
		}
	})
}

// TestValidate_UsesTokenVerifyEndpoint pins the cheap credential check: it must
// hit /user/tokens/verify and nothing else (enumerating resources to prove a
// token works would be slow and could itself 403 on a correctly-scoped token).
func TestValidate_UsesTokenVerifyEndpoint(t *testing.T) {
	t.Run("valid token", func(t *testing.T) {
		f := newFakeAPI(t)
		f.static("/user/tokens/verify", v4List(`{"id":"tok-1","status":"active"}`))
		p := f.provider(t, Config{})

		if err := p.Validate(context.Background()); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
		if got := f.paths(); len(got) != 1 || !strings.HasSuffix(got[0], "/user/tokens/verify") {
			t.Errorf("Validate() made requests %v, want exactly one to /user/tokens/verify", got)
		}
	})

	t.Run("rejected token", func(t *testing.T) {
		f := newFakeAPI(t)
		f.fail("/user/tokens/verify", 401, 1000, "Invalid API Token")
		p := f.provider(t, Config{})

		err := p.Validate(context.Background())
		if err == nil {
			t.Fatal("Validate() = nil, want an error for a rejected token")
		}
		if !strings.Contains(err.Error(), "verify api token") {
			t.Errorf("error %q should be wrapped with %q for provenance", err, "verify api token")
		}
	})
}
