package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/store"
)

// readCache returns a cached audit snapshot for the given provider set if one
// newer than maxAge exists. Any error (missing DB, read failure) degrades to
// "not fresh" with a warning so the audit falls back to a live run.
func readCache(ctx context.Context, dbPath string, providers []string, maxAge time.Duration) ([]core.Asset, time.Time, bool) {
	st, err := store.Open(dbPath)
	if err != nil {
		slog.Warn("cache: open failed; running live audit", "error", err)
		return nil, time.Time{}, false
	}
	defer func() { _ = st.Close() }()

	assets, runAt, fresh, err := st.LatestAudit(ctx, providers, maxAge)
	if err != nil {
		slog.Warn("cache: read failed; running live audit", "error", err)
		return nil, time.Time{}, false
	}
	return assets, runAt, fresh
}

// writeCache persists a freshly collected snapshot. Failures only warn — a
// cache-write problem must not fail the audit the user actually asked for.
func writeCache(ctx context.Context, dbPath string, providers []string, assets []core.Asset) {
	st, err := store.Open(dbPath)
	if err != nil {
		slog.Warn("cache: open for write failed", "error", err)
		return
	}
	defer func() { _ = st.Close() }()
	if _, err := st.SaveAudit(ctx, providers, assets); err != nil {
		slog.Warn("cache: write failed", "error", err)
		return
	}
	slog.Debug("cached audit snapshot", "assets", len(assets))
}

// assetChan replays a slice through the channel the renderers consume,
// honoring ctx cancellation.
func assetChan(ctx context.Context, assets []core.Asset) <-chan core.Asset {
	ch := make(chan core.Asset)
	go func() {
		defer close(ch)
		for _, a := range assets {
			select {
			case ch <- a:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// providerNames is the cache key source: the names of the providers that
// actually resolved and will run. Keying on this (rather than the raw request)
// guarantees the cache row describes its own contents — a provider dropped for
// missing creds or an unknown name changes the key, so a partial set can't be
// served as a full audit.
func providerNames(providers []core.Provider) []string {
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.Name())
	}
	return names
}
