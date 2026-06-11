package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/custom_certificates"
	"github.com/cloudflare/cloudflare-go/v4/mtls_certificates"
	"github.com/cloudflare/cloudflare-go/v4/ssl"
	"github.com/cloudflare/cloudflare-go/v4/zones"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectCertificates emits three certificate families:
//
//   - per-zone certificate packs   (cloudflare.certificate_pack)
//   - per-zone custom certificates (cloudflare.custom_certificate)
//   - per-account mTLS certificates (cloudflare.mtls_certificate)
//
// Collection is best-effort: each family runs even when a previous one
// failed (a 403 on one endpoint must not hide the others), and the
// accumulated errors are returned joined at the end. Context-cancellation
// errors are dropped so a Ctrl+C can't masquerade as a real failure.
func (p *Provider) collectCertificates(ctx context.Context, out chan<- core.Asset) error {
	var errs []error
	record := func(err error) {
		if err != nil && !errors.Is(err, context.Canceled) {
			errs = append(errs, err)
		}
	}

	// listZones returns whatever it managed to fetch even on error, so a
	// partial zone list still yields partial per-zone results.
	zs, err := p.listZones(ctx)
	if err != nil {
		record(fmt.Errorf("list zones: %w", err))
	}

	record(p.collectCertificatePacks(ctx, zs, out))
	record(p.collectCustomCertificates(ctx, zs, out))
	record(p.collectMTLSCertificates(ctx, out))
	return errors.Join(errs...)
}

// collectCertificatePacks lists every certificate pack (all statuses, not
// just active) for each zone. Per-zone failures are accumulated so one
// zone's 403 doesn't hide the rest.
func (p *Provider) collectCertificatePacks(ctx context.Context, zs []zones.Zone, out chan<- core.Asset) error {
	var errs []error
	for _, z := range zs {
		if ctx.Err() != nil {
			break
		}
		iter := p.client.SSL.CertificatePacks.ListAutoPaging(ctx, ssl.CertificatePackListParams{
			ZoneID: cf.F(z.ID),
			// Inventory wants every pack, not just active ones — the
			// default listing filters to active.
			Status: cf.F(ssl.CertificatePackListParamsStatusAll),
		})
		for iter.Next() {
			if !sendAsset(ctx, out, p.certificatePackToAsset(z, iter.Current())) {
				return errors.Join(errs...)
			}
		}
		if err := iter.Err(); err != nil && !errors.Is(err, context.Canceled) {
			errs = append(errs, fmt.Errorf("list certificate packs for zone %s: %w", z.Name, err))
		}
	}
	return errors.Join(errs...)
}

// certificatePackToAsset maps one certificate-pack list item to an Asset.
// The v4 SDK declares the list response as a bare interface{} (the
// underlying API value is a union of universal/advanced pack shapes), so
// the item arrives as a map[string]interface{} decoded by gjson — fields
// are extracted defensively rather than via struct fields.
func (p *Provider) certificatePackToAsset(z zones.Zone, pack interface{}) core.Asset {
	m, _ := pack.(map[string]interface{})
	id := certPackString(m, "id")
	hosts := certPackHosts(m)
	name := strings.Join(hosts, ",")
	if name == "" {
		name = id
	}
	if id == "" {
		// No natural unique ID — compose one (house rule).
		id = z.Account.ID + "/" + name
	}
	status := certPackString(m, "status")
	return core.Asset{
		Provider:  providerName,
		AccountID: z.Account.ID,
		Type:      "cloudflare.certificate_pack",
		ID:        id,
		Name:      name,
		Status:    status,
		Tags: map[string]string{
			"zone_id":               z.ID,
			"zone_name":             z.Name,
			"type":                  certPackString(m, "type"),
			"status":                status,
			"certificate_authority": certPackString(m, "certificate_authority"),
			"hosts":                 strings.Join(hosts, ","),
		},
		Raw: p.rawOf(pack),
	}
}

