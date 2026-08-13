package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/store"
)

// runCacheWithRetention is runCache plus the two standing-policy knobs, which
// live on the ROOT command's persistent flags and so reach `cache` only
// through viper.
func runCacheWithRetention(t *testing.T, dbPath string, keep int, maxAge time.Duration, args ...string) (string, error) {
	t.Helper()
	s := &cliState{v: viper.New()}
	s.v.Set("db", dbPath)
	s.v.Set("cache-retain", keep)
	s.v.Set("cache-retain-age", maxAge)
	cmd := newCacheCmd(s)
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// listIDs returns the surviving snapshot ids, newest first.
func listIDs(t *testing.T, dbPath string) []int64 {
	t.Helper()
	out, err := runCache(t, dbPath, "list", "-o", "json")
	if err != nil {
		t.Fatalf("cache list: %v", err)
	}
	var audits []store.AuditMeta
	if err := json.Unmarshal([]byte(out), &audits); err != nil {
		t.Fatalf("cache list JSON: %v", err)
	}
	ids := make([]int64, len(audits))
	for i, a := range audits {
		ids[i] = a.ID
	}
	return ids
}

// ----------------------------------------------------------------------
// the default: nothing is ever deleted
// ----------------------------------------------------------------------

// This is the whole retention design in one test. The database is the only
// copy of the history; a snapshot deleted on the operator's behalf cannot be
// recomputed, and the moment they would notice is the moment they needed the
// baseline. So the default policy deletes nothing.
func TestWriteCache_DefaultRetentionKeepsEverything(t *testing.T) {
	db := tempDB(t)
	setCacheRetention(store.RetentionPolicy{})
	t.Cleanup(func() { setCacheRetention(store.RetentionPolicy{}) })

	for i := range 6 {
		writeCache(context.Background(), db, []string{"netbird"},
			[]core.Asset{histAsset("p1", "gw", "connected")})
		if got := len(listIDs(t, db)); got != i+1 {
			t.Fatalf("after %d writes the store holds %d snapshots; the default must delete nothing", i+1, got)
		}
	}
}

func TestWriteCache_AppliesTheConfiguredPolicy(t *testing.T) {
	db := tempDB(t)
	setCacheRetention(store.RetentionPolicy{KeepLast: 3})
	t.Cleanup(func() { setCacheRetention(store.RetentionPolicy{}) })

	for range 6 {
		writeCache(context.Background(), db, []string{"netbird"},
			[]core.Asset{histAsset("p1", "gw", "connected")})
	}
	ids := listIDs(t, db)
	if len(ids) != 3 {
		t.Fatalf("KeepLast 3 left %d snapshots: %v", len(ids), ids)
	}
	// The three kept must be the three newest.
	for _, want := range []int64{6, 5, 4} {
		if !containsID(ids, want) {
			t.Errorf("snapshot #%d was pruned but is one of the three newest: %v", want, ids)
		}
	}

	// A different provider set is its own series and is untouched by the
	// netbird series' quota.
	writeCache(context.Background(), db, []string{"oci"}, nil)
	if got := len(listIDs(t, db)); got != 4 {
		t.Errorf("a new provider set left %d snapshots, want 4 (3 netbird + 1 oci)", got)
	}
}

func TestRetentionFromViper(t *testing.T) {
	v := viper.New()
	if p := retentionFromViper(v); !p.Empty() {
		t.Errorf("unset flags = %v, want the empty (keep-everything) policy", p)
	}
	v.Set("cache-retain", 10)
	v.Set("cache-retain-age", 48*time.Hour)
	p := retentionFromViper(v)
	if p.KeepLast != 10 || p.MaxAge != 48*time.Hour {
		t.Errorf("policy = %+v, want KeepLast 10 / MaxAge 48h", p)
	}
}

// ----------------------------------------------------------------------
// cache prune
// ----------------------------------------------------------------------

func TestCachePrune_DryRunDeletesNothing(t *testing.T) {
	db := tempDB(t)
	old := seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
	keep := seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
	backdateAuditRow(t, db, old, 48*3600)

	out, err := runCache(t, db, "prune", "--max-age", "24h", "--dry-run")
	if err != nil {
		t.Fatalf("prune --dry-run: %v", err)
	}
	for _, want := range []string{"Would prune 1 audit snapshot(s)", "drop older than 24h", "Nothing was deleted (--dry-run)"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	// It must name the casualty, not just count it.
	if !strings.Contains(out, itoa64(old)) {
		t.Errorf("dry-run did not name snapshot #%d:\n%s", old, out)
	}
	ids := listIDs(t, db)
	if len(ids) != 2 || !containsID(ids, old) || !containsID(ids, keep) {
		t.Errorf("--dry-run deleted something: %v", ids)
	}
}

func TestCachePrune_KeepIsPerProviderSet(t *testing.T) {
	db := tempDB(t)
	var netbird []int64
	for i := range 4 {
		id := seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
		backdateAuditRow(t, db, id, int64(i)*3600)
		netbird = append(netbird, id)
	}
	full := seedAudit(t, db, []string{"netbird", "cloudflare"}, nil)
	backdateAuditRow(t, db, full, 30*24*3600)

	out, err := runCache(t, db, "prune", "--keep", "2")
	if err != nil {
		t.Fatalf("prune --keep 2: %v", err)
	}
	if !strings.Contains(out, "Pruned 2 audit snapshot(s).") {
		t.Errorf("prune output = %q", out)
	}
	ids := listIDs(t, db)
	// The lone full audit survives despite being the oldest thing in the DB:
	// it is its own series and only one snapshot deep.
	if !containsID(ids, full) {
		t.Errorf("the full-audit series was evicted by the netbird series' quota: %v", ids)
	}
	if !containsID(ids, netbird[0]) || !containsID(ids, netbird[1]) {
		t.Errorf("the two newest netbird snapshots should survive: %v", ids)
	}
	if containsID(ids, netbird[2]) || containsID(ids, netbird[3]) {
		t.Errorf("the two oldest netbird snapshots should be gone: %v", ids)
	}
}

func TestCachePrune_FallsBackToTheConfiguredPolicy(t *testing.T) {
	db := tempDB(t)
	old := seedAudit(t, db, []string{"netbird"}, nil)
	seedAudit(t, db, []string{"netbird"}, nil)
	backdateAuditRow(t, db, old, 48*3600)

	out, err := runCacheWithRetention(t, db, 0, 24*time.Hour, "prune")
	if err != nil {
		t.Fatalf("prune with a configured policy: %v", err)
	}
	if !strings.Contains(out, "Pruned 1 audit snapshot(s).") {
		t.Errorf("prune output = %q", out)
	}
	if ids := listIDs(t, db); containsID(ids, old) {
		t.Errorf("the configured policy was not applied: %v", ids)
	}
}

func TestCachePrune_RefusesWithNoPolicy(t *testing.T) {
	db := tempDB(t)
	seedAudit(t, db, []string{"netbird"}, nil)

	_, err := runCache(t, db, "prune")
	if err == nil || !strings.Contains(err.Error(), "no retention policy") {
		t.Errorf("prune with no policy err = %v, want a refusal naming the knobs", err)
	}
	if _, err := runCache(t, db, "prune", "--keep", "0"); err == nil ||
		!strings.Contains(err.Error(), "--keep must be at least 1") {
		t.Errorf("prune --keep 0 err = %v, want a positive-count error", err)
	}
	if _, err := runCache(t, db, "prune", "--max-age", "0s"); err == nil {
		t.Error("prune --max-age 0s succeeded, want positive-duration error")
	}
}

func TestCachePrune_NothingToDo(t *testing.T) {
	db := tempDB(t)
	seedAudit(t, db, []string{"netbird"}, nil)

	out, err := runCache(t, db, "prune", "--max-age", "24h")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !strings.Contains(out, "Nothing to prune") {
		t.Errorf("prune output = %q, want the nothing-to-do notice", out)
	}
	if len(listIDs(t, db)) != 1 {
		t.Error("a no-op prune deleted something")
	}
}

// ----------------------------------------------------------------------
// cache list footprint
// ----------------------------------------------------------------------

func TestCacheList_ShowsTheFootprintAndPolicy(t *testing.T) {
	db := tempDB(t)
	seedAudit(t, db, []string{"netbird"}, cacheTestAssets)

	out, err := runCache(t, db, "list")
	if err != nil {
		t.Fatalf("cache list: %v", err)
	}
	for _, want := range []string{"1 snapshot(s)", "2 asset row(s)", "on disk at " + db, "Retention: unlimited"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}

	out, err = runCacheWithRetention(t, db, 30, 720*time.Hour, "list")
	if err != nil {
		t.Fatalf("cache list: %v", err)
	}
	if !strings.Contains(out, "Retention: keep last 30 per provider set, drop older than 720h0m0s") {
		t.Errorf("list does not report the configured policy:\n%s", out)
	}
}

// The JSON form stays a bare array of AuditMeta — it is what scripts parse.
func TestCacheList_JSONIsStillABareArray(t *testing.T) {
	db := tempDB(t)
	seedAudit(t, db, []string{"netbird"}, cacheTestAssets)

	out, err := runCache(t, db, "list", "-o", "json")
	if err != nil {
		t.Fatalf("cache list -o json: %v", err)
	}
	var audits []store.AuditMeta
	if err := json.Unmarshal([]byte(out), &audits); err != nil {
		t.Fatalf("JSON output is no longer a bare array (%v):\n%s", err, out)
	}
	if len(audits) != 1 {
		t.Errorf("got %d audits, want 1", len(audits))
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:              "0 B",
		512:            "512 B",
		1024:           "1.0 KiB",
		1536:           "1.5 KiB",
		1024 * 1024:    "1.0 MiB",
		3 * 1 << 30:    "3.0 GiB",
		1 << 40:        "1.0 TiB",
		1024*1024 - 1:  "1024.0 KiB",
		50 * 1024 * 20: "1000.0 KiB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
