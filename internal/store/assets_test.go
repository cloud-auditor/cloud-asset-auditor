package store

// White-box tests (package store, not store_test): PruneAudits needs audits
// with distinct run_at values, and run_at is stamped time.Now() inside
// SaveAudit — so these tests backdate rows through the unexported s.db
// handle instead of sleeping across second boundaries.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func openWhiteBox(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "assets.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// backdateAudit shifts one audit's run_at into the past.
func backdateAudit(t *testing.T, st *Store, id int64, by time.Duration) {
	t.Helper()
	if _, err := st.db.Exec(`UPDATE audits SET run_at = run_at - ? WHERE id=?`,
		int64(by.Seconds()), id); err != nil {
		t.Fatalf("backdate audit %d: %v", id, err)
	}
}

func assetCount(t *testing.T, st *Store, auditID int64) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE audit_id=?`, auditID).Scan(&n); err != nil {
		t.Fatalf("count assets: %v", err)
	}
	return n
}

func TestListAudits_EmptyDB(t *testing.T) {
	st := openWhiteBox(t)
	audits, err := st.ListAudits(context.Background())
	if err != nil {
		t.Fatalf("ListAudits: %v", err)
	}
	if audits == nil || len(audits) != 0 {
		t.Errorf("ListAudits on empty DB = %#v, want empty non-nil slice", audits)
	}
}

func TestListAudits_NewestFirst(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	first, err := st.SaveAudit(ctx, []string{"netbird"}, []core.Asset{{Provider: "netbird", Type: "netbird.peer", ID: "p1"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.SaveAudit(ctx, []string{"oci", "cloudflare"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Same run_at second: id DESC breaks the tie, newest insert first.
	audits, err := st.ListAudits(ctx)
	if err != nil {
		t.Fatalf("ListAudits: %v", err)
	}
	if len(audits) != 2 || audits[0].ID != second || audits[1].ID != first {
		t.Fatalf("ListAudits order = %+v, want [%d %d]", audits, second, first)
	}

	// run_at dominates id: backdating the second audit sends it last.
	backdateAudit(t, st, second, 2*time.Hour)
	audits, err = st.ListAudits(ctx)
	if err != nil {
		t.Fatalf("ListAudits: %v", err)
	}
	if audits[0].ID != first || audits[1].ID != second {
		t.Errorf("after backdate order = [%d %d], want [%d %d]", audits[0].ID, audits[1].ID, first, second)
	}

	got := audits[1]
	if !reflect.DeepEqual(got.Providers, []string{"cloudflare", "oci"}) {
		t.Errorf("Providers = %v, want canonicalized [cloudflare oci]", got.Providers)
	}
	if got.AssetCount != 0 {
		t.Errorf("AssetCount = %d, want 0", got.AssetCount)
	}
	if time.Since(got.RunAt.Add(2*time.Hour)) > time.Minute {
		t.Errorf("RunAt looks wrong: %v", got.RunAt)
	}
}

func TestGetAudit(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	id, err := st.SaveAudit(ctx, []string{"Cloudflare", "netbird"}, []core.Asset{
		{Provider: "netbird", Type: "netbird.peer", ID: "p1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	m, err := st.GetAudit(ctx, id)
	if err != nil {
		t.Fatalf("GetAudit: %v", err)
	}
	if m.ID != id || m.AssetCount != 1 {
		t.Errorf("GetAudit = %+v, want id=%d asset_count=1", m, id)
	}
	if !reflect.DeepEqual(m.Providers, []string{"cloudflare", "netbird"}) {
		t.Errorf("Providers = %v, want [cloudflare netbird]", m.Providers)
	}
	if time.Since(m.RunAt) > time.Minute {
		t.Errorf("RunAt looks wrong: %v", m.RunAt)
	}

	if _, err := st.GetAudit(ctx, id+1); !errors.Is(err, ErrAuditNotFound) {
		t.Errorf("GetAudit(missing) err = %v, want ErrAuditNotFound", err)
	}
}

func TestAuditAssets_RoundTrip(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	want := []core.Asset{
		{
			Provider: "netbird", AccountID: "a1", Region: "eu", Type: "netbird.peer",
			ID: "p1", Name: "gw", Status: "connected", CreatedAt: &created,
			Tags: map[string]string{"ip": "100.64.0.1"},
			Raw:  json.RawMessage(`{"x":1}`),
		},
		{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1", Name: "example.com"},
	}
	id, err := st.SaveAudit(ctx, []string{"netbird", "cloudflare"}, want)
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.AuditAssets(ctx, id)
	if err != nil {
		t.Fatalf("AuditAssets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d assets, want 2", len(got))
	}
	peer := got[0]
	if peer.ID != "p1" {
		peer = got[1]
	}
	if peer.Provider != "netbird" || peer.AccountID != "a1" || peer.Region != "eu" ||
		peer.Name != "gw" || peer.Status != "connected" ||
		peer.Tags["ip"] != "100.64.0.1" || string(peer.Raw) != `{"x":1}` {
		t.Errorf("peer round-trip mismatch: %+v", peer)
	}
	if peer.CreatedAt == nil || !peer.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", peer.CreatedAt, created)
	}
}

func TestAuditAssets_NotFoundVsZeroAssets(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	if _, err := st.AuditAssets(ctx, 42); !errors.Is(err, ErrAuditNotFound) {
		t.Errorf("AuditAssets(missing) err = %v, want ErrAuditNotFound", err)
	}

	id, err := st.SaveAudit(ctx, []string{"netbird"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.AuditAssets(ctx, id)
	if err != nil {
		t.Fatalf("AuditAssets(zero-asset audit) err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("zero-asset audit returned %d assets", len(got))
	}
}

func TestDeleteAudit_CascadesAssets(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	id, err := st.SaveAudit(ctx, []string{"netbird"}, []core.Asset{
		{Provider: "netbird", Type: "netbird.peer", ID: "p1"},
		{Provider: "netbird", Type: "netbird.peer", ID: "p2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := assetCount(t, st, id); n != 2 {
		t.Fatalf("seeded %d asset rows, want 2", n)
	}

	found, err := st.DeleteAudit(ctx, id)
	if err != nil || !found {
		t.Fatalf("DeleteAudit: found=%v err=%v", found, err)
	}
	if _, err := st.AuditAssets(ctx, id); !errors.Is(err, ErrAuditNotFound) {
		t.Errorf("AuditAssets after delete err = %v, want ErrAuditNotFound", err)
	}
	if n := assetCount(t, st, id); n != 0 {
		t.Errorf("%d asset rows survived the cascade", n)
	}

	if again, err := st.DeleteAudit(ctx, id); err != nil || again {
		t.Errorf("second DeleteAudit = (%v, %v), want (false, nil)", again, err)
	}
}

func TestPruneAudits_CutoffIsStrict(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	old, err := st.SaveAudit(ctx, []string{"netbird"}, []core.Asset{{Provider: "netbird", Type: "netbird.peer", ID: "p1"}})
	if err != nil {
		t.Fatal(err)
	}
	recent, err := st.SaveAudit(ctx, []string{"netbird"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	backdateAudit(t, st, old, 48*time.Hour)

	n, err := st.PruneAudits(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PruneAudits: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d audits, want 1", n)
	}
	if _, err := st.GetAudit(ctx, old); !errors.Is(err, ErrAuditNotFound) {
		t.Errorf("old audit survived prune: %v", err)
	}
	if _, err := st.GetAudit(ctx, recent); err != nil {
		t.Errorf("recent audit was pruned: %v", err)
	}
	if c := assetCount(t, st, old); c != 0 {
		t.Errorf("%d asset rows survived the prune cascade", c)
	}

	// run_at < cutoff is strict: a row exactly AT the cutoff survives.
	var runAt int64
	if err := st.db.QueryRow(`SELECT run_at FROM audits WHERE id=?`, recent).Scan(&runAt); err != nil {
		t.Fatal(err)
	}
	if n, err := st.PruneAudits(ctx, time.Unix(runAt, 0)); err != nil || n != 0 {
		t.Errorf("PruneAudits(exact cutoff) = (%d, %v), want (0, nil)", n, err)
	}
}

func TestClearAudits(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	if _, err := st.SaveAudit(ctx, []string{"netbird"}, []core.Asset{{Provider: "netbird", Type: "netbird.peer", ID: "p1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveAudit(ctx, []string{"oci"}, nil); err != nil {
		t.Fatal(err)
	}

	n, err := st.ClearAudits(ctx)
	if err != nil || n != 2 {
		t.Fatalf("ClearAudits = (%d, %v), want (2, nil)", n, err)
	}
	audits, err := st.ListAudits(ctx)
	if err != nil || len(audits) != 0 {
		t.Errorf("after clear: %d audits, err=%v", len(audits), err)
	}
	if again, err := st.ClearAudits(ctx); err != nil || again != 0 {
		t.Errorf("second ClearAudits = (%d, %v), want (0, nil)", again, err)
	}
}
