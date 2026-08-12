package oci

import (
	"context"
	"fmt"
	"strings"

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

// filterCompartments narrows a compartment tree to those selected by
// --oci-compartments. Each selector is either a compartment OCID (exact match,
// detected by the ocid1. prefix) or a compartment name (case-insensitive). A
// selected compartment pulls in its entire subtree — an operator who scopes to
// "Production" means Production and everything beneath it; for an inventory tool
// under-scoping (silently hiding child compartments) is worse than over-scoping.
//
// An empty selector list returns all unchanged. A name that matches several
// compartments (names aren't unique across the tree) selects every match (and
// each of their subtrees). The output preserves the input order.
func filterCompartments(all []identity.Compartment, want []string) []identity.Compartment {
	if len(want) == 0 {
		return all
	}

	idSel := make(map[string]struct{})
	nameSel := make(map[string]struct{})
	for _, w := range want {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if isCompartmentOCID(w) {
			idSel[w] = struct{}{}
		} else {
			nameSel[strings.ToLower(w)] = struct{}{}
		}
	}

	// Directly-selected compartment OCIDs, and the parent of every compartment
	// (for the upward subtree-membership walk).
	matched := make(map[string]bool, len(all))
	parent := make(map[string]string, len(all))
	for _, c := range all {
		id := derefStr(c.Id)
		if id == "" {
			continue
		}
		parent[id] = derefStr(c.CompartmentId)
		if _, ok := idSel[id]; ok {
			matched[id] = true
		}
		if _, ok := nameSel[strings.ToLower(derefStr(c.Name))]; ok {
			matched[id] = true
		}
	}

	// keep reports whether a compartment is selected or descends from one, by
	// walking parent pointers to the root. The step counter is a belt-and-braces
	// guard against a malformed (cyclic) parent chain — a real tree never cycles.
	keep := func(id string) bool {
		for cur, steps := id, 0; cur != "" && steps <= len(all); cur, steps = parent[cur], steps+1 {
			if matched[cur] {
				return true
			}
		}
		return false
	}

	out := make([]identity.Compartment, 0, len(all))
	for _, c := range all {
		if keep(derefStr(c.Id)) {
			out = append(out, c)
		}
	}
	return out
}

// isCompartmentOCID reports whether a --oci-compartments selector is an OCID
// (compartment or tenancy) rather than a compartment name. Every OCI OCID
// begins with the "ocid1." prefix.
func isCompartmentOCID(s string) bool { return strings.HasPrefix(s, "ocid1.") }

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
