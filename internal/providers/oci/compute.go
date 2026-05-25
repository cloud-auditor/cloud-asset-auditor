package oci

import (
	"context"
	"fmt"

	"github.com/oracle/oci-go-sdk/v65/core"

	cinventory "github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectComputeInstances enumerates every Compute instance in one
// compartment from one region. Pagination uses the standard OpcNextPage
// loop — there's no AutoPager in oci-go-sdk.
func (p *Provider) collectComputeInstances(ctx context.Context, region, compartmentOCID string, out chan<- cinventory.Asset) error {
	client, err := core.NewComputeClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("compute client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListInstances(ctx, core.ListInstancesRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list instances: %w", err)
		}
		for _, inst := range resp.Items {
			if !sendAsset(ctx, out, p.computeInstanceToAsset(inst, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) computeInstanceToAsset(i core.Instance, region string) cinventory.Asset {
	return cinventory.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.compute.instance",
		ID:        derefStr(i.Id),
		Name:      derefStr(i.DisplayName),
		Status:    string(i.LifecycleState),
		CreatedAt: derefTime(i.TimeCreated),
		Tags: mergeFreeformTags(i.FreeformTags,
			[2]string{"compartment_id", derefStr(i.CompartmentId)},
			[2]string{"availability_domain", derefStr(i.AvailabilityDomain)},
			[2]string{"shape", derefStr(i.Shape)},
		),
		Raw: p.rawOf(i),
	}
}
