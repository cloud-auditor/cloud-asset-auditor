package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/pages"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectPages emits one asset per Cloudflare Pages project across every
// account the token can see.
func (p *Provider) collectPages(ctx context.Context, out chan<- core.Asset) error {
	accts, err := p.listAccounts(ctx)
	if err != nil {
		return err
	}
	for _, acct := range accts {
		iter := p.client.Pages.Projects.ListAutoPaging(ctx, pages.ProjectListParams{
			AccountID: cf.F(acct.ID),
		})
		for iter.Next() {
			pr := pagesProjectFromListItem(iter.Current())
			if !sendAsset(ctx, out, p.pagesProjectToAsset(acct.ID, pr)) {
				return nil
			}
		}
		if err := iter.Err(); err != nil {
			return fmt.Errorf("list pages projects: %w", err)
		}
	}
	return nil
}

// pagesProjectFromListItem recovers the full pages.Project from a list item.
// The v4 SDK types GET /accounts/{id}/pages/projects as returning
// []pages.Deployment (a generator quirk), but the wire JSON is actually
// project objects — the project-only fields (name, subdomain, domains,
// production_branch) survive only in the item's raw JSON, so re-decode from
// there. When no raw JSON is present (synthetic structs in tests, or a
// decode failure), fall back to the fields Deployment and Project share.
func pagesProjectFromListItem(d pages.Deployment) pages.Project {
	if raw := d.JSON.RawJSON(); raw != "" {
		var pr pages.Project
		if err := json.Unmarshal([]byte(raw), &pr); err == nil {
			return pr
		}
	}
	return pages.Project{
		ID:        d.ID,
		Name:      d.ProjectName,
		CreatedOn: d.CreatedOn,
	}
}

func (p *Provider) pagesProjectToAsset(accountID string, pr pages.Project) core.Asset {
	id := pr.ID
	if id == "" {
		id = pr.Name
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: accountID,
		Type:      "cloudflare.pages_project",
		ID:        id,
		Name:      pr.Name,
		CreatedAt: timePtr(pr.CreatedOn),
		Tags: map[string]string{
			"production_branch": pr.ProductionBranch,
			"subdomain":         pr.Subdomain,
			"domains":           strings.Join(pr.Domains, ","),
		},
		Raw: p.rawOf(pr),
	}
}
