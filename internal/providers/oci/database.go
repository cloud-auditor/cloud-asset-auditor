package oci

import (
	"context"
	"fmt"

	"github.com/oracle/oci-go-sdk/v65/database"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectAutonomousDatabases enumerates Autonomous Databases in one
// compartment from one region.
func (p *Provider) collectAutonomousDatabases(ctx context.Context, region, compartmentOCID string, out chan<- core.Asset) error {
	client, err := database.NewDatabaseClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("database client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListAutonomousDatabases(ctx, database.ListAutonomousDatabasesRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list autonomous databases: %w", err)
		}
		for _, db := range resp.Items {
			if !sendAsset(ctx, out, p.autonomousDatabaseToAsset(db, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) autonomousDatabaseToAsset(db database.AutonomousDatabaseSummary, region string) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.autonomous_database",
		ID:        derefStr(db.Id),
		Name:      derefStr(db.DisplayName),
		Status:    string(db.LifecycleState),
		CreatedAt: derefTime(db.TimeCreated),
		Tags: mergeFreeformTags(db.FreeformTags,
			[2]string{"compartment_id", derefStr(db.CompartmentId)},
			[2]string{"db_name", derefStr(db.DbName)},
			[2]string{"db_version", derefStr(db.DbVersion)},
		),
		Raw: p.rawOf(db),
	}
}

// collectDBSystems enumerates DB Systems (the VM/bare-metal database service)
// in one compartment from one region.
func (p *Provider) collectDBSystems(ctx context.Context, region, compartmentOCID string, out chan<- core.Asset) error {
	client, err := database.NewDatabaseClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("database client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListDbSystems(ctx, database.ListDbSystemsRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list db systems: %w", err)
		}
		for _, sys := range resp.Items {
			if !sendAsset(ctx, out, p.dbSystemToAsset(sys, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) dbSystemToAsset(sys database.DbSystemSummary, region string) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.db_system",
		ID:        derefStr(sys.Id),
		Name:      derefStr(sys.DisplayName),
		Status:    string(sys.LifecycleState),
		CreatedAt: derefTime(sys.TimeCreated),
		Tags: mergeFreeformTags(sys.FreeformTags,
			[2]string{"compartment_id", derefStr(sys.CompartmentId)},
			[2]string{"availability_domain", derefStr(sys.AvailabilityDomain)},
			[2]string{"shape", derefStr(sys.Shape)},
			[2]string{"database_edition", string(sys.DatabaseEdition)},
		),
		Raw: p.rawOf(sys),
	}
}
