package cloudflare

import (
	"context"
	"fmt"
	"time"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/rulesets"
	"github.com/cloudflare/cloudflare-go/v4/zones"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectAccountRulesets emits one asset per account-scoped ruleset across
// every account the token can see. Account- and zone-scoped rulesets share
// the "cloudflare.ruleset" type (the topology wafBinding resolver matches on
// it); the "scope" tag disambiguates.
func (p *Provider) collectAccountRulesets(ctx context.Context, out chan<- core.Asset) error {
	accts, err := p.listAccounts(ctx)
	if err != nil {
		return err
	}
	for _, acct := range accts {
		iter := p.client.Rulesets.ListAutoPaging(ctx, rulesets.RulesetListParams{
			AccountID: cf.F(acct.ID),
		})
		for iter.Next() {
			if !sendAsset(ctx, out, p.accountRulesetToAsset(acct.ID, iter.Current())) {
				return nil
			}
		}
		if err := iter.Err(); err != nil {
			return fmt.Errorf("list account rulesets: %w", err)
		}
	}
	return nil
}

// collectZoneRulesets emits one asset per ruleset scoped to zone z.
func (p *Provider) collectZoneRulesets(ctx context.Context, z zones.Zone, out chan<- core.Asset) error {
	iter := p.client.Rulesets.ListAutoPaging(ctx, rulesets.RulesetListParams{
		ZoneID: cf.F(z.ID),
	})
	for iter.Next() {
		if !sendAsset(ctx, out, p.zoneRulesetToAsset(z, iter.Current())) {
			return nil
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("list zone rulesets: %w", err)
	}
	return nil
}

func (p *Provider) accountRulesetToAsset(accountID string, r rulesets.RulesetListResponse) core.Asset {
	tags := map[string]string{
		"scope": "account",
		"phase": string(r.Phase),
		"kind":  string(r.Kind),
	}
	if !r.LastUpdated.IsZero() {
		tags["last_updated"] = r.LastUpdated.UTC().Format(time.RFC3339)
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: accountID,
		Type:      "cloudflare.ruleset",
		ID:        r.ID,
		Name:      r.Name,
		Tags:      tags,
		Raw:       p.rawOf(r),
	}
}

func (p *Provider) zoneRulesetToAsset(z zones.Zone, r rulesets.RulesetListResponse) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: z.Account.ID,
		Type:      "cloudflare.ruleset",
		ID:        r.ID,
		Name:      r.Name,
		Tags: map[string]string{
			"scope":     "zone",
			"zone_id":   z.ID,
			"zone_name": z.Name,
			"phase":     string(r.Phase),
			"kind":      string(r.Kind),
		},
		Raw: p.rawOf(r),
	}
}
