package oci

// Stub collectors — wired into the orchestrator so adding a real
// implementation is a single-file change. Order roughly matches the
// init-plan.md §3 list. Each will follow the same template as
// compute.go / load_balancer.go:
//
//   1. Construct service client with p.auth, then client.SetRegion(region).
//   2. Loop over OpcNextPage pagination.
//   3. Map each item into a core.Asset via a small toAsset helper.
//
// Per-(compartment, region) collectors:

import (
	"context"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// SDK packages to import when implementing (matched by name in the OCI SDK):
//   block volumes      → core (core_blockstorage_client.go)
//   boot volumes       → core (core_blockstorage_client.go)
//   VCNs / subnets     → core (core_virtualnetwork_client.go)
//   object storage     → objectstorage
//   autonomous DBs     → database
//   DB systems         → database
//   functions          → functions
//   container insts.   → containerinstances
//   OKE clusters       → containerengine
//   vaults             → keymanagement
//   policies/users/groups/dynamic-groups → identity (tenancy-global)

func (p *Provider) collectBlockVolumes(_ context.Context, _, _ string, _ chan<- core.Asset) error {
	return nil
}
func (p *Provider) collectBootVolumes(_ context.Context, _, _ string, _ chan<- core.Asset) error {
	return nil
}
func (p *Provider) collectVCNs(_ context.Context, _, _ string, _ chan<- core.Asset) error {
	return nil
}
func (p *Provider) collectSubnets(_ context.Context, _, _ string, _ chan<- core.Asset) error {
	return nil
}
func (p *Provider) collectObjectStorageBuckets(_ context.Context, _, _ string, _ chan<- core.Asset) error {
	return nil
}
func (p *Provider) collectAutonomousDatabases(_ context.Context, _, _ string, _ chan<- core.Asset) error {
	return nil
}
func (p *Provider) collectDBSystems(_ context.Context, _, _ string, _ chan<- core.Asset) error {
	return nil
}
func (p *Provider) collectFunctions(_ context.Context, _, _ string, _ chan<- core.Asset) error {
	return nil
}
func (p *Provider) collectContainerInstances(_ context.Context, _, _ string, _ chan<- core.Asset) error {
	return nil
}
func (p *Provider) collectOKEClusters(_ context.Context, _, _ string, _ chan<- core.Asset) error {
	return nil
}
func (p *Provider) collectVaults(_ context.Context, _, _ string, _ chan<- core.Asset) error {
	return nil
}

// Tenancy-global stubs.

func (p *Provider) collectPolicies(_ context.Context, _ chan<- core.Asset) error      { return nil }
func (p *Provider) collectUsers(_ context.Context, _ chan<- core.Asset) error         { return nil }
func (p *Provider) collectGroups(_ context.Context, _ chan<- core.Asset) error        { return nil }
func (p *Provider) collectDynamicGroups(_ context.Context, _ chan<- core.Asset) error { return nil }
