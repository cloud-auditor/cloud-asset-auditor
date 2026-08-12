package cloudflare

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudflare/cloudflare-go/v4/mtls_certificates"
)

const (
	certPacksJSON = `[{"id":"cp-1","type":"advanced","status":"active",
		"certificate_authority":"lets_encrypt","hosts":["example.com"]}]`
	customCertsJSON = `[{"id":"cc-1","hosts":["secure.example.com"],"issuer":"DigiCert","status":"active",
		"bundle_method":"ubiquitous","priority":1,"signature":"SHA256WithRSA","zone_id":"zone-1",
		"expires_on":"2026-01-01T00:00:00Z","uploaded_on":"2025-01-01T00:00:00Z",
		"modified_on":"2025-01-01T00:00:00Z"}]`
	mtlsCertsJSON = `[{"id":"mc-1","name":"client-ca","ca":true,"issuer":"Internal CA",
		"expires_on":"2027-01-01T00:00:00Z","uploaded_on":"2025-01-01T00:00:00Z"}]`
)

// TestCollectCertificates_OneFamilyFailingKeepsTheOthers is the isolation
// contract for the three certificate families.
//
// They live behind three unrelated endpoints with three unrelated token scopes,
// so a 403 on one is the normal case, not an outage. collectCertificates
// therefore runs all three unconditionally and joins their errors at the end.
// Rewrite it to bail on the first error — the obvious "simplification" — and
// two thirds of the certificate inventory disappears without a trace.
func TestCollectCertificates_OneFamilyFailingKeepsTheOthers(t *testing.T) {
	cases := []struct {
		name string
		// endpoint suffix that returns 403 instead of data
		failing string
		// substring the joined error must contain, naming the failed family
		wantErr string
		// asset types that must still have been collected
		wantTypes []string
	}{
		{
			name:      "certificate packs forbidden",
			failing:   "/ssl/certificate_packs",
			wantErr:   "list certificate packs for zone example.com",
			wantTypes: []string{"cloudflare.custom_certificate", "cloudflare.mtls_certificate"},
		},
		{
			name:      "custom certificates forbidden",
			failing:   "/custom_certificates",
			wantErr:   "list custom certificates for zone example.com",
			wantTypes: []string{"cloudflare.certificate_pack", "cloudflare.mtls_certificate"},
		},
		{
			name:      "mtls certificates forbidden",
			failing:   "/mtls_certificates",
			wantErr:   "list mtls certificates for account acct-1",
			wantTypes: []string{"cloudflare.certificate_pack", "cloudflare.custom_certificate"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.fail(tc.failing, 403, 9109, "Unauthorized to access requested resource")
			f.static("/accounts", accountsJSON("acct-1"))
			f.static("/zones", zonesJSON("zone-1", "example.com", "acct-1"))
			f.static("/ssl/certificate_packs", v4List(certPacksJSON))
			f.static("/custom_certificates", v4List(customCertsJSON))
			f.static("/mtls_certificates", v4List(mtlsCertsJSON))
			p := f.provider(t, Config{})

			got, err := collectVia(t, p.collectCertificates)

			if err == nil {
				t.Fatalf("collectCertificates() = nil, want the %s failure reported", tc.failing)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to name the failing family (%q)", err, tc.wantErr)
			}
			for _, typ := range tc.wantTypes {
				if n := len(assetsOfType(got, typ)); n != 1 {
					t.Errorf("got %d %s assets, want 1 — a failure in %s must not hide the other families; assets=%v",
						n, typ, tc.failing, assetIDs(got))
				}
			}
			// All three endpoints must have been attempted, not short-circuited.
			for _, suffix := range []string{"/ssl/certificate_packs", "/custom_certificates", "/mtls_certificates"} {
				if f.hits(suffix) == 0 {
					t.Errorf("%s was never called; every family must run regardless of the others", suffix)
				}
			}
		})
	}
}

// TestCollectCertificates_AllFamiliesFailingReportsAllThree makes sure the join
// accumulates rather than overwrites: three separate scope gaps must produce
// three separate, individually actionable messages.
func TestCollectCertificates_AllFamiliesFailingReportsAllThree(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))
	f.static("/zones", zonesJSON("zone-1", "example.com", "acct-1"))
	f.fail("/ssl/certificate_packs", 403, 9109, "Unauthorized")
	f.fail("/custom_certificates", 403, 9109, "Unauthorized")
	f.fail("/mtls_certificates", 403, 9109, "Unauthorized")
	p := f.provider(t, Config{})

	got, err := collectVia(t, p.collectCertificates)
	if err == nil {
		t.Fatal("collectCertificates() = nil, want three joined failures")
	}
	if len(got) != 0 {
		t.Errorf("got %d assets, want 0: %v", len(got), assetIDs(got))
	}
	for _, want := range []string{"certificate packs", "custom certificates", "mtls certificates"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error %q is missing the %q family", err, want)
		}
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("error should be an errors.Join tree so filterServiceDisabled can walk it; got %T", err)
	}
	if n := len(joined.Unwrap()); n != 3 {
		t.Errorf("joined error has %d causes, want 3 (one per family)", n)
	}
}

