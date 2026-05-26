package kubernetes

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Collect drives the audit: load auth, run discovery, then fan out one
// list-per-GVR under an errgroup capped by --max-concurrency. Per-resource
// failures route to the errs channel without cancelling siblings —
// individual GVRs the ServiceAccount can't list are warnings, not fatals.
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
	if err := p.ensureClients(); err != nil {
		emitErr(ctx, errs, fmt.Errorf("kubernetes auth: %w", err))
		return
	}

	targets, discErr := p.discoverResources()
	if discErr != nil {
		// Partial discovery — emit as warning and keep going with what
		// did discover. Aggregated APIs whose backing service is down
		// show up here all the time.
		emitErr(ctx, errs, fmt.Errorf("kubernetes discovery (partial): %w", discErr))
	}
	if len(targets) == 0 {
		emitErr(ctx, errs, errors.New("kubernetes: no listable resources found"))
		return
	}

	g, gctx := errgroup.WithContext(ctx)
	if p.cfg.MaxConcurrency > 0 {
		g.SetLimit(p.cfg.MaxConcurrency)
	}

	for _, t := range targets {
		label := t.GVR.String()
		g.Go(func() error {
			if err := p.listResource(gctx, t, assets); err != nil && !errors.Is(err, context.Canceled) {
				emitErr(gctx, errs, fmt.Errorf("kubernetes %s: %w", label, err))
			}
			return nil
		})
	}

	_ = g.Wait() // errors flow via errs; the group itself never returns non-nil.
}

func emitErr(ctx context.Context, errs chan<- error, err error) {
	select {
	case errs <- err:
	case <-ctx.Done():
	}
}
