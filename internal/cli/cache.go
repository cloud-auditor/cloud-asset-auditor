package cli

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/spf13/viper"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/store"
)

// cacheRetention holds the retention policy resolved from the persistent
// --cache-retain / --cache-retain-age flags. Package-level and atomic for the
// same reason demoMode is: root.go's PersistentPreRunE is where persistent
// flags are read, and the audit write path below has no *cliState to reach
// through. Storing it here means audit.go needs no knowledge of retention.
var cacheRetention atomic.Pointer[store.RetentionPolicy]

func setCacheRetention(p store.RetentionPolicy) { cacheRetention.Store(&p) }

// currentRetention returns the configured policy, or the zero policy (keep
// everything) when nothing set one — which is also what a test binary that
// never ran the root command sees.
func currentRetention() store.RetentionPolicy {
	if p := cacheRetention.Load(); p != nil {
		return *p
	}
	return store.RetentionPolicy{}
}

// retentionFromViper reads the two retention knobs. One reader for both the
// startup path (root.go) and `cache prune`, so the flag, the env var, and the
// config-file key can't come to mean different things in the two places.
func retentionFromViper(v *viper.Viper) store.RetentionPolicy {
	return store.RetentionPolicy{
		KeepLast: v.GetInt("cache-retain"),
		MaxAge:   v.GetDuration("cache-retain-age"),
	}
}

// retentionAdviceThreshold is the snapshot count above which an unconfigured
// cache mentions that retention exists. Growth is only a nasty surprise while
// it is invisible; the default policy still deletes nothing.
const retentionAdviceThreshold = 50

// retentionAdvised keeps the nudge to once per process — a long-running
// `serve` writing the cache should not repeat it on every audit.
var retentionAdvised atomic.Bool

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
	applyRetention(ctx, st)
}

// applyRetention enforces the configured policy right after a snapshot lands,
// which is the only moment the cache is known to have grown. With no policy
// configured — the default — it deletes nothing and instead nudges once the
// history gets large, because this database is the only copy: a snapshot
// deleted on the operator's behalf cannot be recomputed from anything, and
// the moment they'd notice is the moment they needed the baseline.
func applyRetention(ctx context.Context, st *store.Store) {
	policy := currentRetention()
	if policy.Empty() {
		adviseRetention(ctx, st)
		return
	}
	removed, err := st.ApplyRetention(ctx, policy, time.Now())
	if err != nil {
		slog.Warn("cache: retention failed", "error", err, "policy", policy.String())
		return
	}
	if len(removed) > 0 {
		slog.Info("cache: retention removed snapshots",
			"count", len(removed), "policy", policy.String(), "oldest_kept_after", removed[0].RunAt)
	}
}

func adviseRetention(ctx context.Context, st *store.Store) {
	stats, err := st.CacheStats(ctx)
	if err != nil || stats.Audits < retentionAdviceThreshold || retentionAdvised.Swap(true) {
		return
	}
	slog.Warn("cache: no retention policy is set, so snapshots accumulate forever",
		"snapshots", stats.Audits, "asset_rows", stats.AssetRows, "db_bytes", stats.Bytes,
		"hint", "set --cache-retain N / --cache-retain-age D, or run `auditor cache prune --dry-run` to see what a prune would remove")
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
