package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func TestRetentionPolicy_EmptyIsTheDefault(t *testing.T) {
	var p RetentionPolicy
	if !p.Empty() {
		t.Error("the zero RetentionPolicy must mean 'keep everything'")
	}
	if p.String() != "unlimited" {
		t.Errorf("String() = %q, want %q", p.String(), "unlimited")
	}
	if (RetentionPolicy{KeepLast: 3}).Empty() || (RetentionPolicy{MaxAge: time.Hour}).Empty() {
		t.Error("a policy with either axis set is not empty")
	}
}

func TestAuditsToPrune_EmptyPolicyDeletesNothing(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()
	for i := range 5 {
		seedSeries(t, st, time.Duration(i)*24*time.Hour, []string{"netbird"}, peer("p1", "gw", "connected"))
	}

	doomed, err := st.AuditsToPrune(ctx, RetentionPolicy{}, time.Now())
	if err != nil || len(doomed) != 0 {
		t.Fatalf("AuditsToPrune(zero policy) = (%d, %v), want (0, nil)", len(doomed), err)
	}
	removed, err := st.ApplyRetention(ctx, RetentionPolicy{}, time.Now())
	if err != nil || len(removed) != 0 {
		t.Fatalf("ApplyRetention(zero policy) = (%d, %v), want (0, nil)", len(removed), err)
	}
	if audits, _ := st.ListAudits(ctx); len(audits) != 5 {
		t.Errorf("the default policy deleted history: %d snapshots left of 5", len(audits))
	}
}

// KeepLast counts within a provider set, so a frequently-run narrow audit
// cannot evict a rarely-run full one.
func TestAuditsToPrune_KeepLastIsPerProviderSet(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	// Four netbird-only runs, most recent first by age, plus two full audits
	// that are older than every one of them.
	var netbird []int64
	for i := range 4 {
		netbird = append(netbird, seedSeries(t, st, time.Duration(i)*time.Hour,
			[]string{"netbird"}, peer("p1", "gw", "connected")))
	}
	fullNew := seedSeries(t, st, 10*24*time.Hour, []string{"netbird", "cloudflare"}, peer("p1", "gw", "connected"))
	fullOld := seedSeries(t, st, 20*24*time.Hour, []string{"netbird", "cloudflare"}, peer("p1", "gw", "connected"))

	doomed, err := st.AuditsToPrune(ctx, RetentionPolicy{KeepLast: 2}, time.Now())
	if err != nil {
		t.Fatalf("AuditsToPrune: %v", err)
	}
	gone := map[int64]bool{}
	for _, a := range doomed {
		gone[a.ID] = true
	}
	// netbird series: keep the two newest (index 0,1), drop index 2,3.
	if !gone[netbird[2]] || !gone[netbird[3]] {
		t.Errorf("KeepLast 2 did not drop the 3rd/4th netbird snapshot: %v", gone)
	}
	if gone[netbird[0]] || gone[netbird[1]] {
		t.Errorf("KeepLast 2 dropped one of the two newest netbird snapshots: %v", gone)
	}
	// full-audit series: only two exist, so both survive even though they
	// are older than every netbird snapshot.
	if gone[fullNew] || gone[fullOld] {
		t.Errorf("KeepLast evicted the full-audit series: %v", gone)
	}

	// Newest first, so a preview reads chronologically downwards.
	for i := 1; i < len(doomed); i++ {
		if doomed[i-1].RunAt.Before(doomed[i].RunAt) {
			t.Errorf("AuditsToPrune is not newest-first: %v", doomed)
			break
		}
	}
}

