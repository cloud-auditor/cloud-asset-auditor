package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"sync"

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

// run resolves which kubeconfig context(s) to scan and dispatches. A single
// context (the default, in-cluster, or one --kube-contexts value) takes the
// original one-cluster path so its client seams stay test-friendly; several
// contexts fan out one child Provider per cluster (runMulti).
func (p *Provider) run(ctx context.Context, assets chan<- core.Asset, errs chan<- error) {
	contexts, err := p.resolveContexts()
	if err != nil {
		emitErr(ctx, errs, fmt.Errorf("kubernetes contexts: %w", err))
		return
	}
	if len(contexts) > 1 {
		p.runMulti(ctx, contexts, assets, errs)
		return
	}
	// Honor a single explicit --kube-contexts value so ensureClients targets
	// it (a no-op when contexts[0] already equals the singular KubeContext).
	p.cfg.KubeContext = contexts[0]
	p.runSingle(ctx, assets, errs)
}

// runMulti scans several clusters in parallel by delegating each to its own
// single-context child Provider and fanning the children's channels back into
// this audit's. Per-cluster failures stay non-fatal — they're tagged with the
// offending context name and forwarded, never cancel siblings.
func (p *Provider) runMulti(ctx context.Context, contexts []string, assets chan<- core.Asset, errs chan<- error) {
	var wg sync.WaitGroup
	for _, name := range contexts {
		child := New(Config{
			KubeContext:           name,
			KubeNamespace:         p.cfg.KubeNamespace,
			KubeExcludeNamespaces: p.cfg.KubeExcludeNamespaces,
			ExcludeHelmSecrets:    p.cfg.ExcludeHelmSecrets,
			ExcludeEvents:         p.cfg.ExcludeEvents,
			MaxConcurrency:        p.cfg.MaxConcurrency,
			IncludeRaw:            p.cfg.IncludeRaw,
		})
		childAssets, childErrs := child.Collect(ctx)
		wg.Add(1)
		go func() {
			defer wg.Done()
			mergeCluster(ctx, name, childAssets, childErrs, assets, errs)
		}()
	}
	wg.Wait()
}

// mergeCluster forwards one child cluster's assets/errors onto the shared
// channels, prefixing each error with the context name so a multi-cluster
// audit's failures are attributable. Stops promptly on ctx cancellation.
func mergeCluster(
	ctx context.Context,
	contextName string,
	srcAssets <-chan core.Asset, srcErrs <-chan error,
	dstAssets chan<- core.Asset, dstErrs chan<- error,
) {
	for srcAssets != nil || srcErrs != nil {
		select {
		case <-ctx.Done():
			return
		case a, ok := <-srcAssets:
			if !ok {
				srcAssets = nil
				continue
			}
			if !sendAsset(ctx, dstAssets, a) {
				return
			}
		case e, ok := <-srcErrs:
			if !ok {
				srcErrs = nil
				continue
			}
			if e == nil {
				continue
			}
			if contextName != "" {
				e = fmt.Errorf("context %q: %w", contextName, e)
			}
			emitErr(ctx, dstErrs, e)
		}
	}
}

func (p *Provider) runSingle(ctx context.Context, assets chan<- core.Asset, errs chan<- error) {
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
