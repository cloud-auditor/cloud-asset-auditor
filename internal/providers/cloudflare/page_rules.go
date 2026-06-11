package cloudflare

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/page_rules"
	"github.com/cloudflare/cloudflare-go/v4/zones"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectPageRules emits one asset per Page Rule in the zone. Unlike most v4
// list endpoints, /zones/{id}/pagerules is not paginated — List returns the
// full result slice in one call, so there is no ListAutoPaging here.
func (p *Provider) collectPageRules(ctx context.Context, z zones.Zone, out chan<- core.Asset) error {
	rules, err := p.client.PageRules.List(ctx, page_rules.PageRuleListParams{
		ZoneID: cf.F(z.ID),
	})
	if err != nil {
		return fmt.Errorf("list page rules: %w", err)
	}
	if rules == nil {
		return nil
	}
	for _, r := range *rules {
		if !sendAsset(ctx, out, p.pageRuleToAsset(z, r)) {
			return nil
		}
	}
	return nil
}

func (p *Provider) pageRuleToAsset(z zones.Zone, r page_rules.PageRule) core.Asset {
	targets := pageRuleTargetURLs(r.Targets)

	// Page Rules have no name field; the first target's URL pattern is the
	// closest human-readable handle, falling back to the rule ID.
	name := r.ID
	if len(targets) > 0 {
		name = targets[0]
	}

	return core.Asset{
		Provider:  providerName,
		AccountID: z.Account.ID,
		Type:      "cloudflare.page_rule",
		ID:        r.ID,
		Name:      name,
		Status:    string(r.Status),
		CreatedAt: timePtr(r.CreatedOn),
		Tags: map[string]string{
			"zone_id":   z.ID,
			"zone_name": z.Name,
			"priority":  strconv.FormatInt(r.Priority, 10),
			"status":    string(r.Status),
			"targets":   strings.Join(targets, ","),
		},
		Raw: p.rawOf(r),
	}
}

// pageRuleTargetURLs extracts the URL constraint pattern from every rule
// target, skipping targets whose constraint value is empty.
func pageRuleTargetURLs(targets []page_rules.Target) []string {
	urls := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.Constraint.Value != "" {
			urls = append(urls, t.Constraint.Value)
		}
	}
	return urls
}
