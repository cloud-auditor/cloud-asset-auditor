package oci

import (
	"context"
	"fmt"
	"time"

	"github.com/oracle/oci-go-sdk/v65/containerengine"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectOKEClusters enumerates Container Engine for Kubernetes (OKE) clusters
// in one compartment from one region.
func (p *Provider) collectOKEClusters(ctx context.Context, region, compartmentOCID string, out chan<- core.Asset) error {
	client, err := containerengine.NewContainerEngineClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("container engine client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListClusters(ctx, containerengine.ListClustersRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list oke clusters: %w", err)
		}
		for _, c := range resp.Items {
			if !sendAsset(ctx, out, p.okeClusterToAsset(c, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) okeClusterToAsset(c containerengine.ClusterSummary, region string) core.Asset {
	// Unlike most resources, an OKE cluster's creation time lives in its
	// nested Metadata rather than a top-level TimeCreated field.
	var created *time.Time
	if c.Metadata != nil {
		created = derefTime(c.Metadata.TimeCreated)
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.oke.cluster",
		ID:        derefStr(c.Id),
		Name:      derefStr(c.Name),
		Status:    string(c.LifecycleState),
		CreatedAt: created,
		Tags: mergeFreeformTags(c.FreeformTags,
			[2]string{"compartment_id", derefStr(c.CompartmentId)},
			[2]string{"vcn_id", derefStr(c.VcnId)},
			[2]string{"kubernetes_version", derefStr(c.KubernetesVersion)},
		),
		Raw: p.rawOf(c),
	}
}
