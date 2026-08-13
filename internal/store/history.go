package store

// Read-side queries for history: picking one snapshot out of the series by
// time, and following a single asset ACROSS snapshots.
//
// Everything the cache needed until now looked at exactly one audit — the
// newest (LatestAudit) or a named one (AuditAssets). These are the queries
// that make the accumulated snapshots worth keeping.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/filter"
)

// AssetIdentity is the (provider, id) pair that identifies an asset across
// snapshots. It is deliberately the same identity internal/diff matches on —
// a timeline that keyed assets differently from the diff would report changes
// the diff calls add + remove, and vice versa.
type AssetIdentity struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// Observation is one asset as it appeared in one stored snapshot.
//
// Asset.Raw is always nil: the raw payload is never loaded here. It is the
// widest column in the table and diff excludes it from comparison anyway, so
// reading it would multiply the cost of a timeline query for data nothing
// downstream looks at.
type Observation struct {
	AuditID int64      `json:"audit_id"`
	RunAt   time.Time  `json:"run_at"`
	Asset   core.Asset `json:"asset"`
}

// NewestAudit returns the most recent snapshot's metadata. A non-empty
// providers narrows to that exact provider set (canonicalized the same way
// the cache key is); nil or empty means "any set". Returns an error wrapping
// ErrAuditNotFound when nothing matches.
func (s *Store) NewestAudit(ctx context.Context, providers []string) (AuditMeta, error) {
	return s.pickAudit(ctx, providers, "", nil, "run_at DESC, id DESC")
}

// OldestAudit returns the earliest snapshot's metadata, same scoping rules as
// NewestAudit. It exists so a caller that found no snapshot old enough can
// tell the user how far back the history actually goes instead of silently
// comparing against something else.
func (s *Store) OldestAudit(ctx context.Context, providers []string) (AuditMeta, error) {
	return s.pickAudit(ctx, providers, "", nil, "run_at ASC, id ASC")
}

// AuditBefore returns the newest snapshot taken at or before at.
//
// "At or before", not "nearest": a snapshot taken after the requested instant
// is not a baseline for it, however close it lands. Rounding forwards would
// quietly answer a different question than the one asked — "what changed
// since the start of the month" compared against a snapshot from the 9th
// reports none of the first nine days' drift while looking authoritative.
func (s *Store) AuditBefore(ctx context.Context, providers []string, at time.Time) (AuditMeta, error) {
	return s.pickAudit(ctx, providers, "run_at<=?", []any{at.Unix()}, "run_at DESC, id DESC")
}

// pickAudit runs the one-row audits lookup shared by the three selectors
// above. cond and order are compile-time constants from this file; only the
// provider key and the caller's bind values ever reach the query as data.
func (s *Store) pickAudit(ctx context.Context, providers []string, cond string, condArgs []any, order string) (AuditMeta, error) {
	where := []string{"1=1"}
	var args []any
	if key := providerKey(providers); key != "" {
		where = append(where, "providers=?")
		args = append(args, key)
	}
	if cond != "" {
		where = append(where, cond)
		args = append(args, condArgs...)
	}

	row := s.db.QueryRowContext(ctx,
		`SELECT id, run_at, providers, asset_count FROM audits WHERE `+
			strings.Join(where, " AND ")+` ORDER BY `+order+` LIMIT 1`, args...)
	m, err := scanAuditMeta(row.Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AuditMeta{}, fmt.Errorf("store: no matching snapshot: %w", ErrAuditNotFound)
	case err != nil:
		return AuditMeta{}, err
	}
	return m, nil
}

// ProviderSet is one distinct provider set in the store, with how much history
// that series has accumulated.
type ProviderSet struct {
	Providers []string  `json:"providers"`
	Count     int       `json:"count"`
	Newest    time.Time `json:"newest"`
}

// Key is the canonical cache key for the set, for comparing two sets without
// caring about the order they were spelled in.
func (p ProviderSet) Key() string { return providerKey(p.Providers) }

