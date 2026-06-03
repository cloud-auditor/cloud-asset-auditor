package oci

import (
	"context"
	"fmt"

	"github.com/oracle/oci-go-sdk/v65/keymanagement"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectVaults enumerates KMS vaults in one compartment from one region.
func (p *Provider) collectVaults(ctx context.Context, region, compartmentOCID string, out chan<- core.Asset) error {
	client, err := keymanagement.NewKmsVaultClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("kms vault client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListVaults(ctx, keymanagement.ListVaultsRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list vaults: %w", err)
		}
		for _, v := range resp.Items {
			if !sendAsset(ctx, out, p.vaultToAsset(v, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) vaultToAsset(v keymanagement.VaultSummary, region string) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.vault",
		ID:        derefStr(v.Id),
		Name:      derefStr(v.DisplayName),
		Status:    string(v.LifecycleState),
		CreatedAt: derefTime(v.TimeCreated),
		Tags: mergeFreeformTags(v.FreeformTags,
			[2]string{"compartment_id", derefStr(v.CompartmentId)},
			[2]string{"vault_type", string(v.VaultType)},
			[2]string{"management_endpoint", derefStr(v.ManagementEndpoint)},
		),
		Raw: p.rawOf(v),
	}
}
