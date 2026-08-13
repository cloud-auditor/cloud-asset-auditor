package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// runDiff executes `auditor diff <args...>` against the given DB and returns
// the report. The renderers write through openOutput (os.Stdout by default,
// not cobra's out writer), so the report is captured with --output-file.
// A fresh command tree per call keeps flag state from leaking between tests.
func runDiff(t *testing.T, dbPath string, args ...string) (string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "report.txt")

	s := &cliState{v: viper.New()}
	s.v.Set("db", dbPath)
	cmd := newDiffCmd(s)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(append([]string{"--output-file", out}, args...))
	err := cmd.Execute()

	// Missing file = the command failed before rendering, which is itself
	// the assertion in the "refuses to report" tests.
	report, readErr := os.ReadFile(out)
	if readErr != nil {
		return buf.String(), err
	}
	return buf.String() + string(report), err
}

// writeSnapshot writes assets as an `audit -o json` array and returns the path.
func writeSnapshot(t *testing.T, name string, assets []core.Asset) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	data, err := json.Marshal(assets)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func histAsset(id, name, status string) core.Asset {
	return core.Asset{Provider: "netbird", Type: "netbird.peer", ID: id, Name: name, Status: status}
}

// ----------------------------------------------------------------------
// the two-file form, unchanged
// ----------------------------------------------------------------------

func TestDiff_TwoFilesStillWorks(t *testing.T) {
	before := writeSnapshot(t, "before.json", []core.Asset{
		histAsset("p1", "gw", "connected"), histAsset("p2", "old", "connected"),
	})
	after := writeSnapshot(t, "after.json", []core.Asset{
		histAsset("p1", "gw", "disconnected"), histAsset("p3", "new", "connected"),
	})

	out, err := runDiff(t, tempDB(t), before, after)
	if err != nil {
		t.Fatalf("diff two files: %v", err)
	}
	for _, want := range []string{"1 added, 1 removed, 1 changed", "+ netbird/netbird.peer p3", "- netbird/netbird.peer p2", `status: "connected" -> "disconnected"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The stored-snapshot mode must leave no trace on the file form.
	if strings.Contains(out, "Baseline:") {
		t.Errorf("two-file diff emitted a --since provenance header:\n%s", out)
	}
}

func TestDiff_TwoFilesExitCode(t *testing.T) {
	same := []core.Asset{histAsset("p1", "gw", "connected")}
	a, b := writeSnapshot(t, "a.json", same), writeSnapshot(t, "b.json", same)
	if _, err := runDiff(t, tempDB(t), "--exit-code", a, b); err != nil {
		t.Errorf("clean diff --exit-code err = %v, want nil", err)
	}

	c := writeSnapshot(t, "c.json", []core.Asset{histAsset("p1", "gw", "down")})
	if _, err := runDiff(t, tempDB(t), "--exit-code", a, c); !errors.Is(err, ErrDrift) {
		t.Errorf("drifted diff --exit-code err = %v, want ErrDrift", err)
	}
}

// ----------------------------------------------------------------------
// mode selection
// ----------------------------------------------------------------------

// The two ways to name a pair must never be silently mixed: whichever one
// the command guessed at would produce a confident, unrelated report.
func TestValidateDiffMode(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		since      string
		against    string
		againstSet bool
		providers  []string
		wantErr    string
	}{
		{"two files", []string{"a", "b"}, "", diffAgainstNewest, false, nil, ""},
		{"since alone", nil, "30d", diffAgainstNewest, false, nil, ""},
		{"since with live", nil, "30d", diffAgainstLive, true, nil, ""},
		{"since with providers", nil, "30d", diffAgainstNewest, false, []string{"netbird"}, ""},
		{"since plus files", []string{"a", "b"}, "30d", diffAgainstNewest, false, nil, "not both"},
		{"since plus one file", []string{"a"}, "30d", diffAgainstNewest, false, nil, "not both"},
		{"one file alone", []string{"a"}, "", diffAgainstNewest, false, nil, "two snapshot files"},
		{"no args at all", nil, "", diffAgainstNewest, false, nil, "two snapshot files"},
		{"against without since", []string{"a", "b"}, "", diffAgainstLive, true, nil, "only applies to --since"},
		// --providers picks a stored series; two files already name themselves,
		// so accepting it there would silently do nothing.
		{"providers without since", []string{"a", "b"}, "", diffAgainstNewest, false, []string{"oci"}, "only applies to --since"},
		{"bogus against", nil, "30d", "yesterday", true, nil, `unknown --against "yesterday"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDiffMode(tc.args, tc.since, tc.against, tc.againstSet, tc.providers)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("err = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		spec string
		want time.Time
	}{
		{"720h", now.Add(-720 * time.Hour)},
		{"90m", now.Add(-90 * time.Minute)},
		{"30d", now.AddDate(0, 0, -30)},
		{"1d", now.AddDate(0, 0, -1)},
		{"2026-06-01T09:30:00Z", time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC)},
		{" 30d ", now.AddDate(0, 0, -30)}, // surrounding whitespace is not an error
	}
	for _, tc := range cases {
		got, err := parseSince(tc.spec, now)
		if err != nil {
			t.Errorf("parseSince(%q): %v", tc.spec, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("parseSince(%q) = %v, want %v", tc.spec, got, tc.want)
		}
	}

	// A bare date is local midnight — the START of that day, so --since
	// 2026-06-01 excludes everything from June 1st onwards.
	got, err := parseSince("2026-06-01", now)
	if err != nil {
		t.Fatalf("parseSince(date): %v", err)
	}
	if want := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local); !got.Equal(want) {
		t.Errorf("parseSince(date) = %v, want %v", got, want)
	}

	for _, bad := range []string{"", "yesterday", "30 days", "0h", "-4h", "2026-13-01", "d"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("parseSince(%q) succeeded, want an error", bad)
		}
	}
}

