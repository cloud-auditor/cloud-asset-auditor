// Package store is a SQLite-backed local cache for audit snapshots and an
// encrypted secrets vault. It uses the pure-Go modernc.org/sqlite driver so the
// CGO-free static binary (distroless image, goreleaser cross-builds) keeps
// working — the cgo mattn/go-sqlite3 driver would break that.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver (pure Go)
)

// Store wraps the database connection. It serves two independent concerns —
// the audit cache (audits + assets tables) and the secrets vault (secrets
// table) — that happen to share one file so a user has a single DB to manage.
type Store struct {
	db   *sql.DB
	path string
}

// Path is the database file this Store was opened from. Error messages about
// missing history name it — "no snapshots stored" is unactionable until the
// user knows which file was looked in, and --db/$AUDITOR_DB make that vary.
func (s *Store) Path() string { return s.path }

const schema = `
CREATE TABLE IF NOT EXISTS audits (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	run_at      INTEGER NOT NULL,            -- unix seconds
	providers   TEXT    NOT NULL,            -- sorted, comma-joined provider set
	asset_count INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS assets (
	audit_id   INTEGER NOT NULL REFERENCES audits(id) ON DELETE CASCADE,
	provider   TEXT    NOT NULL,
	account_id TEXT,
	region     TEXT,
	type       TEXT    NOT NULL,
	asset_id   TEXT    NOT NULL,
	name       TEXT,
	status     TEXT,
	created_at TEXT,
	tags       TEXT,                         -- JSON object
	raw        BLOB                          -- JSON (only when --include-raw)
);
CREATE INDEX IF NOT EXISTS idx_assets_audit ON assets(audit_id);
-- idx_assets_audit answers "load one snapshot". The history queries ask the
-- perpendicular question — follow ONE asset across every snapshot — which
-- that index cannot serve at all: without this one, AssetTimeline scans every
-- asset row of every snapshot (assets × snapshots), so on a 50k-asset estate
-- it degrades linearly with how much history you have kept. IF NOT EXISTS
-- means existing databases pick it up on the next Open.
CREATE INDEX IF NOT EXISTS idx_assets_asset_id ON assets(asset_id);
CREATE TABLE IF NOT EXISTS secrets (
	name       TEXT PRIMARY KEY,
	ciphertext BLOB    NOT NULL,
	nonce      BLOB    NOT NULL,
	salt       BLOB    NOT NULL,
	updated_at INTEGER NOT NULL
);
`

// Open opens (creating if necessary) the SQLite database at path, runs
// migrations, and tightens the file permissions to 0600 — the secrets vault
// lives here, so it must not be world-readable.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: create db dir: %w", err)
		}
	}
	// Create the main DB file 0600 BEFORE opening — WAL mode spawns -wal/-shm
	// sidecars and SQLite copies the main file's permission bits to them, so
	// getting the mode right up front keeps the vault's sidecars private too
	// (a later os.Chmod would race the sidecar creation). A zero-byte file is
	// a valid empty database to SQLite.
	if f, err := os.OpenFile(path, os.O_CREATE, 0o600); err == nil {
		_ = f.Close()
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",  // concurrent reads during a write
		"PRAGMA foreign_keys=ON",   // the assets→audits cascade
		"PRAGMA busy_timeout=5000", // wait rather than erroring on a lock
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	_ = os.Chmod(path, 0o600) // best-effort; new files are created 0600 anyway
	return &Store{db: db, path: path}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DefaultPath resolves the database location: $AUDITOR_DB if set, otherwise
// <user-config-dir>/auditor/auditor.db (falling back to ./auditor.db when the
// config dir can't be determined).
func DefaultPath() string {
	if p := os.Getenv("AUDITOR_DB"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return "auditor.db"
	}
	return filepath.Join(dir, "auditor", "auditor.db")
}
