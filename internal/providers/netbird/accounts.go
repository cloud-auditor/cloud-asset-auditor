package netbird

import (
	"context"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// account is the subset of GET /api/accounts we map. The overlay network
// ranges live under settings and anchor every peer's IP to this account.
type account struct {
	ID             string `json:"id"`
	Domain         string `json:"domain"`
	DomainCategory string `json:"domain_category"`
	CreatedAt      string `json:"created_at"`
	CreatedBy      string `json:"created_by"`
	Settings       struct {
		NetworkRange   string `json:"network_range"`
		NetworkRangeV6 string `json:"network_range_v6"`
		DNSDomain      string `json:"dns_domain"`
	} `json:"settings"`
}

func (p *Provider) listAccounts(ctx context.Context) ([]account, error) {
	var out []account
	err := p.client.getJSON(ctx, "/api/accounts", &out)
	return out, err
}

// resolveAccounts fetches GET /api/accounts exactly once, caching the full
// list, the primary account id (stamped onto every asset's AccountID), and any
// error. Both the up-front id warm-up in run() and the account collector share
// this single round-trip and single error path — no duplicate request, and the
// resolve error can't be silently swallowed.
func (p *Provider) resolveAccounts(ctx context.Context) ([]account, error) {
	p.accountOnce.Do(func() {
		accts, err := p.listAccounts(ctx)
		if err != nil {
			p.accountErr = err
			return
		}
		p.accounts = accts
		if len(accts) > 0 {
			p.accountID = accts[0].ID
		}
	})
	return p.accounts, p.accountErr
}

// resolveAccountID warms the cache and returns the primary account id. A
// failure leaves the id empty (non-fatal — assets still collect, just without
// an account label; the error surfaces via the account collector).
func (p *Provider) resolveAccountID(ctx context.Context) string {
	_, _ = p.resolveAccounts(ctx)
	return p.accountID
}

func (p *Provider) collectAccounts(ctx context.Context, out chan<- core.Asset) error {
	accts, err := p.resolveAccounts(ctx)
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

func (p *Provider) accountToAsset(a account) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: a.ID,
		Type:      "netbird.account",
		ID:        a.ID,
		Name:      a.Domain,
		CreatedAt: parseTime(a.CreatedAt),
		Tags: tagsOf(
			"domain", a.Domain,
			"domain_category", a.DomainCategory,
			"network_range", a.Settings.NetworkRange,
			"network_range_v6", a.Settings.NetworkRangeV6,
			"dns_domain", a.Settings.DNSDomain,
			"created_by", a.CreatedBy,
		),
		Raw: p.rawOf(a),
	}
}
