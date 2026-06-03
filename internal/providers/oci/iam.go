package oci

import (
	"context"
	"fmt"
	"strconv"

	"github.com/oracle/oci-go-sdk/v65/identity"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectPolicies enumerates IAM policies in one compartment. Unlike users,
// groups, and dynamic groups (which live only at the tenancy root), policies
// can be attached to any compartment, so the orchestrator calls this per
// compartment. Identity is a global service, so no region is set.
func (p *Provider) collectPolicies(ctx context.Context, compartmentOCID string, out chan<- core.Asset) error {
	client, err := p.newIdentityClient()
	if err != nil {
		return err
	}

	var page *string
	for {
		resp, err := client.ListPolicies(ctx, identity.ListPoliciesRequest{
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list policies: %w", err)
		}
		for _, pol := range resp.Items {
			if !sendAsset(ctx, out, p.policyToAsset(pol)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) policyToAsset(pol identity.Policy) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Type:      "oci.iam.policy",
		ID:        derefStr(pol.Id),
		Name:      derefStr(pol.Name),
		Status:    string(pol.LifecycleState),
		CreatedAt: derefTime(pol.TimeCreated),
		Tags: mergeFreeformTags(pol.FreeformTags,
			[2]string{"compartment_id", derefStr(pol.CompartmentId)},
			[2]string{"statement_count", strconv.Itoa(len(pol.Statements))},
		),
		Raw: p.rawOf(pol),
	}
}

// collectUsers enumerates IAM users. Users are tenancy-global — they live in
// the tenancy root compartment — so the orchestrator calls this once.
func (p *Provider) collectUsers(ctx context.Context, out chan<- core.Asset) error {
	client, err := p.newIdentityClient()
	if err != nil {
		return err
	}

	var page *string
	for {
		resp, err := client.ListUsers(ctx, identity.ListUsersRequest{
			CompartmentId: &p.tenancyOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list users: %w", err)
		}
		for _, u := range resp.Items {
			if !sendAsset(ctx, out, p.userToAsset(u)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) userToAsset(u identity.User) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Type:      "oci.iam.user",
		ID:        derefStr(u.Id),
		Name:      derefStr(u.Name),
		Status:    string(u.LifecycleState),
		CreatedAt: derefTime(u.TimeCreated),
		Tags: mergeFreeformTags(u.FreeformTags,
			[2]string{"compartment_id", derefStr(u.CompartmentId)},
			[2]string{"email", derefStr(u.Email)},
		),
		Raw: p.rawOf(u),
	}
}

// collectGroups enumerates IAM groups (tenancy-global, like users).
func (p *Provider) collectGroups(ctx context.Context, out chan<- core.Asset) error {
	client, err := p.newIdentityClient()
	if err != nil {
		return err
	}

	var page *string
	for {
		resp, err := client.ListGroups(ctx, identity.ListGroupsRequest{
			CompartmentId: &p.tenancyOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list groups: %w", err)
		}
		for _, gr := range resp.Items {
			if !sendAsset(ctx, out, p.groupToAsset(gr)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) groupToAsset(gr identity.Group) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Type:      "oci.iam.group",
		ID:        derefStr(gr.Id),
		Name:      derefStr(gr.Name),
		Status:    string(gr.LifecycleState),
		CreatedAt: derefTime(gr.TimeCreated),
		Tags: mergeFreeformTags(gr.FreeformTags,
			[2]string{"compartment_id", derefStr(gr.CompartmentId)},
		),
		Raw: p.rawOf(gr),
	}
}

// collectDynamicGroups enumerates IAM dynamic groups (tenancy-global).
func (p *Provider) collectDynamicGroups(ctx context.Context, out chan<- core.Asset) error {
	client, err := p.newIdentityClient()
	if err != nil {
		return err
	}

	var page *string
	for {
		resp, err := client.ListDynamicGroups(ctx, identity.ListDynamicGroupsRequest{
			CompartmentId: &p.tenancyOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list dynamic groups: %w", err)
		}
		for _, dg := range resp.Items {
			if !sendAsset(ctx, out, p.dynamicGroupToAsset(dg)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

func (p *Provider) dynamicGroupToAsset(dg identity.DynamicGroup) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Type:      "oci.iam.dynamic_group",
		ID:        derefStr(dg.Id),
		Name:      derefStr(dg.Name),
		Status:    string(dg.LifecycleState),
		CreatedAt: derefTime(dg.TimeCreated),
		Tags: mergeFreeformTags(dg.FreeformTags,
			[2]string{"compartment_id", derefStr(dg.CompartmentId)},
			[2]string{"matching_rule", derefStr(dg.MatchingRule)},
		),
		Raw: p.rawOf(dg),
	}
}
