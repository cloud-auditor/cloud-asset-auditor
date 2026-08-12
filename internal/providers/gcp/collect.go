package gcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Collect streams every resource under the configured scope. searchAllResources
// is a single paginated list, so there's no per-resource fan-out — we walk the
// pages and emit each result as it arrives, keeping memory bounded for large
// organizations.
func (p *Provider) Collect(ctx context.Context) (<-chan core.Asset, <-chan error) {
	assets := make(chan core.Asset)
	errs := make(chan error, 4)
	go func() {
		defer close(assets)
		defer close(errs)
		p.run(ctx, assets, errs)
	}()
	return assets, errs
}

func (p *Provider) run(ctx context.Context, assets chan<- core.Asset, errs chan<- error) {
	// No scope configured → quiet no-op. This is the "unconfigured" state (no
	// GOOGLE_CLOUD_PROJECT/GCP_SCOPE and no --gcp-* flag), so an all-provider
	// audit on a non-GCP machine emits nothing rather than a spurious error.
	if p.cfg.Scope == "" {
		slog.Debug("gcp: no project/scope configured; skipping (set GOOGLE_CLOUD_PROJECT or --gcp-project)")
		return
	}
	if err := validateScope(p.cfg.Scope); err != nil {
		emitErr(ctx, errs, err)
		return
	}

	quotaProject := p.quotaProject()
	pageToken := ""
	// maxPages is a backstop: 100k pages × 500 results = 50M assets, far beyond
	// any real organization. Pagination termination otherwise depends entirely
	// on the server, so we also bail if the token stops advancing.
	const maxPages = 100_000
	for page := 0; ; page++ {
		resp, err := p.client.searchAllResources(ctx, p.cfg.Scope, pageToken, quotaProject)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			emitErr(ctx, errs, fmt.Errorf("gcp searchAllResources (%s): %w", p.cfg.Scope, err))
			return
		}
		for _, r := range resp.Results {
			if !sendAsset(ctx, assets, p.resourceToAsset(r)) {
				return
			}
		}
		// Stop when the server reports no more pages OR refuses to advance the
		// token (a non-progressing token would loop forever).
		if resp.NextPageToken == "" || resp.NextPageToken == pageToken {
			return
		}
		if page+1 >= maxPages {
			emitErr(ctx, errs, fmt.Errorf("gcp: pagination exceeded %d pages for %s; stopping", maxPages, p.cfg.Scope))
			return
		}
		pageToken = resp.NextPageToken
	}
}

func emitErr(ctx context.Context, errs chan<- error, err error) {
	select {
	case errs <- err:
	case <-ctx.Done():
	}
}
