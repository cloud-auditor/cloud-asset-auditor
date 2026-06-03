package oci

import (
	"context"
	"fmt"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/core"

	cinventory "github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectVCNs enumerates Virtual Cloud Networks in one compartment from one
// region. Subnets live in the same SDK package (VirtualNetworkClient) and are
// collected by collectSubnets below.
func (p *Provider) collectVCNs(ctx context.Context, region, compartmentOCID string, out chan<- cinventory.Asset) error {
	client, err := core.NewVirtualNetworkClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("virtual network client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListVcns(ctx, core.ListVcnsRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list vcns: %w", err)
		}
		for _, v := range resp.Items {
			if !sendAsset(ctx, out, p.vcnToAsset(v, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) vcnToAsset(v core.Vcn, region string) cinventory.Asset {
	// Prefer the modern multi-CIDR list; fall back to the deprecated single
	// CidrBlock for VCNs created before multi-CIDR support.
	cidrs := v.CidrBlocks
	if len(cidrs) == 0 && derefStr(v.CidrBlock) != "" {
		cidrs = []string{derefStr(v.CidrBlock)}
	}
	return cinventory.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.vcn",
		ID:        derefStr(v.Id),
		Name:      derefStr(v.DisplayName),
		Status:    string(v.LifecycleState),
		CreatedAt: derefTime(v.TimeCreated),
		Tags: mergeFreeformTags(v.FreeformTags,
			[2]string{"compartment_id", derefStr(v.CompartmentId)},
			[2]string{"cidr_blocks", strings.Join(cidrs, ",")},
		),
		Raw: p.rawOf(v),
	}
}

// collectSubnets enumerates subnets in one compartment from one region.
func (p *Provider) collectSubnets(ctx context.Context, region, compartmentOCID string, out chan<- cinventory.Asset) error {
	client, err := core.NewVirtualNetworkClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("virtual network client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListSubnets(ctx, core.ListSubnetsRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list subnets: %w", err)
		}
		for _, s := range resp.Items {
			if !sendAsset(ctx, out, p.subnetToAsset(s, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) subnetToAsset(s core.Subnet, region string) cinventory.Asset {
	return cinventory.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.subnet",
		ID:        derefStr(s.Id),
		Name:      derefStr(s.DisplayName),
		Status:    string(s.LifecycleState),
		CreatedAt: derefTime(s.TimeCreated),
		Tags: mergeFreeformTags(s.FreeformTags,
			[2]string{"compartment_id", derefStr(s.CompartmentId)},
			[2]string{"vcn_id", derefStr(s.VcnId)},
			[2]string{"cidr_block", derefStr(s.CidrBlock)},
		),
		Raw: p.rawOf(s),
	}
}
