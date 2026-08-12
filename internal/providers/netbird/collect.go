package netbird

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Collect launches the audit. The account id is resolved first (so every
// collector can stamp it onto its assets), then each resource collector fans
// out under an errgroup capped by --max-concurrency.
func (p *Provider) Collect(ctx context.Context) (<-chan core.Asset, <-chan error) {
	assets := make(chan core.Asset)
	errs := make(chan error, 16)
	go func() {
		defer close(assets)
		defer close(errs)
		p.run(ctx, assets, errs)
	}()
	return assets, errs
}

func (p *Provider) run(ctx context.Context, assets chan<- core.Asset, errs chan<- error) {
	// Warm the account id before fan-out so collectors read a settled value
	// (sync.Once; the goroutines below only read it). A failure here is
	// non-fatal and is surfaced by the accounts collector itself, so it isn't
	// double-reported here.
	p.resolveAccountID(ctx)

	g, gctx := errgroup.WithContext(ctx)
	if p.cfg.MaxConcurrency > 0 {
		g.SetLimit(p.cfg.MaxConcurrency)
	}

	// collect wraps a named collector. Errors flow into errs instead of
	// failing the group — errgroup would otherwise cancel siblings on the
	// first failure, violating invariant 5 (partial failure is not fatal).
	collect := func(name string, fn func(context.Context) error) {
		g.Go(func() error {
			err := fn(gctx)
			if err == nil || errors.Is(err, context.Canceled) {
				return nil
			}
			emitErr(gctx, errs, fmt.Errorf("netbird %s: %w", name, err))
			return nil
		})
	}

	collect("accounts", func(c context.Context) error { return p.collectAccounts(c, assets) })
	collect("peers", func(c context.Context) error { return p.collectPeers(c, assets) })
	collect("groups", func(c context.Context) error { return p.collectGroups(c, assets) })
	collect("policies", func(c context.Context) error { return p.collectPolicies(c, assets) })
	collect("routes", func(c context.Context) error { return p.collectRoutes(c, assets) })
	collect("networks", func(c context.Context) error { return p.collectNetworks(c, assets) })
	collect("nameservers", func(c context.Context) error { return p.collectNameservers(c, assets) })
	collect("setup-keys", func(c context.Context) error { return p.collectSetupKeys(c, assets) })
	collect("users", func(c context.Context) error { return p.collectUsers(c, assets) })
	collect("posture-checks", func(c context.Context) error { return p.collectPostureChecks(c, assets) })

	_ = g.Wait() // never non-nil; errors flow via errs.
}

func emitErr(ctx context.Context, errs chan<- error, err error) {
	select {
	case errs <- err:
	case <-ctx.Done():
	}
}
