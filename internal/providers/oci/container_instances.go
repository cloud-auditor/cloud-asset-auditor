package oci

import (
	"context"
	"fmt"

	"github.com/oracle/oci-go-sdk/v65/containerinstances"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectContainerInstances enumerates Container Instances (the serverless
// container runtime) in one compartment from one region.
func (p *Provider) collectContainerInstances(ctx context.Context, region, compartmentOCID string, out chan<- core.Asset) error {
	client, err := containerinstances.NewContainerInstanceClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("container instances client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListContainerInstances(ctx, containerinstances.ListContainerInstancesRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list container instances: %w", err)
		}
		for _, ci := range resp.Items {
			if !sendAsset(ctx, out, p.containerInstanceToAsset(ci, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) containerInstanceToAsset(ci containerinstances.ContainerInstanceSummary, region string) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.container_instance",
		ID:        derefStr(ci.Id),
		Name:      derefStr(ci.DisplayName),
		Status:    string(ci.LifecycleState),
		CreatedAt: derefTime(ci.TimeCreated),
		Tags: mergeFreeformTags(ci.FreeformTags,
			[2]string{"compartment_id", derefStr(ci.CompartmentId)},
			[2]string{"availability_domain", derefStr(ci.AvailabilityDomain)},
			[2]string{"shape", derefStr(ci.Shape)},
			[2]string{"container_count", iStr(ci.ContainerCount)},
		),
		Raw: p.rawOf(ci),
	}
}
