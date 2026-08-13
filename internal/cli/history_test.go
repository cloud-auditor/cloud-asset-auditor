package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/store"
)

// runHistory executes `auditor history <args...>`. Like runDiff, the report
// is captured through --output-file because the renderers write to
// openOutput's writer, not cobra's.
func runHistory(t *testing.T, dbPath string, args ...string) (string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "history.txt")

	s := &cliState{v: viper.New()}
	s.v.Set("db", dbPath)
	cmd := newHistoryCmd(s)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(append([]string{"--output-file", out}, args...))
	err := cmd.Execute()

	report, readErr := os.ReadFile(out)
	if readErr != nil {
		return buf.String(), err
	}
	return string(report), err
}

// seedHistory builds a netbird-only series: p1 changes status, is renamed,
// vanishes, and comes back. Returns the audit ids oldest-first.
func seedHistory(t *testing.T, db string) []int64 {
	t.Helper()
	snapshots := [][]core.Asset{
		{histAsset("p1", "gw", "connected"), histAsset("p2", "spare", "connected")},
		{histAsset("p1", "gw", "disconnected"), histAsset("p2", "spare", "connected")},
		{histAsset("p2", "spare", "connected")}, // p1 gone
		{histAsset("p1", "gw-reborn", "connected"), histAsset("p2", "spare", "connected")},
	}
	ids := make([]int64, 0, len(snapshots))
	for _, assets := range snapshots {
		ids = append(ids, seedAudit(t, db, []string{"netbird"}, assets))
	}
	// Backdate so the series is strictly ordered in time: newest last.
	for i, id := range ids {
		backdateAuditRow(t, db, id, int64((len(ids)-i)*3600))
	}
	return ids
}

