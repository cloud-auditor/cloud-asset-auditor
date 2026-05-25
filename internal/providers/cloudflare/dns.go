package cloudflare

import (
	"context"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/cloudflare/cloudflare-go/v4/zones"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func (p *Provider) collectDNS(ctx context.Context, z zones.Zone, out chan<- core.Asset) error {
	iter := p.client.DNS.Records.ListAutoPaging(ctx, dns.RecordListParams{
		ZoneID: cf.F(z.ID),
	})
	for iter.Next() {
		if !sendAsset(ctx, out, p.dnsRecordToAsset(z, iter.Current())) {
			return nil
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("list dns records: %w", err)
	}
	return nil
}

func (p *Provider) dnsRecordToAsset(z zones.Zone, r dns.RecordResponse) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: z.Account.ID,
		Type:      "cloudflare.dns_record",
		ID:        r.ID,
		Name:      r.Name,
		CreatedAt: timePtr(r.CreatedOn),
		Tags: map[string]string{
			"zone_id":   z.ID,
			"zone_name": z.Name,
			"type":      string(r.Type),
			"content":   r.Content,
			"proxied":   fmt.Sprintf("%t", r.Proxied),
		},
		Raw: p.rawOf(r),
	}
}