func TestAuditsToPrune_MaxAgeIsGlobalAndStrict(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	old := seedSeries(t, st, 48*time.Hour, []string{"netbird"}, peer("p1", "gw", "connected"))
	recent := seedSeries(t, st, 1*time.Hour, []string{"cloudflare"},
		core.Asset{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1"})

	now := time.Now()
	doomed, err := st.AuditsToPrune(ctx, RetentionPolicy{MaxAge: 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("AuditsToPrune: %v", err)
	}
	if len(doomed) != 1 || doomed[0].ID != old {
		t.Fatalf("MaxAge doomed %+v, want only audit %d", doomed, old)
	}

	// The cutoff is strict, and second-resolution: a snapshot taken exactly
	// at the cutoff survives. Same rule PruneAudits has always had.
	var runAt int64
	if err := st.db.QueryRow(`SELECT run_at FROM audits WHERE id=?`, recent).Scan(&runAt); err != nil {
		t.Fatal(err)
	}
	exact := RetentionPolicy{MaxAge: now.Sub(time.Unix(runAt, 0))}
	doomed, err = st.AuditsToPrune(ctx, exact, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range doomed {
		if a.ID == recent {
			t.Error("a snapshot exactly at the cutoff was pruned; the cutoff must be strict")
		}
	}
}

func TestAuditsToPrune_AxesCompose(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	// Three snapshots at 0h, 30h and 60h old, all one series.
	newest := seedSeries(t, st, 0, []string{"netbird"}, peer("p1", "gw", "connected"))
	middle := seedSeries(t, st, 30*time.Hour, []string{"netbird"}, peer("p1", "gw", "connected"))
	oldest := seedSeries(t, st, 60*time.Hour, []string{"netbird"}, peer("p1", "gw", "connected"))

	// KeepLast alone would keep newest+middle; MaxAge alone would keep only
	// newest. A snapshot must survive both, so middle goes.
	doomed, err := st.AuditsToPrune(ctx, RetentionPolicy{KeepLast: 2, MaxAge: 24 * time.Hour}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	gone := map[int64]bool{}
	for _, a := range doomed {
		gone[a.ID] = true
	}
	if !gone[oldest] || !gone[middle] || gone[newest] {
		t.Errorf("composed policy doomed %v, want middle+oldest only", gone)
	}
}

func TestApplyRetention_DeletesAndCascades(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	old := seedSeries(t, st, 48*time.Hour, []string{"netbird"},
		peer("p1", "gw", "connected"), peer("p2", "gw2", "connected"))
	keep := seedSeries(t, st, 0, []string{"netbird"}, peer("p1", "gw", "connected"))

	removed, err := st.ApplyRetention(ctx, RetentionPolicy{MaxAge: 24 * time.Hour}, time.Now())
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if len(removed) != 1 || removed[0].ID != old {
		t.Fatalf("removed = %+v, want audit %d", removed, old)
	}
	if _, err := st.GetAudit(ctx, old); !errors.Is(err, ErrAuditNotFound) {
		t.Errorf("pruned audit still present: %v", err)
	}
	if n := assetCount(t, st, old); n != 0 {
		t.Errorf("%d asset rows survived the retention cascade", n)
	}
	if _, err := st.GetAudit(ctx, keep); err != nil {
		t.Errorf("in-policy audit was deleted: %v", err)
	}
}

func TestApplyRetention_BatchesLargeDeletes(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	n := sqlBatch + 10
	for i := range n {
		seedSeries(t, st, time.Duration(i+2)*time.Hour, []string{"netbird"}, peer("p1", "gw", "connected"))
	}
	seedSeries(t, st, 0, []string{"netbird"}, peer("p1", "gw", "connected"))

	removed, err := st.ApplyRetention(ctx, RetentionPolicy{KeepLast: 1}, time.Now())
	if err != nil {
		t.Fatalf("ApplyRetention over %d snapshots: %v", n, err)
	}
	if len(removed) != n {
		t.Errorf("removed %d snapshots, want %d", len(removed), n)
	}
	audits, err := st.ListAudits(ctx)
	if err != nil || len(audits) != 1 {
		t.Errorf("after KeepLast 1: %d snapshots left, err=%v", len(audits), err)
	}
}

func TestAuditsOlderThan_MatchesPruneAudits(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	old := seedSeries(t, st, 48*time.Hour, []string{"netbird"}, peer("p1", "gw", "connected"))
	seedSeries(t, st, 0, []string{"netbird"}, peer("p1", "gw", "connected"))

	cutoff := time.Now().Add(-24 * time.Hour)
	preview, err := st.AuditsOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("AuditsOlderThan: %v", err)
	}
	if len(preview) != 1 || preview[0].ID != old {
		t.Fatalf("preview = %+v, want audit %d", preview, old)
	}
	n, err := st.PruneAudits(ctx, cutoff)
	if err != nil || n != int64(len(preview)) {
		t.Errorf("PruneAudits removed %d, preview promised %d (err=%v)", n, len(preview), err)
	}
}

func TestCacheStats(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	empty, err := st.CacheStats(ctx)
	if err != nil {
		t.Fatalf("CacheStats: %v", err)
	}
	if empty.Audits != 0 || empty.AssetRows != 0 {
		t.Errorf("empty store stats = %+v, want zero counts", empty)
	}
	if empty.Bytes <= 0 {
		t.Errorf("Bytes = %d, want the database file's size", empty.Bytes)
	}

	seedSeries(t, st, 0, []string{"netbird"}, peer("p1", "gw", "connected"), peer("p2", "gw2", "connected"))
	seedSeries(t, st, 0, []string{"oci"})

	got, err := st.CacheStats(ctx)
	if err != nil {
		t.Fatalf("CacheStats: %v", err)
	}
	if got.Audits != 2 || got.AssetRows != 2 {
		t.Errorf("stats = %+v, want 2 audits / 2 asset rows", got)
	}
}
