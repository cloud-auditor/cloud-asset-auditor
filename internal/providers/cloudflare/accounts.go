package cloudflare

import (
	"context"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/accounts"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// accountsPageSize is the explicit per_page used when listing accounts so the
// "short page means last page" termination check below is well-defined.
const accountsPageSize = 50

// listAccounts fetches every account the API token can see, exactly once
// per Provider — all account-scoped collectors fan out from this list, so
// the result is cached behind a sync.Once (same pattern as the OCI
// provider's Object Storage namespace lookup).
//
// We page manually rather than via the SDK's ListAutoPaging because that
// auto-pager only stops when a page comes back EMPTY (it ignores
// result_info.total_pages, which the typed envelope doesn't even expose). The
// Cloudflare /accounts endpoint, asked for a page past the last, REPEATS the
// final page's accounts instead of returning an empty result — so the
// auto-pager increments the page number forever and only escapes by getting
// rate-limited (HTTP 429) ~185 pages in. That manifested as an audit that
// emitted the zone list and then hung for over a minute. Three stop
// conditions defend against it: an empty page, a short page (< per_page), and
// a page that introduces no new account IDs (the direct repeat guard).
func (p *Provider) listAccounts(ctx context.Context) ([]accounts.Account, error) {
	p.accountsOnce.Do(func() {
		seen := map[string]bool{}
		for page := int64(1); ; page++ {
			res, err := p.client.Accounts.List(ctx, accounts.AccountListParams{
				Page:    cf.F(float64(page)),
				PerPage: cf.F(float64(accountsPageSize)),
			})
			if err != nil {
				p.accountsErr = err
				return
			}
			added := 0
			for _, a := range res.Result {
				if seen[a.ID] {
					continue
				}
				seen[a.ID] = true
				p.accounts = append(p.accounts, a)
				added++
			}
			// Stop on an empty page, a short page (last page), or a page that
			// added nothing new (the /accounts repeat-past-last-page bug).
			if len(res.Result) == 0 || len(res.Result) < accountsPageSize || added == 0 {
				return
			}
		}
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
