package cloudflare

import (
	"context"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/workers"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectWorkers lists Worker scripts in every account the token can see.
// Script names double as IDs in the Workers system, so ID == Name.
func (p *Provider) collectWorkers(ctx context.Context, out chan<- core.Asset) error {
	accts, err := p.listAccounts(ctx)
	if err != nil {
		return err
	}
	for _, acct := range accts {
		iter := p.client.Workers.Scripts.ListAutoPaging(ctx, workers.ScriptListParams{
			AccountID: cf.F(acct.ID),
		})
		for iter.Next() {
			if !sendAsset(ctx, out, p.workerScriptToAsset(acct.ID, iter.Current())) {
				return nil
			}
		}
		if err := iter.Err(); err != nil {
			return fmt.Errorf("list worker scripts: account %s: %w", acct.ID, err)
		}
	}
	return nil
}

func (p *Provider) workerScriptToAsset(accountID string, s workers.Script) core.Asset {
	tags := map[string]string{
		"usage_model": string(s.UsageModel),
		"logpush":     fmt.Sprintf("%t", s.Logpush),
	}
	// Smart Placement mode: prefer the current nested placement object; fall
	// back to the deprecated top-level field older API responses still set.
	if s.Placement.Mode != "" {
		tags["placement_mode"] = string(s.Placement.Mode)
	} else if s.PlacementMode != "" {
		tags["placement_mode"] = string(s.PlacementMode)
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: accountID,
		Type:      "cloudflare.worker_script",
		ID:        s.ID,
		Name:      s.ID,
		CreatedAt: timePtr(s.CreatedOn),
		Tags:      tags,
		Raw:       p.rawOf(s),
	}
}
