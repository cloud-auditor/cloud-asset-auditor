package oci

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Collect drives the audit: resolve auth, list the compartment tree, pick
// regions, then fan out per (region, compartment, resource_type) collector
// under an errgroup capped by --max-concurrency.
func (p *Provider) Collect(ctx context.Context) (<-chan core.Asset, <-chan error) {
	assets := make(chan core.Asset)
	errs := make(chan error, 64)
	go func() {
		defer close(assets)
		defer close(errs)
		p.run(ctx, assets, errs)
	}()
	return assets, errs
}

func (p *Provider) run(ctx context.Context, assets chan<- core.Asset, errs chan<- error) {
	if _, err := p.ensureAuth(); err != nil {
		emitErr(ctx, errs, fmt.Errorf("oci auth: %w", err))
		return
	}

	compartments, err := p.listCompartments(ctx)
	if err != nil {
		// Without compartments there's literally nothing to scan — bail
		// rather than silently returning an empty result.
		emitErr(ctx, errs, fmt.Errorf("oci compartments: %w", err))
		return
	}

	for _, c := range compartments {
		if !sendAsset(ctx, assets, p.compartmentToAsset(c)) {
			return
		}
	}

	regions, err := p.resolveRegions(ctx)
	if err != nil {
		emitErr(ctx, errs, fmt.Errorf("oci regions: %w", err))
		return
	}

	g, gctx := errgroup.WithContext(ctx)
	if p.cfg.MaxConcurrency > 0 {
		g.SetLimit(p.cfg.MaxConcurrency)
	}

	// collect wraps a named collector so errors route through errs without
	// cancelling siblings (init-plan.md §6 invariant 5).
	collect := func(label string, fn func(context.Context) error) {
		g.Go(func() error {
			if err := fn(gctx); err != nil && !errors.Is(err, context.Canceled) {
				emitErr(gctx, errs, fmt.Errorf("oci %s: %w", label, err))
			}
			return nil
		})
	}

	// Cross-product: every (region, compartment, resource_type).
	for _, region := range regions {
		for _, c := range compartments {
			cOCID := derefStr(c.Id)
			cName := derefStr(c.Name)
			tag := fmt.Sprintf("%s/%s", region, cName)

			collect("compute "+tag, func(ctx context.Context) error {
				return p.collectComputeInstances(ctx, region, cOCID, assets)
			})
			collect("load-balancers "+tag, func(ctx context.Context) error {
				return p.collectLoadBalancers(ctx, region, cOCID, assets)
			})

			// Stubs — wired so the orchestrator already covers every
			// resource type from init-plan.md §3; fill-in is per-file.
			collect("block-volumes "+tag, func(ctx context.Context) error {
				return p.collectBlockVolumes(ctx, region, cOCID, assets)
			})
			collect("boot-volumes "+tag, func(ctx context.Context) error {
				return p.collectBootVolumes(ctx, region, cOCID, assets)
			})
			collect("vcns "+tag, func(ctx context.Context) error {
				return p.collectVCNs(ctx, region, cOCID, assets)
			})
			collect("subnets "+tag, func(ctx context.Context) error {
				return p.collectSubnets(ctx, region, cOCID, assets)
			})
			collect("object-storage "+tag, func(ctx context.Context) error {
				return p.collectObjectStorageBuckets(ctx, region, cOCID, assets)
			})
			collect("autonomous-dbs "+tag, func(ctx context.Context) error {
				return p.collectAutonomousDatabases(ctx, region, cOCID, assets)
			})
			collect("db-systems "+tag, func(ctx context.Context) error {
				return p.collectDBSystems(ctx, region, cOCID, assets)
			})
			collect("functions "+tag, func(ctx context.Context) error {
				return p.collectFunctions(ctx, region, cOCID, assets)
			})
			collect("container-instances "+tag, func(ctx context.Context) error {
				return p.collectContainerInstances(ctx, region, cOCID, assets)
			})
			collect("oke-clusters "+tag, func(ctx context.Context) error {
				return p.collectOKEClusters(ctx, region, cOCID, assets)
			})
			collect("vaults "+tag, func(ctx context.Context) error {
				return p.collectVaults(ctx, region, cOCID, assets)
			})
		}
	}

	// Tenancy-global resources (don't repeat per region).
	collect("policies", func(ctx context.Context) error {
		return p.collectPolicies(ctx, assets)
	})
	collect("users", func(ctx context.Context) error {
		return p.collectUsers(ctx, assets)
	})
	collect("groups", func(ctx context.Context) error {
		return p.collectGroups(ctx, assets)
	})
	collect("dynamic-groups", func(ctx context.Context) error {
		return p.collectDynamicGroups(ctx, assets)
	})

	_ = g.Wait()
}

func emitErr(ctx context.Context, errs chan<- error, err error) {
	select {
	case errs <- err:
	case <-ctx.Done():
	}
}