// ----------------------------------------------------------------------
// --since against the stored snapshots
// ----------------------------------------------------------------------

func TestDiffSince_ComparesNewestAtOrBeforeAgainstNewest(t *testing.T) {
	db := tempDB(t)
	ancient := seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
	baseline := seedAudit(t, db, []string{"netbird"}, []core.Asset{
		histAsset("p1", "gw", "connected"), histAsset("p2", "old", "connected"),
	})
	newest := seedAudit(t, db, []string{"netbird"}, []core.Asset{
		histAsset("p1", "gw", "disconnected"), histAsset("p3", "new", "connected"),
	})
	backdateAuditRow(t, db, ancient, 90*24*3600)
	backdateAuditRow(t, db, baseline, 30*24*3600)

	out, err := runDiff(t, db, "--since", "20d")
	if err != nil {
		t.Fatalf("diff --since 20d: %v", err)
	}
	// The 30-day-old snapshot is the newest at or before 20 days ago; the
	// 90-day-old one must not win just by also being older.
	if !strings.Contains(out, "Baseline: snapshot #"+itoa64(baseline)) {
		t.Errorf("wrong baseline chosen (want #%d, not #%d):\n%s", baseline, ancient, out)
	}
	if !strings.Contains(out, "Current:  snapshot #"+itoa64(newest)) {
		t.Errorf("output does not name the current snapshot #%d:\n%s", newest, out)
	}
	for _, want := range []string{"1 added, 1 removed, 1 changed", `status: "connected" -> "disconnected"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// The command must say how far back the history goes rather than quietly
// comparing against a snapshot that is not old enough.
func TestDiffSince_NoBaselineNamesTheOldestSnapshot(t *testing.T) {
	db := tempDB(t)
	oldest := seedAudit(t, db, []string{"netbird", "cloudflare"}, []core.Asset{histAsset("p1", "gw", "connected")})
	seedAudit(t, db, []string{"netbird", "cloudflare"}, []core.Asset{histAsset("p1", "gw", "connected")})
	backdateAuditRow(t, db, oldest, 10*24*3600)

	_, err := runDiff(t, db, "--since", "365d")
	if err == nil {
		t.Fatal("diff --since 365d succeeded; want an error naming the oldest snapshot")
	}
	msg := err.Error()
	for _, want := range []string{
		"no stored snapshot at or before",
		"the oldest one is #" + itoa64(oldest),
		"cloudflare,netbird",
		"auditor cache show " + itoa64(oldest),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %s", want, msg)
		}
	}
}

func TestDiffSince_EmptyStoreNamesTheDatabase(t *testing.T) {
	db := tempDB(t)
	_, err := runDiff(t, db, "--since", "30d")
	if err == nil {
		t.Fatal("diff --since on an empty store succeeded")
	}
	if !strings.Contains(err.Error(), "no stored snapshots") || !strings.Contains(err.Error(), db) {
		t.Errorf("error should name the database that was searched: %v", err)
	}
}

// Comparing the baseline against itself would print "No drift" — a clean
// bill of health backed by nothing.
func TestDiffSince_RefusesToCompareASnapshotWithItself(t *testing.T) {
	db := tempDB(t)
	only := seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
	backdateAuditRow(t, db, only, 60*24*3600)

	out, err := runDiff(t, db, "--since", "30d")
	if err == nil {
		t.Fatalf("diff succeeded comparing #%d against itself:\n%s", only, out)
	}
	for _, want := range []string{"also the newest one stored", "--against live"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if strings.Contains(out, "No drift") {
		t.Errorf("a self-comparison reported no drift:\n%s", out)
	}
}

// A narrow one-off audit must not become the "current" side of a full
// audit's baseline: every provider missing from it would read as removed.
func TestDiffSince_StaysWithinOneProviderSet(t *testing.T) {
	db := tempDB(t)
	full := []string{"netbird", "cloudflare"}
	oldFull := seedAudit(t, db, full, []core.Asset{
		histAsset("p1", "gw", "connected"),
		{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1", Name: "example.com"},
	})
	newFull := seedAudit(t, db, full, []core.Asset{
		histAsset("p1", "gw", "connected"),
		{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1", Name: "example.com"},
	})
	// Newest snapshot overall, but a different provider set.
	narrow := seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
	backdateAuditRow(t, db, oldFull, 30*24*3600)
	backdateAuditRow(t, db, newFull, 2*3600)
	backdateAuditRow(t, db, narrow, 3600)

	out, err := runDiff(t, db, "--since", "20d")
	if err != nil {
		t.Fatalf("diff --since 20d: %v", err)
	}
	if !strings.Contains(out, "Current:  snapshot #"+itoa64(newFull)) {
		t.Errorf("current side should be the full-set snapshot #%d, not the narrow #%d:\n%s", newFull, narrow, out)
	}
	if !strings.Contains(out, "No drift") {
		t.Errorf("the two full-set snapshots are identical; want no drift:\n%s", out)
	}
}

// seedTwoSeries builds a store holding two independent histories: a big
// "full estate" series whose newest snapshot is comparatively old, and a small
// single-provider series that is more recent. Asking for a short window then
// lands the baseline in the small series, which is the setup that used to
// answer an estate-wide question with a single provider's data.
func seedTwoSeries(t *testing.T) (dbPath string, full []string) {
	t.Helper()
	db := tempDB(t)
	full = []string{"netbird", "cloudflare"}
	fullAssets := []core.Asset{
		histAsset("p1", "gw", "connected"),
		{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1", Name: "example.com"},
	}
	oldFull := seedAudit(t, db, full, fullAssets)
	newFull := seedAudit(t, db, full, fullAssets)
	narrowOld := seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
	narrowNew := seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})

	backdateAuditRow(t, db, oldFull, 60*24*3600)
	backdateAuditRow(t, db, newFull, 30*24*3600)
	backdateAuditRow(t, db, narrowOld, 5*24*3600)
	backdateAuditRow(t, db, narrowNew, 2*24*3600)
	return db, full
}

// The baseline is picked by timestamp across every series in the store, and it
// then scopes both sides of the diff. When it lands in a series that covers
// only part of the estate the comparison is internally consistent but answers
// a far narrower question than the one asked — and it can answer it with
// "No drift", a clean bill of health over a fraction of the assets. The scope
// must be stated, and the other series named.
func TestDiffSince_NamesTheSeriesItDidNotExamine(t *testing.T) {
	db, full := seedTwoSeries(t)

	// 3 days back lands on the 5-day-old netbird snapshot, leaving the 2-day-old
	// one as a valid current side — a complete, self-consistent comparison that
	// nonetheless covers half the estate.
	out, err := runDiff(t, db, "--since", "3d")
	if err != nil {
		t.Fatalf("diff --since 3d: %v", err)
	}
	if !strings.Contains(out, "No drift") {
		t.Fatalf("precondition: the netbird series is unchanged, so this reports no drift:\n%s", out)
	}
	// The store canonicalizes a provider set (lower-cased, deduped, sorted), so
	// the unexamined series prints in that order rather than as `full` spells it.
	canonical := append([]string(nil), full...)
	sort.Strings(canonical)
	for _, want := range []string{
		`compares the "netbird" series only`,
		"says nothing about",
		strings.Join(canonical, ","), // the unexamined series, named
		"--providers",                // and the lever to select it
	} {
		if !strings.Contains(out, want) {
			t.Errorf("a scoped answer must disclose %q; got:\n%s", want, out)
		}
	}
}

// The warning is only honest if the lever it advertises actually works.
func TestDiffSince_ProvidersPinsTheSeries(t *testing.T) {
	db, full := seedTwoSeries(t)

	// Same 3-day window, now pinned to the full estate: its newest snapshot is
	// 30 days old, so there is no pair to compare and the command must refuse
	// rather than fall back to the netbird series.
	out, err := runDiff(t, db, "--since", "3d", "--providers", strings.Join(full, ","))
	if err == nil {
		t.Fatalf("want a refusal: the pinned series has no snapshot newer than the baseline; got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "netbird,cloudflare") && !strings.Contains(err.Error(), "cloudflare,netbird") {
		t.Errorf("the refusal must name the pinned set: %v", err)
	}

	// A window that does contain a comparable pair in the pinned series works.
	out, err = runDiff(t, db, "--since", "45d", "--providers", strings.Join(full, ","))
	if err != nil {
		t.Fatalf("diff --since 45d --providers %v: %v", full, err)
	}
	// Pinned explicitly, so the "which series did I pick" note is redundant.
	if strings.Contains(out, "series only") {
		t.Errorf("an explicitly pinned series must not be warned about:\n%s", out)
	}
}

// One series in the store is the common case and must stay quiet, or the
// warning becomes noise that gets tuned out before it is ever needed.
func TestDiffSince_NoSeriesWarningWhenOnlyOneExists(t *testing.T) {
	db := tempDB(t)
	old := seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
	seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "disconnected")})
	backdateAuditRow(t, db, old, 30*24*3600)

	out, err := runDiff(t, db, "--since", "20d")
	if err != nil {
		t.Fatalf("diff --since 20d: %v", err)
	}
	if strings.Contains(out, "series only") {
		t.Errorf("a single-series store has nothing to disclose:\n%s", out)
	}
}

// A --providers set that matches no stored series must say which sets do
// exist: the alternative is guessing at an opaque store.
func TestDiffSince_UnknownProviderSetListsTheKnownOnes(t *testing.T) {
	db, _ := seedTwoSeries(t)

	out, err := runDiff(t, db, "--since", "20d", "--providers", "oci")
	if err == nil {
		t.Fatalf("want an error for a series that was never stored; got:\n%s", out)
	}
	for _, want := range []string{"no stored snapshots for providers oci", "It holds snapshots for:", "netbird"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// JSON output must stay a machine document: the provenance header belongs to
// the human formats only.
func TestDiffSince_JSONStaysParseable(t *testing.T) {
	db := tempDB(t)
	baseline := seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
	seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "down")})
	backdateAuditRow(t, db, baseline, 30*24*3600)

	out, err := runDiff(t, db, "--since", "20d", "-o", "json")
	if err != nil {
		t.Fatalf("diff --since -o json: %v", err)
	}
	var report struct {
		Changed []struct {
			Fields []struct{ Field, Old, New string }
		}
		Summary struct {
			Changed  int `json:"changed"`
			OldTotal int `json:"old_total"`
			NewTotal int `json:"new_total"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("JSON output is not parseable (%v):\n%s", err, out)
	}
	if report.Summary.Changed != 1 || report.Summary.OldTotal != 1 || report.Summary.NewTotal != 1 {
		t.Errorf("summary = %+v, want 1 changed of 1/1", report.Summary)
	}
}

