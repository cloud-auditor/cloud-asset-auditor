package cloudflare

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v4/custom_certificates"
	"github.com/cloudflare/cloudflare-go/v4/mtls_certificates"
	"github.com/cloudflare/cloudflare-go/v4/zones"
)

// certificatesTestZone is the synthetic zone shared by the per-zone
// certificate mapping tests.
func certificatesTestZone() zones.Zone {
	return zones.Zone{
		ID:      "cert-zone-id-1",
		Name:    "certs.example.com",
		Account: zones.ZoneAccount{ID: "cert-acct-id-1", Name: "Cert Test Account"},
	}
}

func TestCertificatePackToAsset_Mapping(t *testing.T) {
	p := &Provider{}
	z := certificatesTestZone()
	// The v4 SDK list response for certificate packs is interface{}; gjson
	// decodes each item into map[string]interface{}.
	pack := map[string]interface{}{
		"id":                    "pack-1",
		"type":                  "advanced",
		"status":                "active",
		"certificate_authority": "lets_encrypt",
		"hosts":                 []interface{}{"certs.example.com", "www.certs.example.com"},
	}

	a := p.certificatePackToAsset(z, pack)

	if a.Provider != "cloudflare" {
		t.Errorf("Provider = %q", a.Provider)
	}
	if a.Type != "cloudflare.certificate_pack" {
		t.Errorf("Type = %q, want cloudflare.certificate_pack", a.Type)
	}
	if a.ID != "pack-1" {
		t.Errorf("ID = %q, want pack-1", a.ID)
	}
	if a.Name != "certs.example.com,www.certs.example.com" {
		t.Errorf("Name = %q, want joined hosts", a.Name)
	}
	if a.AccountID != "cert-acct-id-1" {
		t.Errorf("AccountID = %q, want cert-acct-id-1 (from zone)", a.AccountID)
	}
	if a.Status != "active" {
		t.Errorf("Status = %q, want active", a.Status)
	}
	wantTags := map[string]string{
		"zone_id":               "cert-zone-id-1",
		"zone_name":             "certs.example.com",
		"type":                  "advanced",
		"status":                "active",
		"certificate_authority": "lets_encrypt",
		"hosts":                 "certs.example.com,www.certs.example.com",
	}
	for k, want := range wantTags {
		if got := a.Tags[k]; got != want {
			t.Errorf("Tags[%s] = %q, want %q", k, got, want)
		}
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestCertificatePackToAsset_NonMapPayloadDoesNotPanic(t *testing.T) {
	p := &Provider{}
	z := certificatesTestZone()

	a := p.certificatePackToAsset(z, "not-a-map")

	if a.Type != "cloudflare.certificate_pack" {
		t.Errorf("Type = %q", a.Type)
	}
	if a.AccountID != "cert-acct-id-1" {
		t.Errorf("AccountID = %q", a.AccountID)
	}
	if a.Tags["zone_id"] != "cert-zone-id-1" {
		t.Errorf("Tags[zone_id] = %q", a.Tags["zone_id"])
	}
}

func TestCertificatePackToAsset_IncludeRaw(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	z := certificatesTestZone()
	pack := map[string]interface{}{"id": "pack-raw", "status": "active"}

	a := p.certificatePackToAsset(z, pack)
	if a.Raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(a.Raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if back["id"] != "pack-raw" {
		t.Errorf("Raw.id = %v, want pack-raw", back["id"])
	}
}

func TestCustomCertificateToAsset_Mapping(t *testing.T) {
	p := &Provider{}
	z := certificatesTestZone()
	expires := time.Date(2027, 3, 14, 9, 26, 53, 0, time.UTC)
	uploaded := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	c := custom_certificates.CustomCertificate{
		ID:         "cust-cert-1",
		Hosts:      []string{"certs.example.com", "api.certs.example.com"},
		Issuer:     "DigiCertInc",
		Status:     custom_certificates.CustomCertificateStatusActive,
		ExpiresOn:  expires,
		UploadedOn: uploaded,
		ZoneID:     "cert-zone-id-1",
	}

	a := p.customCertificateToAsset(z, c)

	if a.Type != "cloudflare.custom_certificate" {
		t.Errorf("Type = %q, want cloudflare.custom_certificate", a.Type)
	}
	if a.ID != "cust-cert-1" {
		t.Errorf("ID = %q", a.ID)
	}
	if a.Name != "certs.example.com,api.certs.example.com" {
		t.Errorf("Name = %q, want joined hosts", a.Name)
	}
	if a.AccountID != "cert-acct-id-1" {
		t.Errorf("AccountID = %q, want cert-acct-id-1", a.AccountID)
	}
	if a.Status != "active" {
		t.Errorf("Status = %q, want active", a.Status)
	}
	if a.CreatedAt == nil || !a.CreatedAt.Equal(uploaded) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt, uploaded)
	}
	wantTags := map[string]string{
		"zone_id":    "cert-zone-id-1",
		"zone_name":  "certs.example.com",
		"issuer":     "DigiCertInc",
		"status":     "active",
		"expires_on": "2027-03-14T09:26:53Z",
	}
	for k, want := range wantTags {
		if got := a.Tags[k]; got != want {
			t.Errorf("Tags[%s] = %q, want %q", k, got, want)
		}
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestCustomCertificateToAsset_ZeroTimesOmitted(t *testing.T) {
	p := &Provider{}
	z := certificatesTestZone()
	c := custom_certificates.CustomCertificate{ID: "cust-cert-2"}

	a := p.customCertificateToAsset(z, c)

	if a.CreatedAt != nil {
		t.Errorf("CreatedAt should be nil for zero uploaded_on, got %v", a.CreatedAt)
	}
	if _, ok := a.Tags["expires_on"]; ok {
		t.Error("Tags[expires_on] should be absent for zero expires_on")
	}
	if a.Name != "cust-cert-2" {
		t.Errorf("Name = %q, want fallback to ID when hosts are empty", a.Name)
	}
}

func TestCustomCertificateToAsset_IncludeRaw(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	z := certificatesTestZone()
	c := custom_certificates.CustomCertificate{ID: "cust-cert-raw", Issuer: "GTS"}

	a := p.customCertificateToAsset(z, c)
	if a.Raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(a.Raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
}

func TestMTLSCertificateToAsset_Mapping(t *testing.T) {
	p := &Provider{}
	expires := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	uploaded := time.Date(2026, 2, 2, 12, 30, 0, 0, time.UTC)
	c := mtls_certificates.MTLSCertificate{
		ID:           "mtls-cert-1",
		Name:         "origin-client-ca",
		CA:           true,
		Issuer:       "InternalRootCA",
		ExpiresOn:    expires,
		UploadedOn:   uploaded,
		SerialNumber: "0123456789",
	}

	a := p.mtlsCertificateToAsset("cert-acct-id-1", c)

	if a.Type != "cloudflare.mtls_certificate" {
		t.Errorf("Type = %q, want cloudflare.mtls_certificate", a.Type)
	}
	if a.ID != "mtls-cert-1" || a.Name != "origin-client-ca" {
		t.Errorf("id/name = %q / %q", a.ID, a.Name)
	}
	if a.AccountID != "cert-acct-id-1" {
		t.Errorf("AccountID = %q, want cert-acct-id-1", a.AccountID)
	}
	if a.Status != "" {
		t.Errorf("Status = %q, want empty (mTLS certs have no status)", a.Status)
	}
	if a.CreatedAt == nil || !a.CreatedAt.Equal(uploaded) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt, uploaded)
	}
	wantTags := map[string]string{
		"issuer":     "InternalRootCA",
		"ca":         "true",
		"expires_on": "2030-06-01T00:00:00Z",
	}
	for k, want := range wantTags {
		if got := a.Tags[k]; got != want {
			t.Errorf("Tags[%s] = %q, want %q", k, got, want)
		}
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestMTLSCertificateToAsset_NameFallsBackToID(t *testing.T) {
	p := &Provider{}
	c := mtls_certificates.MTLSCertificate{ID: "mtls-cert-2", CA: false}

	a := p.mtlsCertificateToAsset("cert-acct-id-1", c)

	if a.Name != "mtls-cert-2" {
		t.Errorf("Name = %q, want fallback to ID", a.Name)
	}
	if a.Tags["ca"] != "false" {
		t.Errorf("Tags[ca] = %q, want false", a.Tags["ca"])
	}
	if _, ok := a.Tags["expires_on"]; ok {
		t.Error("Tags[expires_on] should be absent for zero expires_on")
	}
}

func TestMTLSCertificateToAsset_IncludeRaw(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	c := mtls_certificates.MTLSCertificate{ID: "mtls-cert-raw", Name: "raw-cert"}

	a := p.mtlsCertificateToAsset("cert-acct-id-1", c)
	if a.Raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(a.Raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if back["id"] != "mtls-cert-raw" {
		t.Errorf("Raw.id = %v, want mtls-cert-raw", back["id"])
	}
}

// certPack helper coverage — these guard the defensive extraction used for
// the SDK's untyped certificate-pack list items.
func TestCertPackHelpers_DefensiveExtraction(t *testing.T) {
	if got := certPackString(nil, "id"); got != "" {
		t.Errorf("certPackString(nil) = %q, want empty", got)
	}
	m := map[string]interface{}{
		"id":    42, // wrong type on purpose
		"hosts": []interface{}{"a.example.com", 7, "b.example.com"},
	}
	if got := certPackString(m, "id"); got != "" {
		t.Errorf("certPackString(non-string) = %q, want empty", got)
	}
	hosts := certPackHosts(m)
	if len(hosts) != 2 || hosts[0] != "a.example.com" || hosts[1] != "b.example.com" {
		t.Errorf("certPackHosts = %v, want non-string entries skipped", hosts)
	}
	if got := certPackHosts(nil); len(got) != 0 {
		t.Errorf("certPackHosts(nil) = %v, want empty", got)
	}
}
