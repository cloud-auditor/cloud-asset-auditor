package cloudflare

import (
	"context"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/zero_trust"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectTunnels emits one asset per cloudflared (Zero Trust) tunnel across
// every account the token can see. Deleted tunnels are excluded at the API
// level via the is_deleted=false filter; a defensive DeletedAt check in the
// loop guards against the filter being ignored by older API behaviour.
func (p *Provider) collectTunnels(ctx context.Context, out chan<- core.Asset) error {
	accts, err := p.listAccounts(ctx)
	if err != nil {
		return err
	}
	for _, a := range accts {
		iter := p.client.ZeroTrust.Tunnels.Cloudflared.ListAutoPaging(ctx, zero_trust.TunnelCloudflaredListParams{
			AccountID: cf.F(a.ID),
			IsDeleted: cf.F(false),
		})
		for iter.Next() {
			t := iter.Current()
			if !t.DeletedAt.IsZero() {
				continue
			}
			if !sendAsset(ctx, out, p.tunnelToAsset(a.ID, t)) {
				return nil
			}
		}
		if err := iter.Err(); err != nil {
			return fmt.Errorf("list tunnels: %w", err)
		}
	}
	return nil
}

func (p *Provider) tunnelToAsset(accountID string, t zero_trust.TunnelCloudflaredListResponse) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: accountID,
		Type:      "cloudflare.tunnel",
		ID:        t.ID,
		Name:      t.Name,
		Status:    string(t.Status),
		CreatedAt: timePtr(t.CreatedAt),
		Tags: map[string]string{
			"tun_type":      string(t.TunType),
			"remote_config": fmt.Sprintf("%t", t.RemoteConfig),
		},
		Raw: p.rawOf(t),
	}
}
