package cloudflare

import (
	"context"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/kv"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectKV emits one asset per Workers KV namespace across every account
// the token can see. KV namespaces expose no creation time or status in the
// v4 SDK, so neither field is set.
func (p *Provider) collectKV(ctx context.Context, out chan<- core.Asset) error {
	accts, err := p.listAccounts(ctx)
	if err != nil {
		return err
	}
	for _, acct := range accts {
		iter := p.client.KV.Namespaces.ListAutoPaging(ctx, kv.NamespaceListParams{
			AccountID: cf.F(acct.ID),
		})
		for iter.Next() {
			if !sendAsset(ctx, out, p.kvNamespaceToAsset(acct.ID, iter.Current())) {
				return nil
			}
		}
		if err := iter.Err(); err != nil {
			return fmt.Errorf("list kv namespaces: %w", err)
		}
	}
	return nil
}

func (p *Provider) kvNamespaceToAsset(accountID string, ns kv.Namespace) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: accountID,
		Type:      "cloudflare.kv_namespace",
		ID:        ns.ID,
		Name:      ns.Title,
		Tags: map[string]string{
			"supports_url_encoding": fmt.Sprintf("%t", ns.SupportsURLEncoding),
			"beta":                  fmt.Sprintf("%t", ns.Beta),
		},
		Raw: p.rawOf(ns),
	}
}
