package cloudflare

import (
	"context"
	"fmt"
	"strings"
	"time"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/load_balancers"
	"github.com/cloudflare/cloudflare-go/v4/zones"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func (p *Provider) collectLoadBalancers(ctx context.Context, z zones.Zone, out chan<- core.Asset) error {
	iter := p.client.LoadBalancers.ListAutoPaging(ctx, load_balancers.LoadBalancerListParams{
		ZoneID: cf.F(z.ID),
	})
	for iter.Next() {
		if !sendAsset(ctx, out, p.loadBalancerToAsset(z, iter.Current())) {
			return nil
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("list load balancers: %w", err)
	}
	return nil
}

func (p *Provider) loadBalancerToAsset(z zones.Zone, lb load_balancers.LoadBalancer) core.Asset {
	status := "disabled"
	if lb.Enabled {
		status = "enabled"
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: z.Account.ID,
		Type:      "cloudflare.load_balancer",
		ID:        lb.ID,
		// Name IS the load balancer's DNS hostname — keep it verbatim; the
		// topology resolvers join on hostnames.
		Name:      lb.Name,
		Status:    status,
		CreatedAt: loadBalancerCreatedOn(lb.CreatedOn),
		Tags: map[string]string{
			"zone_id":          z.ID,
			"zone_name":        z.Name,
			"proxied":          fmt.Sprintf("%t", lb.Proxied),
			"steering_policy":  string(lb.SteeringPolicy),
			"session_affinity": string(lb.SessionAffinity),
			"fallback_pool":    lb.FallbackPool,
			"default_pools":    strings.Join(lb.DefaultPools, ","),
		},
		Raw: p.rawOf(lb),
	}
}

// loadBalancerCreatedOn parses a load balancer's created_on timestamp. The
// generated v4 SDK exposes it as a plain string (not time.Time), so timePtr
// can't be used directly. Empty or unparseable values map to nil, matching
// timePtr's zero-time behaviour.
func loadBalancerCreatedOn(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return timePtr(t)
}
