package store

// Retention for the audit cache. `audit --cache` appends a snapshot on every
// run and, until this file existed, nothing ever removed one — on a
// 50k-asset estate audited hourly that is a million asset rows a day.
//
// The deliberate default is NO retention at all (see RetentionPolicy.Empty):
// this database is the only place the history lives, a deleted snapshot
// cannot be recomputed from anything, and the moment an operator notices the
// loss is the moment they needed the baseline. Disk is recoverable, history
// is not — so a policy is something the operator opts into, and CacheStats
// exists to make the growth visible before it becomes a problem.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"
)

// RetentionPolicy bounds how much snapshot history the cache keeps. A zero
// field means "no limit on this axis"; the zero policy keeps everything.
//
// The two axes answer different questions and compose: KeepLast bounds disk
// ("no more than N snapshots"), MaxAge bounds the window ("nothing older than
// D days"). A snapshot must survive both to be kept.
type RetentionPolicy struct {
	// KeepLast keeps at most N snapshots PER PROVIDER SET. Per-set, not
	// global, because each provider set is an independent series: an hourly
	// `--provider netbird` run would otherwise evict every weekly full audit
	// long before N snapshots of the full audit had accumulated.
	KeepLast int
	// MaxAge deletes snapshots that ran more than this long ago, across all
	// provider sets. Global because "I don't care about anything older than
	// D" is a statement about time, not about a series.
	MaxAge time.Duration
}

// Empty reports whether the policy imposes no limit at all — the default,
// and the value that means "never delete anything".
func (p RetentionPolicy) Empty() bool { return p.KeepLast <= 0 && p.MaxAge <= 0 }

// String renders the policy for logs and help output.
func (p RetentionPolicy) String() string {
	var parts []string
	if p.KeepLast > 0 {
		parts = append(parts, fmt.Sprintf("keep last %d per provider set", p.KeepLast))
	}
	if p.MaxAge > 0 {
		parts = append(parts, fmt.Sprintf("drop older than %s", p.MaxAge))
	}
	if len(parts) == 0 {
		return "unlimited"
	}
	return strings.Join(parts, ", ")
}

// AuditsToPrune returns the snapshots the policy would delete, newest first,
// WITHOUT deleting anything. It is the preview `auditor cache prune
// --dry-run` prints: a retention policy that can only be evaluated by running
// it is a policy nobody can safely adopt.
//
// now is a parameter rather than time.Now() so the caller (and the tests)
// control the clock the age axis is measured against.
func (s *Store) AuditsToPrune(ctx context.Context, p RetentionPolicy, now time.Time) ([]AuditMeta, error) {
	if p.Empty() {
		return nil, nil
	}
	audits, err := s.ListAudits(ctx) // newest first
	if err != nil {
		return nil, err
	}

	// Bucket by the canonical provider key so KeepLast counts within a
	// series. ListAudits already ordered newest-first, so each bucket keeps
	// that order and the index IS the "how many newer snapshots exist" rank.
	series := map[string][]AuditMeta{}
	for _, a := range audits {
		key := providerKey(a.Providers)
		series[key] = append(series[key], a)
	}

	// Match PruneAudits' second-resolution cutoff exactly: run_at is stored as
	// unix seconds, so a cutoff carrying sub-second precision would make two
	// spellings of the same policy disagree on a snapshot taken in that second.
	var cutoff time.Time
	if p.MaxAge > 0 {
		cutoff = time.Unix(now.Add(-p.MaxAge).Unix(), 0)
	}

	var doomed []AuditMeta
	for _, key := range slices.Sorted(maps.Keys(series)) {
		for i, a := range series[key] {
			switch {
			case p.KeepLast > 0 && i >= p.KeepLast:
			case p.MaxAge > 0 && a.RunAt.Before(cutoff):
			default:
				continue
			}
			doomed = append(doomed, a)
		}
	}
	sort.SliceStable(doomed, func(i, j int) bool {
		if !doomed[i].RunAt.Equal(doomed[j].RunAt) {
			return doomed[i].RunAt.After(doomed[j].RunAt)
		}
		return doomed[i].ID > doomed[j].ID
	})
	return doomed, nil
}

// ApplyRetention deletes the snapshots AuditsToPrune identifies and returns
// them (assets cascade via the foreign key). The returned slice is what was
// actually removed, so a caller can name the casualties rather than print a
// bare count.
func (s *Store) ApplyRetention(ctx context.Context, p RetentionPolicy, now time.Time) ([]AuditMeta, error) {
	doomed, err := s.AuditsToPrune(ctx, p, now)
	if err != nil || len(doomed) == 0 {
		return nil, err
	}
	ids := make([]int64, len(doomed))
	for i, a := range doomed {
		ids[i] = a.ID
	}
	if err := s.deleteAudits(ctx, ids); err != nil {
		return nil, err
	}
	return doomed, nil
}

// AuditsOlderThan lists the snapshots a MaxAge prune would remove. It is the
// preview counterpart of PruneAudits and shares its strict `<` cutoff.
func (s *Store) AuditsOlderThan(ctx context.Context, olderThan time.Time) ([]AuditMeta, error) {
	audits, err := s.ListAudits(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := time.Unix(olderThan.Unix(), 0)
	var out []AuditMeta
	for _, a := range audits {
		if a.RunAt.Before(cutoff) {
			out = append(out, a)
		}
	}
	return out, nil
}

// deleteAudits removes snapshots by id, batched so a large prune can't blow
// the bind-parameter limit.
func (s *Store) deleteAudits(ctx context.Context, ids []int64) error {
	for chunk := range slices.Chunk(ids, sqlBatch) {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM audits WHERE id IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("store: delete audits: %w", err)
		}
	}
	return nil
}

// CacheStats is the cache's footprint: what is stored and what it costs.
type CacheStats struct {
	Audits    int   `json:"audits"`
	AssetRows int64 `json:"asset_rows"`
	// Bytes is the whole database file (page_count × page_size), so it
	// includes the secrets vault — which is a handful of rows next to any
	// real snapshot history.
	Bytes int64 `json:"bytes"`
}

// CacheStats reports how much the cache is holding. Unbounded growth is only
// a surprise when it is invisible; this is what `cache list` prints so the
// operator can decide on a retention policy before the disk decides for them.
func (s *Store) CacheStats(ctx context.Context) (CacheStats, error) {
	var st CacheStats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audits`).Scan(&st.Audits); err != nil {
		return st, fmt.Errorf("store: count audits: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM assets`).Scan(&st.AssetRows); err != nil {
		return st, fmt.Errorf("store: count assets: %w", err)
	}
	var pages, pageSize int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return st, fmt.Errorf("store: page_count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return st, fmt.Errorf("store: page_size: %w", err)
	}
	st.Bytes = pages * pageSize
	return st, nil
}
