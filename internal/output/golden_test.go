package output_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// update rewrites all golden files when set. Invoke via `go test -update`
// (or `just test-update`).
var update = flag.Bool("update", false, "rewrite golden files")

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)

	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("update golden %s: %v", name, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run `go test -update` to create)", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output mismatch for %s\n--- want (%d bytes) ---\n%s\n--- got (%d bytes) ---\n%s",
			name, len(want), want, len(got), got)
	}
}

func mustTime(t *testing.T, s string) *time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return &v
}

// fixtureAssets is the shared set used by JSON and CSV tests. Keep it small
// and deterministic — two assets that together exercise:
//   - omitempty for Region (asset 1) and CreatedAt/Tags (asset 2)
//   - tag key sort order (env < team)
//   - non-empty Status on both
func fixtureAssets(t *testing.T) []core.Asset {
	return []core.Asset{
		{
			Provider:  "cloudflare",
			AccountID: "acct1",
			Type:      "cloudflare.zone",
			ID:        "zone-1",
			Name:      "example.com",
			Status:    "active",
			CreatedAt: mustTime(t, "2025-01-02T03:04:05Z"),
			Tags:      map[string]string{"env": "prod", "team": "platform"},
		},
		{
			Provider:  "kubernetes",
			AccountID: "kind-test",
			Region:    "local",
			Type:      "v1.Pod",
			ID:        "uid-2",
			Name:      "nginx-abc",
			Status:    "Running",
		},
	}
}

// feedAssets returns a closed, prefilled channel — convenient for renderer
// tests where the concurrency story isn't under test.
func feedAssets(assets []core.Asset) <-chan core.Asset {
	ch := make(chan core.Asset, len(assets))
	for _, a := range assets {
		ch <- a
	}
	close(ch)
	return ch
}
