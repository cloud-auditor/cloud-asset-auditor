package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/store"
)

func openTemp(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestAuditCache_Roundtrip(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	assets := []core.Asset{
		{
			Provider: "netbird", AccountID: "a1", Type: "netbird.peer", ID: "p1", Name: "gw",
			Status: "connected", Tags: map[string]string{"ip": "100.64.0.1"}, Raw: json.RawMessage(`{"x":1}`),
		},
		{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1", Name: "example.com"},
	}
	id, err := st.SaveAudit(ctx, []string{"netbird", "cloudflare"}, assets)
	if err != nil || id == 0 {
		t.Fatalf("SaveAudit: id=%d err=%v", id, err)
	}

	// Provider order must not matter (canonicalized key).
	got, runAt, fresh, err := st.LatestAudit(ctx, []string{"cloudflare", "netbird"}, time.Hour)
	if err != nil || !fresh {
		t.Fatalf("LatestAudit: fresh=%v err=%v", fresh, err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d assets, want 2", len(got))
	}
	if time.Since(runAt) > time.Minute {
		t.Errorf("runAt looks wrong: %v", runAt)
	}
	var peer *core.Asset
	for i := range got {
		if got[i].Type == "netbird.peer" {
			peer = &got[i]
		}
	}
	if peer == nil || peer.Tags["ip"] != "100.64.0.1" || !strings.Contains(string(peer.Raw), `"x":1`) {
		t.Errorf("peer round-trip lost tags/raw: %+v", peer)
	}
}

func TestAuditCache_NotFreshCases(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if _, err := st.SaveAudit(ctx, []string{"netbird"}, []core.Asset{{Provider: "netbird", Type: "netbird.peer", ID: "p1"}}); err != nil {
		t.Fatal(err)
	}
	// maxAge 0 disables the cache.
	if _, _, fresh, _ := st.LatestAudit(ctx, []string{"netbird"}, 0); fresh {
		t.Error("maxAge=0 must never be fresh")
	}
	// A different provider set is a cache miss.
	if _, _, fresh, _ := st.LatestAudit(ctx, []string{"oci"}, time.Hour); fresh {
		t.Error("different provider set must miss")
	}
	// Newest snapshot wins.
	if _, err := st.SaveAudit(ctx, []string{"netbird"}, []core.Asset{
		{Provider: "netbird", Type: "netbird.peer", ID: "p1"},
		{Provider: "netbird", Type: "netbird.peer", ID: "p2"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _, fresh, _ := st.LatestAudit(ctx, []string{"netbird"}, time.Hour)
	if !fresh || len(got) != 2 {
		t.Errorf("expected the newer 2-asset snapshot, got fresh=%v len=%d", fresh, len(got))
	}
}

func TestSecrets_Roundtrip(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	const pass = "correct horse battery staple"

	if err := st.SetSecret(ctx, "NETBIRD_API_TOKEN", "nbp_secret", pass); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	got, err := st.GetSecret(ctx, "NETBIRD_API_TOKEN", pass)
	if err != nil || got != "nbp_secret" {
		t.Fatalf("GetSecret: %q err=%v", got, err)
	}

	// Wrong passphrase must fail (GCM auth), not return garbage.
	if _, err := st.GetSecret(ctx, "NETBIRD_API_TOKEN", "wrong"); !errors.Is(err, store.ErrWrongPassphrase) {
		t.Errorf("wrong passphrase err = %v, want ErrWrongPassphrase", err)
	}
	// Missing secret.
	if _, err := st.GetSecret(ctx, "NOPE", pass); !errors.Is(err, store.ErrSecretNotFound) {
		t.Errorf("missing secret err = %v, want ErrSecretNotFound", err)
	}
	// No passphrase.
	if err := st.SetSecret(ctx, "X", "y", ""); !errors.Is(err, store.ErrNoPassphrase) {
		t.Errorf("empty passphrase err = %v, want ErrNoPassphrase", err)
	}

	// Overwrite.
	if err := st.SetSecret(ctx, "NETBIRD_API_TOKEN", "nbp_new", pass); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetSecret(ctx, "NETBIRD_API_TOKEN", pass); got != "nbp_new" {
		t.Errorf("after overwrite got %q, want nbp_new", got)
	}

	names, _ := st.ListSecretNames(ctx)
	if len(names) != 1 || names[0] != "NETBIRD_API_TOKEN" {
		t.Errorf("ListSecretNames = %v", names)
	}

	found, _ := st.DeleteSecret(ctx, "NETBIRD_API_TOKEN")
	if !found {
		t.Error("DeleteSecret should report found")
	}
	if again, _ := st.DeleteSecret(ctx, "NETBIRD_API_TOKEN"); again {
		t.Error("second delete should report not found")
	}
}

func TestSecrets_CiphertextIsNotPlaintext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSecret(context.Background(), "TOK", "PLAINTEXT_SECRET_VALUE", "pw"); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PLAINTEXT_SECRET_VALUE") {
		t.Error("secret value appears in plaintext in the database file")
	}
}

func TestLoadSecretsIntoEnv_ExplicitEnvWins(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	const pass = "pw"
	if err := st.SetSecret(ctx, "NB_TEST_TOKEN", "fromvault", pass); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSecret(ctx, "NB_TEST_OVERRIDE", "vaultval", pass); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NB_TEST_OVERRIDE", "envval") // pre-set → must NOT be overridden
	t.Cleanup(func() { _ = os.Unsetenv("NB_TEST_TOKEN") })

	loaded, err := st.LoadSecretsIntoEnv(ctx, pass)
	if err != nil {
		t.Fatalf("LoadSecretsIntoEnv: %v", err)
	}
	if os.Getenv("NB_TEST_TOKEN") != "fromvault" {
		t.Errorf("NB_TEST_TOKEN = %q, want fromvault", os.Getenv("NB_TEST_TOKEN"))
	}
	if os.Getenv("NB_TEST_OVERRIDE") != "envval" {
		t.Errorf("NB_TEST_OVERRIDE = %q, want the pre-set env value to win", os.Getenv("NB_TEST_OVERRIDE"))
	}
	if !contains(loaded, "NB_TEST_TOKEN") || contains(loaded, "NB_TEST_OVERRIDE") {
		t.Errorf("loaded = %v, want only NB_TEST_TOKEN", loaded)
	}
}

func TestStore_FilePermsArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	path := filepath.Join(t.TempDir(), "perms.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	// Force a WAL write so the -wal/-shm sidecars exist while we check them.
	if err := st.SetSecret(context.Background(), "X", "y", "pw"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue // sidecar may not exist on all platforms/modes
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s is group/other-accessible (%o); the secrets vault must be private", filepath.Base(p), perm)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
