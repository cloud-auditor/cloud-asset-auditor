package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// ErrAuditNotFound is returned (wrapped with the id) when the requested
// audit doesn't exist.
var ErrAuditNotFound = errors.New("audit not found")

// AuditMeta describes one cached audit snapshot without loading its assets.
type AuditMeta struct {
	ID         int64     `json:"id"`
	RunAt      time.Time `json:"run_at"`
	Providers  []string  `json:"providers"`
	AssetCount int       `json:"asset_count"`
}

// SaveAudit persists a snapshot of assets collected from the given provider
// set and returns the new audit id. The whole snapshot is written in one
// transaction so a reader never sees a half-saved audit.
func (s *Store) SaveAudit(ctx context.Context, providers []string, assets []core.Asset) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO audits(run_at, providers, asset_count) VALUES(?,?,?)`,
		time.Now().Unix(), providerKey(providers), len(assets))
	if err != nil {
		return 0, fmt.Errorf("store: insert audit: %w", err)
	}
	auditID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO assets(audit_id,provider,account_id,region,type,asset_id,name,status,created_at,tags,raw)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = stmt.Close() }()

	for _, a := range assets {
		var tags string
		if len(a.Tags) > 0 {
			b, _ := json.Marshal(a.Tags)
			tags = string(b)
		}
		var created string
		if a.CreatedAt != nil {
			created = a.CreatedAt.Format(time.RFC3339)
		}
		var raw []byte
		if len(a.Raw) > 0 {
			raw = []byte(a.Raw)
		}
		if _, err := stmt.ExecContext(ctx, auditID, a.Provider, a.AccountID, a.Region,
			a.Type, a.ID, a.Name, a.Status, created, tags, raw); err != nil {
			return 0, fmt.Errorf("store: insert asset: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit: %w", err)
	}
	return auditID, nil
}

// LatestAudit returns the assets of the most recent audit that ran the SAME
// provider set and is younger than maxAge. fresh is false (with a nil slice)
// when nothing qualifies — the caller should then run a live audit. A maxAge
// of 0 disables the cache (nothing is ever fresh).
func (s *Store) LatestAudit(ctx context.Context, providers []string, maxAge time.Duration) (assets []core.Asset, runAt time.Time, fresh bool, err error) {
	if maxAge <= 0 {
		return nil, time.Time{}, false, nil
	}
	cutoff := time.Now().Add(-maxAge).Unix()

	var id, runAtUnix int64
	row := s.db.QueryRowContext(ctx,
		`SELECT id, run_at FROM audits WHERE providers=? AND run_at>=? ORDER BY run_at DESC, id DESC LIMIT 1`,
		providerKey(providers), cutoff)
	switch err := row.Scan(&id, &runAtUnix); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, time.Time{}, false, nil
	case err != nil:
		return nil, time.Time{}, false, err
	}

	assets, err = s.loadAssets(ctx, id)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	return assets, time.Unix(runAtUnix, 0), true, nil
}

// loadAssets scans one audit's asset rows back into core.Asset values.
func (s *Store) loadAssets(ctx context.Context, auditID int64) ([]core.Asset, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider,account_id,region,type,asset_id,name,status,created_at,tags,raw
		 FROM assets WHERE audit_id=?`, auditID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var assets []core.Asset
	for rows.Next() {
		var a core.Asset
		var created, tags string
		var raw []byte
		if err := rows.Scan(&a.Provider, &a.AccountID, &a.Region, &a.Type, &a.ID,
			&a.Name, &a.Status, &created, &tags, &raw); err != nil {
			return nil, err
		}
		if tags != "" {
			_ = json.Unmarshal([]byte(tags), &a.Tags)
		}
		if len(raw) > 0 {
			a.Raw = append([]byte(nil), raw...)
		}
		if created != "" {
			if t, e := time.Parse(time.RFC3339, created); e == nil {
				a.CreatedAt = &t
			}
		}
		assets = append(assets, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return assets, nil
}

// ListAudits returns every cached audit snapshot, newest first.
func (s *Store) ListAudits(ctx context.Context) ([]AuditMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_at, providers, asset_count FROM audits ORDER BY run_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	audits := []AuditMeta{}
	for rows.Next() {
		m, err := scanAuditMeta(rows.Scan)
		if err != nil {
			return nil, err
		}
		audits = append(audits, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return audits, nil
}

// GetAudit returns the metadata of one cached audit snapshot, or an error
// wrapping ErrAuditNotFound when no audit has that id.
func (s *Store) GetAudit(ctx context.Context, id int64) (AuditMeta, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, run_at, providers, asset_count FROM audits WHERE id=?`, id)
	m, err := scanAuditMeta(row.Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AuditMeta{}, fmt.Errorf("store: audit %d: %w", id, ErrAuditNotFound)
	case err != nil:
		return AuditMeta{}, err
	}
	return m, nil
}

// scanAuditMeta scans one audits row (id, run_at, providers, asset_count)
// via the given Scan function, shared by ListAudits and GetAudit.
func scanAuditMeta(scan func(...any) error) (AuditMeta, error) {
	var m AuditMeta
	var runAt int64
	var providers string
	if err := scan(&m.ID, &runAt, &providers, &m.AssetCount); err != nil {
		return AuditMeta{}, err
	}
	m.RunAt = time.Unix(runAt, 0)
	if providers != "" {
		m.Providers = strings.Split(providers, ",")
	}
	return m, nil
}

// AuditAssets loads the assets of one cached audit snapshot. It returns an
// error wrapping ErrAuditNotFound when no audit has that id — distinct from
// an existing audit that saved zero assets, which yields an empty slice.
func (s *Store) AuditAssets(ctx context.Context, id int64) ([]core.Asset, error) {
	if _, err := s.GetAudit(ctx, id); err != nil {
		return nil, err
	}
	return s.loadAssets(ctx, id)
}

// DeleteAudit removes one cached audit snapshot (its assets cascade via the
// foreign key). found is false when no audit has that id.
func (s *Store) DeleteAudit(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM audits WHERE id=?`, id)
	if err != nil {
		return false, fmt.Errorf("store: delete audit: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// PruneAudits deletes every cached audit snapshot that ran strictly before
// olderThan and returns how many were removed.
func (s *Store) PruneAudits(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM audits WHERE run_at < ?`, olderThan.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: prune audits: %w", err)
	}
	return res.RowsAffected()
}

// ClearAudits deletes every cached audit snapshot and returns how many were
// removed. The secrets vault is untouched.
func (s *Store) ClearAudits(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM audits`)
	if err != nil {
		return 0, fmt.Errorf("store: clear audits: %w", err)
	}
	return res.RowsAffected()
}

// providerKey canonicalizes a provider set into a cache key: lower-cased,
// de-duplicated, sorted, comma-joined. Two audits over the same providers
// (regardless of flag order) share a key; different sets don't collide.
func providerKey(providers []string) string {
	seen := map[string]struct{}{}
	var p []string
	for _, name := range providers {
		n := strings.ToLower(strings.TrimSpace(name))
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		p = append(p, n)
	}
	sort.Strings(p)
	return strings.Join(p, ",")
}
