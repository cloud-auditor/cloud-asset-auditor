package oci

import (
	"context"
	"fmt"

	"github.com/oracle/oci-go-sdk/v65/core"

	cinventory "github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectBlockVolumes enumerates block volumes in one compartment from one
// region. AvailabilityDomain is optional in this SDK version, so a single
// per-compartment list returns volumes across every AD.
func (p *Provider) collectBlockVolumes(ctx context.Context, region, compartmentOCID string, out chan<- cinventory.Asset) error {
	client, err := core.NewBlockstorageClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("blockstorage client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListVolumes(ctx, core.ListVolumesRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list volumes: %w", err)
		}
		for _, v := range resp.Items {
			if !sendAsset(ctx, out, p.blockVolumeToAsset(v, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) blockVolumeToAsset(v core.Volume, region string) cinventory.Asset {
	return cinventory.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.block_volume",
		ID:        derefStr(v.Id),
		Name:      derefStr(v.DisplayName),
		Status:    string(v.LifecycleState),
		CreatedAt: derefTime(v.TimeCreated),
		Tags: mergeFreeformTags(v.FreeformTags,
			[2]string{"compartment_id", derefStr(v.CompartmentId)},
			[2]string{"availability_domain", derefStr(v.AvailabilityDomain)},
			[2]string{"size_gb", i64Str(v.SizeInGBs)},
		),
		Raw: p.rawOf(v),
	}
}

// collectBootVolumes enumerates boot volumes in one compartment from one
// region. Like block volumes, AvailabilityDomain is optional here, so we list
// the whole compartment in one pass.
func (p *Provider) collectBootVolumes(ctx context.Context, region, compartmentOCID string, out chan<- cinventory.Asset) error {
	client, err := core.NewBlockstorageClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("blockstorage client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListBootVolumes(ctx, core.ListBootVolumesRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list boot volumes: %w", err)
		}
		for _, v := range resp.Items {
			if !sendAsset(ctx, out, p.bootVolumeToAsset(v, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) bootVolumeToAsset(v core.BootVolume, region string) cinventory.Asset {
	return cinventory.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.boot_volume",
		ID:        derefStr(v.Id),
		Name:      derefStr(v.DisplayName),
		Status:    string(v.LifecycleState),
		CreatedAt: derefTime(v.TimeCreated),
		Tags: mergeFreeformTags(v.FreeformTags,
			[2]string{"compartment_id", derefStr(v.CompartmentId)},
			[2]string{"availability_domain", derefStr(v.AvailabilityDomain)},
			[2]string{"size_gb", i64Str(v.SizeInGBs)},
			[2]string{"image_id", derefStr(v.ImageId)},
		),
		Raw: p.rawOf(v),
	}
}
