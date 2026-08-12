package tailscale

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Collect launches the audit. Every Tailscale resource hangs off the same
// tailnet path, so unlike OCI/Cloudflare there's no account discovery step —
// collectors fan out immediately under an errgroup capped by
// --max-concurrency.
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
			emitErr(gctx, errs, fmt.Errorf("tailscale %s: %w", name, err))
			return nil
		})
	}

	collect("devices", func(c context.Context) error { return p.collectDevices(c, assets) })
	collect("users", func(c context.Context) error { return p.collectUsers(c, assets) })
	collect("keys", func(c context.Context) error { return p.collectKeys(c, assets) })
	collect("dns", func(c context.Context) error { return p.collectDNS(c, assets) })
	collect("acl", func(c context.Context) error { return p.collectACL(c, assets) })

	_ = g.Wait() // never non-nil; errors flow via errs.
}

func emitErr(ctx context.Context, errs chan<- error, err error) {
	select {
	case errs <- err:
	case <-ctx.Done():
	}
}
