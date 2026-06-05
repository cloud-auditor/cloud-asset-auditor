package oci

import (
	"context"
	"fmt"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/loadbalancer"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectLoadBalancers enumerates classic Load Balancers in one compartment
// from one region. (Network Load Balancers — the newer, layer-4 variant —
// live in a separate SDK package; see network_load_balancer.go.)
//
// This collector is specifically what Phase 10's topology view needs to
// connect Cloudflare DNS records to OCI compute / OKE backends.
func (p *Provider) collectLoadBalancers(ctx context.Context, region, compartmentOCID string, out chan<- core.Asset) error {
	client, err := loadbalancer.NewLoadBalancerClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("load balancer client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListLoadBalancers(ctx, loadbalancer.ListLoadBalancersRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list load balancers: %w", err)
		}
		for _, lb := range resp.Items {
			if !sendAsset(ctx, out, p.loadBalancerToAsset(lb, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) loadBalancerToAsset(lb loadbalancer.LoadBalancer, region string) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.load_balancer",
		ID:        derefStr(lb.Id),
		Name:      derefStr(lb.DisplayName),
		Status:    string(lb.LifecycleState),
		CreatedAt: derefTime(lb.TimeCreated),
		Tags: mergeFreeformTags(lb.FreeformTags,
			[2]string{"compartment_id", derefStr(lb.CompartmentId)},
			[2]string{"shape", derefStr(lb.ShapeName)},
			[2]string{"ip_addresses", joinIPAddresses(lb.IpAddresses)},
		),
		Raw: p.rawOf(lb),
	}
}

// joinIPAddresses flattens the LB's IP list into a comma-separated string
// — Phase 10's topology resolver matches DNS records against this.
func joinIPAddresses(ips []loadbalancer.IpAddress) string {
	if len(ips) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ips))
	for _, ip := range ips {
		if ip.IpAddress != nil && *ip.IpAddress != "" {
			parts = append(parts, *ip.IpAddress)
		}
	}
	return strings.Join(parts, ",")
}