// certPackString reads a string field from the untyped pack map; missing
// or non-string values yield "".
func certPackString(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
}

// certPackHosts reads the hosts array from the untyped pack map.
func certPackHosts(m map[string]interface{}) []string {
	raw, _ := m["hosts"].([]interface{})
	hosts := make([]string, 0, len(raw))
	for _, h := range raw {
		if s, ok := h.(string); ok {
			hosts = append(hosts, s)
		}
	}
	return hosts
}

// collectCustomCertificates lists every custom (uploaded) SSL certificate
// for each zone. Per-zone failures are accumulated, same as packs.
func (p *Provider) collectCustomCertificates(ctx context.Context, zs []zones.Zone, out chan<- core.Asset) error {
	var errs []error
	for _, z := range zs {
		if ctx.Err() != nil {
			break
		}
		iter := p.client.CustomCertificates.ListAutoPaging(ctx, custom_certificates.CustomCertificateListParams{
			ZoneID: cf.F(z.ID),
		})
		for iter.Next() {
			if !sendAsset(ctx, out, p.customCertificateToAsset(z, iter.Current())) {
				return errors.Join(errs...)
			}
		}
		if err := iter.Err(); err != nil && !errors.Is(err, context.Canceled) {
			errs = append(errs, fmt.Errorf("list custom certificates for zone %s: %w", z.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (p *Provider) customCertificateToAsset(z zones.Zone, c custom_certificates.CustomCertificate) core.Asset {
	// Custom certificates carry no name field — the hosts they cover are
	// the human-readable identity.
	name := strings.Join(c.Hosts, ",")
	if name == "" {
		name = c.ID
	}
	tags := map[string]string{
		"zone_id":   z.ID,
		"zone_name": z.Name,
		"issuer":    c.Issuer,
		"status":    string(c.Status),
	}
	if !c.ExpiresOn.IsZero() {
		tags["expires_on"] = c.ExpiresOn.Format(time.RFC3339)
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: z.Account.ID,
		Type:      "cloudflare.custom_certificate",
		ID:        c.ID,
		Name:      name,
		Status:    string(c.Status),
		CreatedAt: timePtr(c.UploadedOn),
		Tags:      tags,
		Raw:       p.rawOf(c),
	}
}

// collectMTLSCertificates lists account-scoped mTLS certificates for every
// account the token can see.
func (p *Provider) collectMTLSCertificates(ctx context.Context, out chan<- core.Asset) error {
	accts, err := p.listAccounts(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, acct := range accts {
		if ctx.Err() != nil {
			break
		}
		iter := p.client.MTLSCertificates.ListAutoPaging(ctx, mtls_certificates.MTLSCertificateListParams{
			AccountID: cf.F(acct.ID),
		})
		for iter.Next() {
			if !sendAsset(ctx, out, p.mtlsCertificateToAsset(acct.ID, iter.Current())) {
				return errors.Join(errs...)
			}
		}
		if err := iter.Err(); err != nil && !errors.Is(err, context.Canceled) {
			errs = append(errs, fmt.Errorf("list mtls certificates for account %s: %w", acct.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (p *Provider) mtlsCertificateToAsset(accountID string, c mtls_certificates.MTLSCertificate) core.Asset {
	name := c.Name // optional in the API, "Only used for human readability"
	if name == "" {
		name = c.ID
	}
	id := c.ID
	if id == "" {
		id = accountID + "/" + name
	}
	tags := map[string]string{
		"issuer": c.Issuer,
		"ca":     fmt.Sprintf("%t", c.CA),
	}
	if !c.ExpiresOn.IsZero() {
		tags["expires_on"] = c.ExpiresOn.Format(time.RFC3339)
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: accountID,
		Type:      "cloudflare.mtls_certificate",
		ID:        id,
		Name:      name,
		CreatedAt: timePtr(c.UploadedOn),
		Tags:      tags,
		Raw:       p.rawOf(c),
	}
}