func TestDiffSince_ExitCodeGatesOnDrift(t *testing.T) {
	db := tempDB(t)
	baseline := seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "connected")})
	seedAudit(t, db, []string{"netbird"}, []core.Asset{histAsset("p1", "gw", "down")})
	backdateAuditRow(t, db, baseline, 30*24*3600)

	if _, err := runDiff(t, db, "--since", "20d", "--exit-code"); !errors.Is(err, ErrDrift) {
		t.Errorf("err = %v, want ErrDrift", err)
	}
}

func TestDiffSince_UnknownFormatFailsBeforeTouchingTheStore(t *testing.T) {
	// No DB is ever created: an unknown -o must cost a millisecond, not a
	// ten-minute live audit.
	if _, err := runDiff(t, filepath.Join(t.TempDir(), "never-created.db"), "--since", "30d", "-o", "yaml"); err == nil {
		t.Error("diff -o yaml succeeded, want unknown-format error")
	}
}

// ----------------------------------------------------------------------
// --against live
// ----------------------------------------------------------------------

type liveFakeProvider struct {
	name   string
	assets []core.Asset
	fail   bool
}

func (f liveFakeProvider) Name() string                   { return f.name }
func (f liveFakeProvider) Validate(context.Context) error { return nil }
func (f liveFakeProvider) Collect(context.Context) (<-chan core.Asset, <-chan error) {
	assets := make(chan core.Asset, len(f.assets))
	errs := make(chan error, 1)
	for _, a := range f.assets {
		assets <- a
	}
	if f.fail {
		errs <- errors.New("region eu-west timed out")
	}
	close(assets)
	close(errs)
	return assets, errs
}