func TestHistory_TableReportsTheWholeLife(t *testing.T) {
	db := tempDB(t)
	ids := seedHistory(t, db)

	out, err := runHistory(t, db, "p1")
	if err != nil {
		t.Fatalf("history p1: %v", err)
	}
	for _, want := range []string{
		"netbird/netbird.peer p1 (gw-reborn)", // named by what it is called NOW
		"first seen",
		"in newest    yes",
		"observed in  3 of 4 snapshot(s) covering netbird",
		"appeared",
		"changed",
		`status: "connected" -> "disconnected"`,
		"disappeared (last seen in #" + itoa64(ids[1]) + ")",
		"reappeared",
		`name: "gw" -> "gw-reborn"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("history output missing %q:\n%s", want, out)
		}
	}
}

func TestHistory_JSON(t *testing.T) {
	db := tempDB(t)
	ids := seedHistory(t, db)

	out, err := runHistory(t, db, "p1", "-o", "json")
	if err != nil {
		t.Fatalf("history -o json: %v", err)
	}
	var report historyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("history JSON is not parseable (%v):\n%s", err, out)
	}
	if report.Matched != 1 || report.Truncated || report.Snapshots != 4 {
		t.Fatalf("report = %+v, want 1 match across 4 snapshots, untruncated", report)
	}
	tl := report.Timelines[0]
	if tl.FirstSeen.AuditID != ids[0] || tl.LastSeen.AuditID != ids[3] {
		t.Errorf("first/last seen = #%d/#%d, want #%d/#%d",
			tl.FirstSeen.AuditID, tl.LastSeen.AuditID, ids[0], ids[3])
	}
	if !tl.InNewest || tl.Newest.AuditID != ids[3] {
		t.Errorf("InNewest=%v newest=#%d, want true / #%d", tl.InNewest, tl.Newest.AuditID, ids[3])
	}
	if tl.Observed != 3 || tl.Candidates != 4 {
		t.Errorf("observed %d of %d, want 3 of 4", tl.Observed, tl.Candidates)
	}

	kinds := make([]string, len(tl.Events))
	for i, e := range tl.Events {
		kinds[i] = e.Kind
	}
	want := []string{eventAppeared, eventChanged, eventDisappeared, eventReappeared}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Errorf("event kinds = %v, want %v", kinds, want)
	}
	// The change event names both endpoints so "when did this happen" is
	// answerable from the report alone.
	changed := tl.Events[1]
	if changed.From == nil || changed.From.AuditID != ids[0] || changed.At.AuditID != ids[1] {
		t.Errorf("changed event = %+v, want from #%d to #%d", changed, ids[0], ids[1])
	}
	if len(changed.Fields) != 1 || changed.Fields[0].Field != "status" {
		t.Errorf("changed fields = %+v, want one status change", changed.Fields)
	}
}

// An asset that is gone must say so, and say which snapshot it vanished in.
func TestHistory_AbsentFromNewest(t *testing.T) {
	db := tempDB(t)
	first := seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
	last := seedAudit(t, db, []string{"netbird"}, nil)
	backdateAuditRow(t, db, first, 7200)
	backdateAuditRow(t, db, last, 3600)

	out, err := runHistory(t, db, "p1", "-o", "json")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var report historyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	tl := report.Timelines[0]
	if tl.InNewest {
		t.Error("InNewest is true for an asset absent from the newest snapshot")
	}
	if n := len(tl.Events); n != 2 || tl.Events[1].Kind != eventDisappeared || tl.Events[1].At.AuditID != last {
		t.Errorf("events = %+v, want appeared then disappeared at #%d", tl.Events, last)
	}
}

// A snapshot that never ran the asset's provider is not evidence about it.
// Reading a narrow audit as "everything else was deleted" is the single most
// wrong thing a timeline could say.
func TestHistory_NarrowSnapshotsAreNotEvidenceOfDeletion(t *testing.T) {
	db := tempDB(t)
	full := []string{"netbird", "cloudflare"}
	firstFull := seedAudit(t, db, full, []core.Asset{
		histAsset("p1", "gw", "connected"),
		{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1", Name: "example.com"},
	})
	// A later netbird-only run: it contains no zones, but that says nothing
	// about whether z1 still exists.
	narrow := seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
	backdateAuditRow(t, db, firstFull, 7200)
	backdateAuditRow(t, db, narrow, 3600)

	out, err := runHistory(t, db, "z1", "-o", "json")
	if err != nil {
		t.Fatalf("history z1: %v", err)
	}
	var report historyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	tl := report.Timelines[0]
	if tl.Candidates != 1 {
		t.Errorf("candidate snapshots = %d, want 1 (only the run that included cloudflare)", tl.Candidates)
	}
	if !tl.InNewest || tl.Newest.AuditID != firstFull {
		t.Errorf("newest covering snapshot = #%d (in=%v), want #%d / true", tl.Newest.AuditID, tl.InNewest, firstFull)
	}
	for _, e := range tl.Events {
		if e.Kind == eventDisappeared {
			t.Errorf("a narrow audit was read as a deletion: %+v", tl.Events)
		}
	}
	// The netbird peer, which the narrow run DID cover, sees both snapshots.
	out, err = runHistory(t, db, "p1", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Timelines[0].Candidates != 2 {
		t.Errorf("peer candidate snapshots = %d, want 2", report.Timelines[0].Candidates)
	}
}

func TestHistory_SelectorIsAGlobOverIDAndName(t *testing.T) {
	db := tempDB(t)
	seedAudit(t, db, []string{"netbird"}, []core.Asset{
		histAsset("p1", "gw-prod", "connected"),
		histAsset("p2", "gw-dev", "connected"),
	})

	for _, selector := range []string{"p1", "P1", "gw-prod", "*prod", "p*"} {
		out, err := runHistory(t, db, selector, "-o", "json")
		if err != nil {
			t.Fatalf("history %q: %v", selector, err)
		}
		var report historyReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Timelines) == 0 {
			t.Errorf("selector %q matched nothing", selector)
		}
	}
}

func TestHistory_TruncationIsReported(t *testing.T) {
	db := tempDB(t)
	assets := make([]core.Asset, 0, 5)
	for _, id := range []string{"p1", "p2", "p3", "p4", "p5"} {
		assets = append(assets, histAsset(id, "peer", "connected"))
	}
	seedAudit(t, db, []string{"netbird"}, assets)

	out, err := runHistory(t, db, "p*", "--limit", "2")
	if err != nil {
		t.Fatalf("history --limit 2: %v", err)
	}
	if !strings.Contains(out, "5 asset(s) matching") || !strings.Contains(out, "showing the first 2") {
		t.Errorf("a capped report must say it capped:\n%s", out)
	}
}

// An empty report would read as "this asset has no history", a very
// different claim from "nothing matched your selector".
func TestHistory_Errors(t *testing.T) {
	empty := tempDB(t)
	if _, err := runHistory(t, empty, "p1"); err == nil ||
		!strings.Contains(err.Error(), "no stored snapshots") || !strings.Contains(err.Error(), empty) {
		t.Errorf("history on an empty store err = %v, want one naming the database", err)
	}

	db := tempDB(t)
	seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
	_, err := runHistory(t, db, "nope")
	if err == nil || !strings.Contains(err.Error(), "matched no asset") || !strings.Contains(err.Error(), `"*nope*"`) {
		t.Errorf("unmatched selector err = %v, want a widen-your-glob hint", err)
	}
	if _, err := runHistory(t, db, "p1", "-o", "yaml"); err == nil {
		t.Error("history -o yaml succeeded, want unknown-format error")
	}
}

// timelineFor is the pure core; exercising it directly pins the ordering
// contract its callers rely on (observations oldest-first, audits newest-
// first) without a store round trip.
func TestTimelineFor_HandlesASingleObservation(t *testing.T) {
	audits := []store.AuditMeta{
		{ID: 2, Providers: []string{"netbird"}},
		{ID: 1, Providers: []string{"netbird"}},
	}
	obs := []store.Observation{{AuditID: 1, Asset: histAsset("p1", "gw", "connected")}}

	tl := timelineFor(obs, audits)
	if tl.Observed != 1 || tl.Candidates != 2 || tl.InNewest {
		t.Fatalf("timeline = %+v, want 1 of 2 observations and InNewest=false", tl)
	}
	if len(tl.Events) != 2 || tl.Events[0].Kind != eventAppeared || tl.Events[1].Kind != eventDisappeared {
		t.Errorf("events = %+v, want appeared then disappeared", tl.Events)
	}
}

func TestCoversProvider(t *testing.T) {
	a := store.AuditMeta{Providers: []string{"netbird", "cloudflare"}}
	if !coversProvider(a, "NetBird") {
		t.Error("provider matching must be case-insensitive")
	}
	if coversProvider(a, "oci") {
		t.Error("oci is not in the provider set")
	}
	if coversProvider(store.AuditMeta{}, "netbird") {
		t.Error("an audit with no recorded provider set covers nothing")
	}
}

func TestHumanAge(t *testing.T) {
	cases := map[string]string{
		"45s":    "45s",
		"90m":    "1h30m",
		"720h":   "30d",
		"36h":    "36h0m",
		"755h":   "31d11h",
		"30m":    "30m",
		"1h0m0s": "1h0m",
	}
	for input, want := range cases {
		d, err := time.ParseDuration(input)
		if err != nil {
			t.Fatal(err)
		}
		if got := humanAge(d); got != want {
			t.Errorf("humanAge(%s) = %q, want %q", input, got, want)
		}
	}
}

// buildHistory needs a live store; this keeps the ctx plumbing honest.
func TestBuildHistory_UsesTheCommandContext(t *testing.T) {
	db := tempDB(t)
	seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := buildHistory(ctx, st, "p1", 0); err == nil {
		t.Error("buildHistory ignored a cancelled context")
	}
}