// TestCollectCertificatePacks_OneZoneForbiddenKeepsOtherZones is the same
// isolation rule one level down. Certificate packs are listed per zone, and a
// token can easily be scoped to a subset of zones; the first zone's 403 must
// not stop the loop.
func TestCollectCertificatePacks_OneZoneForbiddenKeepsOtherZones(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))
	// Two zones in one listing.
	f.static("/zones", v4List(`[
	  {"id":"zone-blocked","name":"blocked.com","status":"active","paused":false,"type":"full",
	   "account":{"id":"acct-1","name":"Acme"},"created_on":"2024-01-01T00:00:00Z"},
	  {"id":"zone-ok","name":"ok.com","status":"active","paused":false,"type":"full",
	   "account":{"id":"acct-1","name":"Acme"},"created_on":"2024-01-01T00:00:00Z"}]`))
	f.route("/ssl/certificate_packs", func(w http.ResponseWriter, r *http.Request, _ int) {
		if strings.Contains(r.URL.Path, "zone-blocked") {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, cfErrorBody(9109, "Unauthorized to access requested resource"))
			return
		}
		fmt.Fprint(w, v4List(certPacksJSON))
	})
	p := f.provider(t, Config{})

	got, err := collectVia(t, p.collectCertificates)
	if err == nil {
		t.Fatal("the blocked zone's 403 should still be reported")
	}
	if !strings.Contains(err.Error(), "blocked.com") {
		t.Errorf("error %q should name the zone that failed", err)
	}
	if strings.Contains(err.Error(), "ok.com") {
		t.Errorf("error %q blames a zone that succeeded", err)
	}
	packs := assetsOfType(got, "cloudflare.certificate_pack")
	if len(packs) != 1 {
		t.Fatalf("got %d certificate packs, want 1 from the readable zone; assets=%v", len(packs), assetIDs(got))
	}
	if packs[0].Tags["zone_name"] != "ok.com" {
		t.Errorf("surviving pack came from zone %q, want ok.com", packs[0].Tags["zone_name"])
	}
}

// TestCollectCertificates_ServiceDisabledCauseIsStrippedFromTheJoin ties the
// join back to run()'s filter. "Plan level does not allow custom certificates"
// is a plan limitation, not a finding — but a real scope gap joined alongside it
// must still reach the operator.
func TestCollectCertificates_ServiceDisabledCauseIsStrippedFromTheJoin(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))
	f.static("/zones", zonesJSON("zone-1", "example.com", "acct-1"))
	f.fail("/custom_certificates", 400, 1011, "Plan level does not allow custom certificates with type ")
	f.fail("/mtls_certificates", 403, 9109, "Unauthorized to access requested resource")
	f.static("/ssl/certificate_packs", v4List(certPacksJSON))
	p := f.provider(t, Config{})

	_, err := collectVia(t, p.collectCertificates)
	if err == nil {
		t.Fatal("collectCertificates() = nil, want the mTLS scope gap")
	}

	filtered := filterServiceDisabled(err)
	if filtered == nil {
		t.Fatal("filterServiceDisabled dropped the whole join; the mTLS scope gap must survive")
	}
	if strings.Contains(filtered.Error(), "Plan level") {
		t.Errorf("the plan-limitation cause should have been stripped, got %q", filtered)
	}
	if !strings.Contains(filtered.Error(), "mtls certificates") {
		t.Errorf("the real scope gap should remain, got %q", filtered)
	}
	if !errors.Is(withScopeHint(filtered), filtered) {
		t.Error("withScopeHint must wrap rather than replace the filtered error")
	}
}

// TestMTLSCertificateToAsset_IDFallsBackToAccountAndName covers the last
// unkeyed case. Asset identity is (provider, id) — that pair is what auditor
// diff compares snapshots on — so an asset with an empty ID either collides with
// every other empty-ID asset or reports phantom drift on every run. mTLS
// certificates are the one family where both the id and the (optional) name can
// be absent, so the composed fallback has to hold.
func TestMTLSCertificateToAsset_IDFallsBackToAccountAndName(t *testing.T) {
	p := &Provider{}

	withName := p.mtlsCertificateToAsset("acct-1", mtls_certificates.MTLSCertificate{Name: "client-ca"})
	if withName.ID != "acct-1/client-ca" {
		t.Errorf("ID = %q, want acct-1/client-ca (composed when the API omits the id)", withName.ID)
	}

	// Neither id nor name: the composed key still has to be account-scoped
	// rather than a bare empty string shared by every such certificate.
	bare := p.mtlsCertificateToAsset("acct-1", mtls_certificates.MTLSCertificate{})
	if bare.ID != "acct-1/" {
		t.Errorf("ID = %q, want acct-1/ for a certificate with neither id nor name", bare.ID)
	}
	if bare.AccountID != "acct-1" {
		t.Errorf("AccountID = %q, want acct-1", bare.AccountID)
	}
}