func init() {
	core.Register("difflive", func() (core.Provider, error) {
		return liveFakeProvider{name: "difflive", assets: []core.Asset{
			{Provider: "difflive", Type: "t", ID: "p1", Name: "gw", Status: "down"},
			{Provider: "difflive", Type: "t", ID: "p9", Name: "brand-new", Status: "up"},
		}}, nil
	})
	core.Register("difflivebroken", func() (core.Provider, error) {
		return liveFakeProvider{name: "difflivebroken", fail: true}, nil
	})
}

func TestDiffSince_AgainstLive(t *testing.T) {
	db := tempDB(t)
	baseline := seedAudit(t, db, []string{"difflive"}, []core.Asset{
		{Provider: "difflive", Type: "t", ID: "p1", Name: "gw", Status: "up"},
	})
	backdateAuditRow(t, db, baseline, 30*24*3600)

	out, err := runDiff(t, db, "--since", "20d", "--against", "live")
	if err != nil {
		t.Fatalf("diff --against live: %v", err)
	}
	if !strings.Contains(out, "Current:  live audit of difflive (2 assets)") {
		t.Errorf("output does not describe the live side:\n%s", out)
	}
	for _, want := range []string{"1 added, 0 removed, 1 changed", `status: "up" -> "down"`, "+ difflive/t p9"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// A provider that failed halfway would make every one of its assets read as
// removed. Refusing is the same rule `audit --cache` uses before persisting.
func TestDiffSince_AgainstLiveRefusesAPartialCollection(t *testing.T) {
	db := tempDB(t)
	baseline := seedAudit(t, db, []string{"difflivebroken"}, []core.Asset{
		{Provider: "difflivebroken", Type: "t", ID: "p1", Name: "gw"},
	})
	backdateAuditRow(t, db, baseline, 30*24*3600)

	out, err := runDiff(t, db, "--since", "20d", "--against", "live")
	if !errors.Is(err, ErrPartial) {
		t.Fatalf("err = %v, want ErrPartial", err)
	}
	if !strings.Contains(err.Error(), "would read as removed") {
		t.Errorf("error should explain why it refused: %v", err)
	}
	// No report at all — not an empty one, not a partial one.
	if strings.Contains(out, "added,") || strings.Contains(out, "No drift") {
		t.Errorf("a drift report was rendered from an incomplete audit:\n%s", out)
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// A provider that never started is silence, not consent: its whole inventory
// would read as removed, and selectProviders only logs a warning.
func TestDiffSince_AgainstLiveRefusesAMissingProvider(t *testing.T) {
	db := tempDB(t)
	baseline := seedAudit(t, db, []string{"difflive", "gone-from-this-build"}, []core.Asset{
		{Provider: "difflive", Type: "t", ID: "p1", Name: "gw"},
	})
	backdateAuditRow(t, db, baseline, 30*24*3600)

	out, err := runDiff(t, db, "--since", "20d", "--against", "live")
	if err == nil {
		t.Fatalf("diff succeeded with a provider that could not start:\n%s", out)
	}
	for _, want := range []string{"gone-from-this-build", "reported as removed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if strings.Contains(out, "added,") || strings.Contains(out, "No drift") {
		t.Errorf("a drift report was rendered anyway:\n%s", out)
	}
}

func TestMissingProviders(t *testing.T) {
	got := []core.Provider{liveFakeProvider{name: "netbird"}, liveFakeProvider{name: "OCI"}}
	if m := missingProviders([]string{"netbird", "oci"}, got); len(m) != 0 {
		t.Errorf("missingProviders = %v, want none (matching is case-insensitive)", m)
	}
	if m := missingProviders([]string{"netbird", "cloudflare", "gcp"}, got); strings.Join(m, ",") != "cloudflare,gcp" {
		t.Errorf("missingProviders = %v, want [cloudflare gcp]", m)
	}
}
