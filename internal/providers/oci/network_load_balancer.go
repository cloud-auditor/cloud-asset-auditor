package oci

import (
	"context"
	"fmt"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/networkloadbalancer"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectNetworkLoadBalancers enumerates Network Load Balancers — the newer,
// layer-4 variant of OCI load balancing — in one compartment from one region.
// They live in a separate SDK package (networkloadbalancer) from the classic
// layer-7 Load Balancers handled in load_balancer.go.
//
// Like the classic LB collector, this feeds Phase 10's topology view: the
// flattened ip_addresses tag is what the DNS→target resolver matches against.
func (p *Provider) collectNetworkLoadBalancers(ctx context.Context, region, compartmentOCID string, out chan<- core.Asset) error {
	client, err := networkloadbalancer.NewNetworkLoadBalancerClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("network load balancer client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListNetworkLoadBalancers(ctx, networkloadbalancer.ListNetworkLoadBalancersRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list network load balancers: %w", err)
		}
		for _, nlb := range resp.Items {
			if !sendAsset(ctx, out, p.networkLoadBalancerToAsset(nlb, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) networkLoadBalancerToAsset(nlb networkloadbalancer.NetworkLoadBalancerSummary, region string) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.network_load_balancer",
		ID:        derefStr(nlb.Id),
		Name:      derefStr(nlb.DisplayName),
		Status:    string(nlb.LifecycleState),
		CreatedAt: derefTime(nlb.TimeCreated),
		Tags: mergeFreeformTags(nlb.FreeformTags,
			[2]string{"compartment_id", derefStr(nlb.CompartmentId)},
			[2]string{"subnet_id", derefStr(nlb.SubnetId)},
			[2]string{"is_private", boolStr(nlb.IsPrivate)},
			[2]string{"ip_addresses", joinNLBIPAddresses(nlb.IpAddresses)},
		),
		Raw: p.rawOf(nlb),
	}
}

// joinNLBIPAddresses flattens the NLB's IP list into a comma-separated string,
// mirroring joinIPAddresses for classic LBs so Phase 10's topology resolver can
// match either kind the same way.
func joinNLBIPAddresses(ips []networkloadbalancer.IpAddress) string {
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
