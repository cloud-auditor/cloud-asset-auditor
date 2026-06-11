package cloudflare

import (
	"context"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/d1"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectD1 emits one asset per D1 database across every account the token
// can see. Account-scoped: GET /accounts/{account_id}/d1/database.
func (p *Provider) collectD1(ctx context.Context, out chan<- core.Asset) error {
	accts, err := p.listAccounts(ctx)
	if err != nil {
		return err
	}
	for _, acct := range accts {
		iter := p.client.D1.Database.ListAutoPaging(ctx, d1.DatabaseListParams{
			AccountID: cf.F(acct.ID),
		})
		for iter.Next() {
			if !sendAsset(ctx, out, p.d1DatabaseToAsset(acct.ID, iter.Current())) {
				return nil
			}
		}
		if err := iter.Err(); err != nil {
			return fmt.Errorf("list d1 databases: %w", err)
		}
	}
	return nil
}

func (p *Provider) d1DatabaseToAsset(accountID string, db d1.DatabaseListResponse) core.Asset {
	var tags map[string]string
	if db.Version != "" {
		tags = map[string]string{"version": db.Version}
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: accountID,
		Type:      "cloudflare.d1_database",
		ID:        db.UUID,
		Name:      db.Name,
		CreatedAt: timePtr(db.CreatedAt),
		Tags:      tags,
		Raw:       p.rawOf(db),
	}
}
