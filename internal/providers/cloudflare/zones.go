package cloudflare

import (
	"context"
	"fmt"

	"github.com/cloudflare/cloudflare-go/v4/zones"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// listZones fetches every zone the API token can see. The full slice has to
// be materialized (not streamed) because per-zone collectors fan out from it.
func (p *Provider) listZones(ctx context.Context) ([]zones.Zone, error) {
	iter := p.client.Zones.ListAutoPaging(ctx, zones.ZoneListParams{})
	var out []zones.Zone
	for iter.Next() {
		out = append(out, iter.Current())
	}
	if err := iter.Err(); err != nil {
		return out, fmt.Errorf("list: %w", err)
	}
	return out, nil
}

func (p *Provider) zoneToAsset(z zones.Zone) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: z.Account.ID,
		Type:      "cloudflare.zone",
		ID:        z.ID,
		Name:      z.Name,
		Status:    string(z.Status),
		CreatedAt: timePtr(z.CreatedOn),
		Tags: map[string]string{
			"account_name": z.Account.Name,
			"zone_type":    string(z.Type),
			"paused":       fmt.Sprintf("%t", z.Paused),
		},
		Raw: p.rawOf(z),
	}
}
