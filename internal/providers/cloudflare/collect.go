package cloudflare

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Collect launches the audit. Zones are listed first because several
// per-zone collectors depend on them; everything else fans out under an
// errgroup capped by --max-concurrency.
func (p *Provider) Collect(ctx context.Context) (<-chan core.Asset, <-chan error) {
	assets := make(chan core.Asset)
	errs := make(chan error, 32)
	go func() {
		defer close(assets)
		defer close(errs)
		p.run(ctx, assets, errs)
	}()
	return assets, errs
}

func (p *Provider) run(ctx context.Context, assets chan<- core.Asset, errs chan<- error) {
	zones, err := p.listZones(ctx)
	if err != nil {
		// Don't bail — account-scoped collectors don't need the zone list.
		emitErr(ctx, errs, fmt.Errorf("cloudflare zones: %w", err))
	}

	for _, z := range zones {
		if !sendAsset(ctx, assets, p.zoneToAsset(z)) {
			return
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	if p.cfg.MaxConcurrency > 0 {
		g.SetLimit(p.cfg.MaxConcurrency)
	}

	// collect wraps a named collector. Errors flow into errs instead of
	// returning non-nil from g.Go — errgroup would otherwise cancel
	// siblings on the first failure, which violates init-plan.md §6
	// invariant 5 (partial failure is not fatal).
	collect := func(name string, fn func(context.Context) error) {
		g.Go(func() error {
			if err := fn(gctx); err != nil && !errors.Is(err, context.Canceled) {
				emitErr(gctx, errs, fmt.Errorf("cloudflare %s: %w", name, err))
			}
			return nil
		})
	}

	// Account-scoped (Phase 2 v0.1: all stubs except as noted).
	collect("r2", func(c context.Context) error { return p.collectR2(c, assets) })
	collect("kv", func(c context.Context) error { return p.collectKV(c, assets) })
	collect("workers", func(c context.Context) error { return p.collectWorkers(c, assets) })
	collect("d1", func(c context.Context) error { return p.collectD1(c, assets) })
	collect("pages", func(c context.Context) error { return p.collectPages(c, assets) })
	collect("access", func(c context.Context) error { return p.collectAccessApps(c, assets) })
	collect("tunnels", func(c context.Context) error { return p.collectTunnels(c, assets) })
	collect("certificates", func(c context.Context) error { return p.collectCertificates(c, assets) })
	collect("account-rulesets", func(c context.Context) error { return p.collectAccountRulesets(c, assets) })

	// Per-zone (DNS is implemented; rest are stubs).
	for _, z := range zones {
		z := z
		collect("dns/"+z.Name, func(c context.Context) error { return p.collectDNS(c, z, assets) })
		collect("page-rules/"+z.Name, func(c context.Context) error { return p.collectPageRules(c, z, assets) })
		collect("load-balancers/"+z.Name, func(c context.Context) error { return p.collectLoadBalancers(c, z, assets) })
		collect("zone-rulesets/"+z.Name, func(c context.Context) error { return p.collectZoneRulesets(c, z, assets) })
	}

	_ = g.Wait() // never non-nil; errors flow via errs.
}

func emitErr(ctx context.Context, errs chan<- error, err error) {
	select {
	case errs <- err:
	case <-ctx.Done():
	}
}
