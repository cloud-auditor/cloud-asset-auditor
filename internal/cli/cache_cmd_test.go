package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/store"
)

// runCache executes `auditor cache <args...>` against the given DB and
// captures its output. A fresh command tree per call keeps flag state from
// leaking between tests.
func runCache(t *testing.T, dbPath string, args ...string) (string, error) {
	t.Helper()
	s := &cliState{v: viper.New()}
	s.v.Set("db", dbPath)
	cmd := newCacheCmd(s)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func seedAudit(t *testing.T, dbPath string, providers []string, assets []core.Asset) int64 {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.SaveAudit(context.Background(), providers, assets)
	if err != nil {
		t.Fatalf("SaveAudit: %v", err)
	}
	return id
}

func tempDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "cache.db")
}

var cacheTestAssets = []core.Asset{
	{Provider: "netbird", AccountID: "a1", Type: "netbird.peer", ID: "p1", Name: "gw",
		Status: "connected", Tags: map[string]string{"ip": "100.64.0.1"}},
	{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1", Name: "example.com"},
}

func TestCacheList_Table(t *testing.T) {
	db := tempDB(t)
	id := seedAudit(t, db, []string{"netbird", "cloudflare"}, cacheTestAssets)

	out, err := runCache(t, db, "list")
	if err != nil {
		t.Fatalf("cache list: %v", err)
	}
	for _, want := range []string{"ID", "WHEN", "AGE", "PROVIDERS", "ASSETS",
		strconv.FormatInt(id, 10), "cloudflare,netbird", "2"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestCacheList_JSON(t *testing.T) {
	db := tempDB(t)
	id := seedAudit(t, db, []string{"netbird", "cloudflare"}, cacheTestAssets)

	out, err := runCache(t, db, "list", "-o", "json")
	if err != nil {
		t.Fatalf("cache list -o json: %v", err)
	}
	var audits []store.AuditMeta
	if err := json.Unmarshal([]byte(out), &audits); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	if len(audits) != 1 || audits[0].ID != id || audits[0].AssetCount != 2 {
		t.Fatalf("audits = %+v, want one entry with id=%d asset_count=2", audits, id)
	}
	for _, key := range []string{`"id"`, `"run_at"`, `"providers"`, `"asset_count"`} {
		if !strings.Contains(out, key) {
			t.Errorf("JSON output missing key %s:\n%s", key, out)
		}
	}
}

func TestCacheList_Empty(t *testing.T) {
	out, err := runCache(t, tempDB(t), "list")
	if err != nil {
		t.Fatalf("cache list: %v", err)
	}
	if !strings.Contains(out, "No cached audits.") {
		t.Errorf("empty list output = %q, want the no-audits notice", out)
	}
}

func TestCacheList_UnknownFormat(t *testing.T) {
	if _, err := runCache(t, tempDB(t), "list", "-o", "yaml"); err == nil {
		t.Error("cache list -o yaml succeeded, want unknown-format error")
	}
}

func TestCacheShow_JSONArrayRoundTrip(t *testing.T) {
	db := tempDB(t)
	id := seedAudit(t, db, []string{"netbird", "cloudflare"}, cacheTestAssets)
	outFile := filepath.Join(t.TempDir(), "snap.json")

	if _, err := runCache(t, db, "show", strconv.FormatInt(id, 10), "--output-file", outFile); err != nil {
		t.Fatalf("cache show: %v", err)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	var got []core.Asset
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("show output is not a JSON array: %v\n%s", err, data)
	}
	if len(got) != 2 {
		t.Fatalf("got %d assets, want 2", len(got))
	}
	byID := map[string]core.Asset{}
	for _, a := range got {
		byID[a.ID] = a
	}
	if p := byID["p1"]; p.Provider != "netbird" || p.Name != "gw" || p.Tags["ip"] != "100.64.0.1" {
		t.Errorf("asset p1 round-trip mismatch: %+v", p)
	}
}

func TestCacheShow_Stream(t *testing.T) {
	db := tempDB(t)
	id := seedAudit(t, db, []string{"netbird", "cloudflare"}, cacheTestAssets)
	outFile := filepath.Join(t.TempDir(), "snap.ndjson")

	if _, err := runCache(t, db, "show", strconv.FormatInt(id, 10), "--stream", "--output-file", outFile); err != nil {
		t.Fatalf("cache show --stream: %v", err)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("NDJSON output has %d lines, want 2:\n%s", len(lines), data)
	}
	for _, line := range lines {
		var a core.Asset
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Errorf("line %q is not a JSON asset: %v", line, err)
		}
	}
}

func TestCacheShow_Errors(t *testing.T) {
	db := tempDB(t)
	if _, err := runCache(t, db, "show", "999"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("show 999 err = %v, want not-found", err)
	}
	if _, err := runCache(t, db, "show", "abc"); err == nil || !strings.Contains(err.Error(), "invalid audit id") {
		t.Errorf("show abc err = %v, want invalid-id", err)
	}
}

func TestCacheRm(t *testing.T) {
	db := tempDB(t)
	id := seedAudit(t, db, []string{"netbird"}, cacheTestAssets[:1])
	arg := strconv.FormatInt(id, 10)

	out, err := runCache(t, db, "rm", arg)
	if err != nil {
		t.Fatalf("cache rm: %v", err)
	}
	if want := "Deleted audit " + arg + "."; !strings.Contains(out, want) {
		t.Errorf("rm output = %q, want %q", out, want)
	}
	if _, err := runCache(t, db, "rm", arg); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("second rm err = %v, want not-found", err)
	}
}

func TestCachePrune(t *testing.T) {
	db := tempDB(t)
	old := seedAudit(t, db, []string{"netbird"}, cacheTestAssets[:1])
	recent := seedAudit(t, db, []string{"netbird"}, cacheTestAssets)
	backdateAuditRow(t, db, old, 48*3600)

	out, err := runCache(t, db, "prune", "--max-age", "24h")
	if err != nil {
		t.Fatalf("cache prune: %v", err)
	}
	if !strings.Contains(out, "Pruned 1 audit snapshot(s).") {
		t.Errorf("prune output = %q", out)
	}
	list, err := runCache(t, db, "list", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var audits []store.AuditMeta
	if err := json.Unmarshal([]byte(list), &audits); err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].ID != recent {
		t.Errorf("after prune, audits = %+v, want only %d", audits, recent)
	}

	if _, err := runCache(t, db, "prune", "--max-age", "0s"); err == nil {
		t.Error("prune --max-age 0s succeeded, want positive-duration error")
	}
	if _, err := runCache(t, db, "prune"); err == nil {
		t.Error("prune without --max-age succeeded, want required-flag error")
	}
}

func TestCacheClear(t *testing.T) {
	db := tempDB(t)
	seedAudit(t, db, []string{"netbird"}, cacheTestAssets[:1])
	seedAudit(t, db, []string{"oci"}, nil)

	out, err := runCache(t, db, "clear")
	if err != nil {
		t.Fatalf("cache clear: %v", err)
	}
	if !strings.Contains(out, "Cleared 2 audit snapshot(s).") {
		t.Errorf("clear output = %q", out)
	}
	list, err := runCache(t, db, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "No cached audits.") {
		t.Errorf("after clear, list = %q", list)
	}
}

// backdateAuditRow shifts an audit's run_at into the past through a direct
// connection — the store intentionally exposes no way to rewrite run_at.
func backdateAuditRow(t *testing.T, dbPath string, id int64, seconds int64) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE audits SET run_at = run_at - ? WHERE id=?`, seconds, id); err != nil {
		t.Fatalf("backdate audit %d: %v", id, err)
	}
}
