package oci

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/identity"
)

// resolveRegions decides which regions to scan:
//   - empty list  → every region the tenancy is subscribed to (the default)
//   - {"all"}     → same as empty: every subscribed region (explicit sentinel)
//   - explicit    → exactly that list (case-insensitive; lower-cased)
//
// When the subscription lookup fails (e.g. a missing identity permission) the
// default falls back to the tenancy's home region so a single transient failure
// still scans something rather than aborting the whole OCI audit.
//
// The result is always non-empty unless the SDK rejects everything.
func (p *Provider) resolveRegions(ctx context.Context) ([]string, error) {
	want := p.cfg.Regions

	if len(want) == 0 || (len(want) == 1 && strings.EqualFold(want[0], "all")) {
		lister := p.listSubscribedRegions
		if p.listSubscribed != nil { // test seam
			lister = p.listSubscribed
		}
		regions, err := lister(ctx)
		if err != nil {
			if p.homeRegion != "" {
				slog.Warn("oci: could not list subscribed regions; falling back to home region",
					"home_region", p.homeRegion, "error", err)
				return []string{p.homeRegion}, nil
			}
			return nil, err
		}
		return regions, nil
	}

	out := make([]string, 0, len(want))
	for _, r := range want {
		r = strings.ToLower(strings.TrimSpace(r))
		if r != "" {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no usable regions after filtering: %v", want)
	}
	return out, nil
}

// listSubscribedRegions calls /tenancies/{tenancyId}/regionSubscriptions
// and extracts the region name field from each subscription.
func (p *Provider) listSubscribedRegions(ctx context.Context) ([]string, error) {
	client, err := p.newIdentityClient()
	if err != nil {
		return nil, err
	}
	resp, err := client.ListRegionSubscriptions(ctx, identity.ListRegionSubscriptionsRequest{
		TenancyId: &p.tenancyOCID,
	})
	if err != nil {
		return nil, fmt.Errorf("list region subscriptions: %w", err)
	}
	out := make([]string, 0, len(resp.Items))
	for _, sub := range resp.Items {
		if name := derefStr(sub.RegionName); name != "" {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tenancy has no region subscriptions")
	}
	return out, nil
}
