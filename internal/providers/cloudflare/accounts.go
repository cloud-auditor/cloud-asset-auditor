package cloudflare

import (
	"context"
	"fmt"

	"github.com/cloudflare/cloudflare-go/v4/accounts"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// listAccounts fetches every account the API token can see, exactly once
// per Provider — all account-scoped collectors fan out from this list, so
// the result is cached behind a sync.Once (same pattern as the OCI
// provider's Object Storage namespace lookup).
func (p *Provider) listAccounts(ctx context.Context) ([]accounts.Account, error) {
	p.accountsOnce.Do(func() {
		iter := p.client.Accounts.ListAutoPaging(ctx, accounts.AccountListParams{})
		for iter.Next() {
			p.accounts = append(p.accounts, iter.Current())
		}
		p.accountsErr = iter.Err()
	})
	if p.accountsErr != nil {
		return nil, fmt.Errorf("list accounts: %w", p.accountsErr)
	}
	return p.accounts, nil
}

// collectAccounts emits one asset per account. Accounts double as grouping
// containers downstream (XLSX sheet-by, topology), the same way OCI emits
// compartments.
func (p *Provider) collectAccounts(ctx context.Context, out chan<- core.Asset) error {
	accts, err := p.listAccounts(ctx)
	if err != nil {
		return err
	}
	for _, a := range accts {
		if !sendAsset(ctx, out, p.accountToAsset(a)) {
			return nil
		}
	}
	return nil
}

func (p *Provider) accountToAsset(a accounts.Account) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: a.ID,
		Type:      "cloudflare.account",
		ID:        a.ID,
		Name:      a.Name,
		CreatedAt: timePtr(a.CreatedOn),
		Raw:       p.rawOf(a),
	}
}
