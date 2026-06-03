package oci

import (
	"context"
	"fmt"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/functions"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectFunctions enumerates OCI Functions in one compartment from one
// region. Functions are nested under Applications, so we list applications
// first (emitting each as an asset) and then the functions within each.
func (p *Provider) collectFunctions(ctx context.Context, region, compartmentOCID string, out chan<- core.Asset) error {
	client, err := functions.NewFunctionsManagementClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("functions client: %w", err)
	}
	client.SetRegion(region)

	var page *string
	for {
		resp, err := client.ListApplications(ctx, functions.ListApplicationsRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list applications: %w", err)
		}
		for _, app := range resp.Items {
			if !sendAsset(ctx, out, p.functionsApplicationToAsset(app, region)) {
				return nil
			}
			if err := p.collectFunctionsForApp(ctx, client, derefStr(app.Id), region, out); err != nil {
				return err
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

// collectFunctionsForApp lists every function belonging to one application.
func (p *Provider) collectFunctionsForApp(ctx context.Context, client functions.FunctionsManagementClient, appID, region string, out chan<- core.Asset) error {
	var page *string
	for {
		resp, err := client.ListFunctions(ctx, functions.ListFunctionsRequest{
			ApplicationId: &appID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list functions: %w", err)
		}
		for _, fn := range resp.Items {
			if !sendAsset(ctx, out, p.functionToAsset(fn, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) functionsApplicationToAsset(app functions.ApplicationSummary, region string) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.functions.application",
		ID:        derefStr(app.Id),
		Name:      derefStr(app.DisplayName),
		Status:    string(app.LifecycleState),
		CreatedAt: derefTime(app.TimeCreated),
		Tags: mergeFreeformTags(app.FreeformTags,
			[2]string{"compartment_id", derefStr(app.CompartmentId)},
			[2]string{"subnet_ids", strings.Join(app.SubnetIds, ",")},
		),
		Raw: p.rawOf(app),
	}
}

func (p *Provider) functionToAsset(fn functions.FunctionSummary, region string) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.functions.function",
		ID:        derefStr(fn.Id),
		Name:      derefStr(fn.DisplayName),
		Status:    string(fn.LifecycleState),
		CreatedAt: derefTime(fn.TimeCreated),
		Tags: mergeFreeformTags(fn.FreeformTags,
			[2]string{"compartment_id", derefStr(fn.CompartmentId)},
			[2]string{"application_id", derefStr(fn.ApplicationId)},
			[2]string{"image", derefStr(fn.Image)},
			[2]string{"memory_mb", i64Str(fn.MemoryInMBs)},
		),
		Raw: p.rawOf(fn),
	}
}
