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

// collectNATGateways enumerates NAT gateways in one compartment from one region.
// A NAT gateway carries a public NatIp that lets private instances reach the
// internet outbound-only; that IP is surfaced as the nat_ip tag.
func (p *Provider) collectNATGateways(ctx context.Context, region, compartmentOCID string, out chan<- cinventory.Asset) error {
	client, err := core.NewVirtualNetworkClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("virtual network client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListNatGateways(ctx, core.ListNatGatewaysRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list nat gateways: %w", err)
		}
		for _, g := range resp.Items {
			if !sendAsset(ctx, out, p.natGatewayToAsset(g, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) natGatewayToAsset(g core.NatGateway, region string) cinventory.Asset {
	return cinventory.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.nat_gateway",
		ID:        derefStr(g.Id),
		Name:      derefStr(g.DisplayName),
		Status:    string(g.LifecycleState),
		CreatedAt: derefTime(g.TimeCreated),
		Tags: mergeFreeformTags(g.FreeformTags,
			[2]string{"compartment_id", derefStr(g.CompartmentId)},
			[2]string{"vcn_id", derefStr(g.VcnId)},
			[2]string{"nat_ip", derefStr(g.NatIp)},
			[2]string{"block_traffic", boolStr(g.BlockTraffic)},
		),
		Raw: p.rawOf(g),
	}
}

// collectInternetGateways enumerates internet gateways in one compartment from
// one region. They have no IP of their own (they route a subnet's public IPs),
// so only their VCN association is surfaced.
func (p *Provider) collectInternetGateways(ctx context.Context, region, compartmentOCID string, out chan<- cinventory.Asset) error {
	client, err := core.NewVirtualNetworkClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("virtual network client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListInternetGateways(ctx, core.ListInternetGatewaysRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list internet gateways: %w", err)
		}
		for _, g := range resp.Items {
			if !sendAsset(ctx, out, p.internetGatewayToAsset(g, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) internetGatewayToAsset(g core.InternetGateway, region string) cinventory.Asset {
	return cinventory.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.internet_gateway",
		ID:        derefStr(g.Id),
		Name:      derefStr(g.DisplayName),
		Status:    string(g.LifecycleState),
		CreatedAt: derefTime(g.TimeCreated),
		Tags: mergeFreeformTags(g.FreeformTags,
			[2]string{"compartment_id", derefStr(g.CompartmentId)},
			[2]string{"vcn_id", derefStr(g.VcnId)},
			[2]string{"is_enabled", boolStr(g.IsEnabled)},
		),
		Raw: p.rawOf(g),
	}
}

// collectServiceGateways enumerates service gateways in one compartment from one
// region. A service gateway routes traffic to OCI services (Object Storage,
// etc.) without traversing the internet.
func (p *Provider) collectServiceGateways(ctx context.Context, region, compartmentOCID string, out chan<- cinventory.Asset) error {
	client, err := core.NewVirtualNetworkClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("virtual network client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListServiceGateways(ctx, core.ListServiceGatewaysRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list service gateways: %w", err)
		}
		for _, g := range resp.Items {
			if !sendAsset(ctx, out, p.serviceGatewayToAsset(g, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) serviceGatewayToAsset(g core.ServiceGateway, region string) cinventory.Asset {
	return cinventory.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.service_gateway",
		ID:        derefStr(g.Id),
		Name:      derefStr(g.DisplayName),
		Status:    string(g.LifecycleState),
		CreatedAt: derefTime(g.TimeCreated),
		Tags: mergeFreeformTags(g.FreeformTags,
			[2]string{"compartment_id", derefStr(g.CompartmentId)},
			[2]string{"vcn_id", derefStr(g.VcnId)},
			[2]string{"block_traffic", boolStr(g.BlockTraffic)},
		),
		Raw: p.rawOf(g),
	}
}

// collectLocalPeeringGateways enumerates local peering gateways (LPGs) in one
// compartment from one region. An LPG connects two VCNs in the same region;
// the peer association is surfaced via peer_id and peer_advertised_cidr.
func (p *Provider) collectLocalPeeringGateways(ctx context.Context, region, compartmentOCID string, out chan<- cinventory.Asset) error {
	client, err := core.NewVirtualNetworkClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("virtual network client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListLocalPeeringGateways(ctx, core.ListLocalPeeringGatewaysRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list local peering gateways: %w", err)
		}
		for _, g := range resp.Items {
			if !sendAsset(ctx, out, p.localPeeringGatewayToAsset(g, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) localPeeringGatewayToAsset(g core.LocalPeeringGateway, region string) cinventory.Asset {
	return cinventory.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.local_peering_gateway",
		ID:        derefStr(g.Id),
		Name:      derefStr(g.DisplayName),
		Status:    string(g.LifecycleState),
		CreatedAt: derefTime(g.TimeCreated),
		Tags: mergeFreeformTags(g.FreeformTags,
			[2]string{"compartment_id", derefStr(g.CompartmentId)},
			[2]string{"vcn_id", derefStr(g.VcnId)},
			[2]string{"peering_status", string(g.PeeringStatus)},
			[2]string{"peer_advertised_cidr", derefStr(g.PeerAdvertisedCidr)},
		),
		Raw: p.rawOf(g),
	}
}

// collectDRGs enumerates Dynamic Routing Gateways (DRGs) in one compartment from
// one region. A DRG is a standalone virtual router — it has no IP and isn't
// bound to a single VCN (it attaches to VCNs, IPSec/FastConnect, and other DRGs
// via separate DRG attachments), so only its compartment association is
// surfaced here.
func (p *Provider) collectDRGs(ctx context.Context, region, compartmentOCID string, out chan<- cinventory.Asset) error {
	client, err := core.NewVirtualNetworkClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("virtual network client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListDrgs(ctx, core.ListDrgsRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list drgs: %w", err)
		}
		for _, d := range resp.Items {
			if !sendAsset(ctx, out, p.drgToAsset(d, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) drgToAsset(d core.Drg, region string) cinventory.Asset {
	return cinventory.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.drg",
		ID:        derefStr(d.Id),
		Name:      derefStr(d.DisplayName),
		Status:    string(d.LifecycleState),
		CreatedAt: derefTime(d.TimeCreated),
		Tags: mergeFreeformTags(d.FreeformTags,
			[2]string{"compartment_id", derefStr(d.CompartmentId)},
		),
		Raw: p.rawOf(d),
	}
}