// ProviderSets lists the distinct provider sets the store holds snapshots for,
// most history first.
//
// It exists so a command that picks one series on the user's behalf can say
// what it did NOT look at. A store accumulating both a nightly full audit and
// an hourly single-provider one holds two independent histories, and answering
// a question about the estate from whichever series happens to sit nearest a
// timestamp is how "no drift" gets reported over a fraction of the assets.
func (s *Store) ProviderSets(ctx context.Context) ([]ProviderSet, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT providers, COUNT(*), MAX(run_at) FROM audits
		 GROUP BY providers ORDER BY COUNT(*) DESC, providers`)
	if err != nil {
		return nil, fmt.Errorf("store: list provider sets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ProviderSet
	for rows.Next() {
		var (
			providers string
			set       ProviderSet
			newest    int64
		)
		if err := rows.Scan(&providers, &set.Count, &newest); err != nil {
			return nil, err
		}
		if providers != "" {
			set.Providers = strings.Split(providers, ",")
		}
		set.Newest = time.Unix(newest, 0)
		out = append(out, set)
	}
	return out, rows.Err()
}

// MatchAssetIdentities resolves a selector to the distinct identities the
// store has ever recorded. The selector is a case-insensitive glob matched
// against both the asset id and its name — the same grammar and the same
// both-fields rule `auditor reach --from` uses, so one selector learned once
// works in both commands.
//
// The glob is evaluated in Go rather than pushed down as SQL LIKE. LIKE would
// need its own escaping for '%' and '_' (and '_' is everywhere in asset ids),
// and its case folding is ASCII-only while filter.Glob folds Unicode — two
// dialects that disagree at the edges is how a selector silently stops
// matching. DISTINCT already collapses the scan to one row per identity, so
// what crosses the driver is bounded by the size of the estate, not by the
// number of snapshots.
func (s *Store) MatchAssetIdentities(ctx context.Context, selector string) ([]AssetIdentity, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT provider, asset_id, name FROM assets`)
	if err != nil {
		return nil, fmt.Errorf("store: scan asset identities: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[AssetIdentity]struct{}{}
	var out []AssetIdentity
	for rows.Next() {
		var id AssetIdentity
		var name string
		if err := rows.Scan(&id.Provider, &id.ID, &name); err != nil {
			return nil, err
		}
		if !filter.Glob(selector, id.ID) && !filter.Glob(selector, name) {
			continue
		}
		// One identity can carry several names over its life (a rename), so
		// the DISTINCT above can yield the same identity more than once.
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, compareIdentities)
	return out, nil
}

// sqlBatch caps how many identities go into one IN (…) list. SQLite's bind
// parameter limit is well above this on current builds, but it was 999 for
// years and an estate can easily match more than that with a broad glob.
const sqlBatch = 400

// AssetTimeline returns every observation of the given identities across
// every stored snapshot, ordered by identity then by run time (oldest first).
//
// This is the query idx_assets_asset_id exists for: without it, following one
// asset through the history is a full scan of every asset row of every
// snapshot, because idx_assets_audit only answers "give me one snapshot".
func (s *Store) AssetTimeline(ctx context.Context, ids []AssetIdentity) ([]Observation, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	wanted := make(map[AssetIdentity]struct{}, len(ids))
	assetIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, dup := wanted[id]; dup {
			continue
		}
		wanted[id] = struct{}{}
		assetIDs = append(assetIDs, id.ID)
	}

	var out []Observation
	for chunk := range slices.Chunk(assetIDs, sqlBatch) {
		obs, err := s.timelineChunk(ctx, chunk, wanted)
		if err != nil {
			return nil, err
		}
		out = append(out, obs...)
	}

	slices.SortFunc(out, func(a, b Observation) int {
		if c := compareIdentities(identityOf(a.Asset), identityOf(b.Asset)); c != 0 {
			return c
		}
		if !a.RunAt.Equal(b.RunAt) {
			if a.RunAt.Before(b.RunAt) {
				return -1
			}
			return 1
		}
		return int(a.AuditID - b.AuditID)
	})
	return out, nil
}

// timelineChunk reads one batch of asset ids. The asset_id IN (…) predicate
// is an index seek per value; the provider is re-checked in Go because
// identity is (provider, id) and two providers could in principle mint the
// same id string.
func (s *Store) timelineChunk(ctx context.Context, assetIDs []string, wanted map[AssetIdentity]struct{}) ([]Observation, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(assetIDs)), ",")
	args := make([]any, len(assetIDs))
	for i, id := range assetIDs {
		args[i] = id
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT a.audit_id, au.run_at, a.provider, a.account_id, a.region, a.type,
		        a.asset_id, a.name, a.status, a.created_at, a.tags
		 FROM assets a JOIN audits au ON au.id = a.audit_id
		 WHERE a.asset_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: asset timeline: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Observation
	for rows.Next() {
		var o Observation
		var runAt int64
		var created, tags string
		if err := rows.Scan(&o.AuditID, &runAt, &o.Asset.Provider, &o.Asset.AccountID,
			&o.Asset.Region, &o.Asset.Type, &o.Asset.ID, &o.Asset.Name,
			&o.Asset.Status, &created, &tags); err != nil {
			return nil, err
		}
		if _, ok := wanted[identityOf(o.Asset)]; !ok {
			continue
		}
		o.RunAt = time.Unix(runAt, 0)
		if tags != "" {
			_ = json.Unmarshal([]byte(tags), &o.Asset.Tags)
		}
		if created != "" {
			if t, e := time.Parse(time.RFC3339, created); e == nil {
				o.Asset.CreatedAt = &t
			}
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func identityOf(a core.Asset) AssetIdentity {
	return AssetIdentity{Provider: a.Provider, ID: a.ID}
}

func compareIdentities(a, b AssetIdentity) int {
	if a.Provider != b.Provider {
		return strings.Compare(a.Provider, b.Provider)
	}
	return strings.Compare(a.ID, b.ID)
}
