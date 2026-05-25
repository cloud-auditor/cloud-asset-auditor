package oci

import (
	"context"
	"fmt"

	"github.com/oracle/oci-go-sdk/v65/identity"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// listCompartments returns the tenancy root followed by every accessible
// child compartment in the tree. This is *the* OCI gotcha — most home-grown
// inventory tools forget to recurse and miss everything outside the root.
// We rely on the SDK's CompartmentIdInSubtree=true to do the traversal in
// a single (paginated) request.
func (p *Provider) listCompartments(ctx context.Context) ([]identity.Compartment, error) {
	client, err := p.newIdentityClient()
	if err != nil {
		return nil, err
	}

	tenancyOCID := p.tenancyOCID

	// The tenancy itself is the root compartment but isn't returned by
	// ListCompartments — synthesize it so the caller has a uniform slice.
	out := []identity.Compartment{
		{
			Id:             &tenancyOCID,
			Name:           ptrString("(tenancy root)"),
			CompartmentId:  nil,
			LifecycleState: identity.CompartmentLifecycleStateActive,
		},
	}

	var page *string
	subtree := true
	for {
		resp, err := client.ListCompartments(ctx, identity.ListCompartmentsRequest{
			CompartmentId:          &tenancyOCID,
			CompartmentIdInSubtree: &subtree,
			AccessLevel:            identity.ListCompartmentsAccessLevelAccessible,
			LifecycleState:         identity.CompartmentLifecycleStateActive,
			Page:                   page,
		})
		if err != nil {
			return out, fmt.Errorf("list compartments: %w", err)
		}
		out = append(out, resp.Items...)
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			break
		}
		page = resp.OpcNextPage
	}
	return out, nil
}

// newIdentityClient constructs a client using the resolved auth provider.
// Identity is a regional service that defaults to the home region when no
// explicit region override is set — sufficient for compartment + tenancy
// operations.
func (p *Provider) newIdentityClient() (identity.IdentityClient, error) {
	client, err := identity.NewIdentityClientWithConfigurationProvider(p.auth)
	if err != nil {
		return client, fmt.Errorf("identity client: %w", err)
	}
	return client, nil
}

// checkTenancyAccess proves the auth chain works end-to-end by fetching the
// tenancy. Used by Validate.
func (p *Provider) checkTenancyAccess(ctx context.Context) error {
	client, err := p.newIdentityClient()
	if err != nil {
		return err
	}
	_, err = client.GetTenancy(ctx, identity.GetTenancyRequest{
		TenancyId: &p.tenancyOCID,
	})
	if err != nil {
		return fmt.Errorf("get tenancy: %w", err)
	}
	return nil
}

// compartmentToAsset maps an identity.Compartment to a core.Asset so the
// compartment tree itself shows up in the inventory output.
func (p *Provider) compartmentToAsset(c identity.Compartment) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Type:      "oci.compartment",
		ID:        derefStr(c.Id),
		Name:      derefStr(c.Name),
		Status:    string(c.LifecycleState),
		CreatedAt: derefTime(c.TimeCreated),
		Tags:      mergeFreeformTags(c.FreeformTags, [2]string{"parent_compartment_id", derefStr(c.CompartmentId)}),
		Raw:       p.rawOf(c),
	}
}

func ptrString(s string) *string { return &s }
