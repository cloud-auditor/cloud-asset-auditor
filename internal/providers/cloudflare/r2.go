package cloudflare

import (
	"context"
	"fmt"
	"time"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/r2"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// r2ListPageSize is the explicit per_page used when listing R2 buckets so the
// "short page means last page" pagination check below is well-defined.
const r2ListPageSize = 1000

// collectR2 emits one asset per R2 bucket across every account the token can
// see. The v4 SDK has no ListAutoPaging for R2 buckets — BucketService.List
// returns the bare result and discards the envelope's result_info.cursor — so
// pagination is driven by start_after: buckets are ordered lexicographically
// by name, and we resume after the last name of each full page.
func (p *Provider) collectR2(ctx context.Context, out chan<- core.Asset) error {
	accts, err := p.listAccounts(ctx)
	if err != nil {
		return err
	}
	for _, acct := range accts {
		params := r2.BucketListParams{
			AccountID: cf.F(acct.ID),
			PerPage:   cf.F(float64(r2ListPageSize)),
		}
		for {
			page, err := p.client.R2.Buckets.List(ctx, params)
			if err != nil {
				return fmt.Errorf("list r2 buckets: %w", err)
			}
			for _, b := range page.Buckets {
				if !sendAsset(ctx, out, p.r2BucketToAsset(acct.ID, b)) {
					return nil
				}
			}
			if len(page.Buckets) < r2ListPageSize {
				break
			}
			params.StartAfter = cf.F(page.Buckets[len(page.Buckets)-1].Name)
		}
	}
	return nil
}

func (p *Provider) r2BucketToAsset(accountID string, b r2.Bucket) core.Asset {
	tags := map[string]string{}
	if b.Location != "" {
		tags["location"] = string(b.Location)
	}
	if b.StorageClass != "" {
		tags["storage_class"] = string(b.StorageClass)
	}
	if b.Jurisdiction != "" {
		tags["jurisdiction"] = string(b.Jurisdiction)
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: accountID,
		Type:      "cloudflare.r2_bucket",
		// Buckets have no global ID — compose one from account + name.
		ID:        accountID + "/" + b.Name,
		Name:      b.Name,
		CreatedAt: r2ParseCreationDate(b.CreationDate),
		Tags:      tags,
		Raw:       p.rawOf(b),
	}
}

// r2ParseCreationDate converts the SDK's string creation_date (RFC 3339,
// e.g. "2022-06-24T19:58:49.477Z") into Asset.CreatedAt. Empty or unparsable
// values yield nil rather than a bogus timestamp.
func r2ParseCreationDate(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}
