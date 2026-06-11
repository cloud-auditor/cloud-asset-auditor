package cloudflare

import (
	"context"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/zero_trust"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectAccessApps emits one asset per Zero Trust Access application,
// across every account the token can see. The v4 list response is a
// union-ish generated type (self-hosted / SaaS / SSH / ...), but the
// fields we need (ID, Name, Domain, AUD, SessionDuration, CreatedAt,
// Type) are all hoisted to the top level of
// zero_trust.AccessApplicationListResponse.
func (p *Provider) collectAccessApps(ctx context.Context, out chan<- core.Asset) error {
	accts, err := p.listAccounts(ctx)
	if err != nil {
		return err
	}
	for _, acct := range accts {
		iter := p.client.ZeroTrust.Access.Applications.ListAutoPaging(ctx, zero_trust.AccessApplicationListParams{
			AccountID: cf.F(acct.ID),
		})
		for iter.Next() {
			if !sendAsset(ctx, out, p.accessAppToAsset(acct.ID, iter.Current())) {
				return nil
			}
		}
		if err := iter.Err(); err != nil {
			return fmt.Errorf("list access applications: %w", err)
		}
	}
	return nil
}

func (p *Provider) accessAppToAsset(accountID string, app zero_trust.AccessApplicationListResponse) core.Asset {
	tags := map[string]string{}
	if app.Domain != "" {
		tags["domain"] = app.Domain
	}
	if app.Type != "" {
		tags["type"] = string(app.Type)
	}
	if app.AUD != "" {
		tags["aud"] = app.AUD
	}
	if app.SessionDuration != "" {
		tags["session_duration"] = app.SessionDuration
	}

	// Some app variants (e.g. bookmark) can come back without a UUID;
	// compose a stable fallback so the asset is still uniquely keyed.
	id := app.ID
	if id == "" {
		id = accountID + "/" + app.Name
	}

	return core.Asset{
		Provider:  providerName,
		AccountID: accountID,
		Type:      "cloudflare.access_app",
		ID:        id,
		Name:      app.Name,
		CreatedAt: timePtr(app.CreatedAt),
		Tags:      tags,
		Raw:       p.rawOf(app),
	}
}
