package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
			err := fn(gctx)
			if err == nil || errors.Is(err, context.Canceled) {
				return nil
			}
			// Skip services the account simply hasn't enabled (e.g. R2 code
			// 10042) — not a scope gap or a failure. filterServiceDisabled
			// drops only the disabled-service causes, so a real error joined
			// alongside one still surfaces. See diagnostics.go.
			filtered := filterServiceDisabled(err)
			if filtered == nil {
				slog.Debug("cloudflare: skipping disabled service",
					"collector", name, "error", err)
				return nil
			}
			emitErr(gctx, errs, fmt.Errorf("cloudflare %s: %w", name, withScopeHint(filtered)))
			return nil
		})
	}

	// Account diagnostic — runs concurrently with everything else so it can
	// never gate the streaming collectors (a slow or hanging GET /accounts
	// must not stall DNS, which previously streamed in parallel). It warms the
	// sync.Once cache every account-scoped collector shares AND catches the
	// most confusing real-world failure: a token scoped to only Zone/DNS reads
	// makes GET /accounts return success with ZERO accounts, silently zeroing
	// out R2, KV, Workers, D1, Pages, Access, Tunnels, mTLS certs, and account
	// rulesets with no error. Without this, the operator just sees "only DNS"
	// and no explanation. See docs/providers.md "Why am I only getting DNS?".
	g.Go(func() error {
		accts, acctErr := p.listAccounts(gctx)
		switch {
		case errors.Is(acctErr, context.Canceled):
		case acctErr != nil:
			emitErr(gctx, errs, fmt.Errorf("cloudflare accounts: %w", withScopeHint(acctErr)))
		case len(accts) == 0:
			emitErr(gctx, errs, errors.New(noAccountsHint))
		}
		return nil
	})

	// Account-scoped.
	collect("accounts", func(c context.Context) error { return p.collectAccounts(c, assets) })
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
