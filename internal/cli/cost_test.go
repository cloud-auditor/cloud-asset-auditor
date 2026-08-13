package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// costFixture is one asset per outcome the annotator can produce, so a single
// audit exercises priced, metered, and unknown in one pass.
var costFixture = []core.Asset{
	// Priced: a volume that carries its own size reads its quantity straight
	// off the asset, so it exercises the ordinary tag path rather than a rule
	// that prices from a literal — 200 GB Balanced is the book's own
	// conformance case (200 × (0.0255 + 10×0.0017) = $8.50/mo).
	{Provider: "oci", Type: "oci.block_volume", ID: "ocid1.volume.a", Name: "prod-data-01",
		Tags: map[string]string{"size_gb": "200", "vpus_per_gb": "10"}},
	// Metered: R2 is declared unpriceable — billing is consumption-based.
	{Provider: "cloudflare", Type: "cloudflare.r2_bucket", ID: "r2-assets", Name: "assets"},
	// Unknown: no rule in the book. A gap in the book, not a free resource.
	{Provider: "oci", Type: "oci.not.a.real.type", ID: "ocid1.mystery.a", Name: "mystery"},
}

type costFakeProvider struct{}

func (costFakeProvider) Name() string                   { return "costfake" }
func (costFakeProvider) Validate(context.Context) error { return nil }
func (costFakeProvider) Collect(context.Context) (<-chan core.Asset, <-chan error) {
	assets, errs := make(chan core.Asset, len(costFixture)), make(chan error)
	for _, a := range costFixture {
		assets <- a
	}
	close(assets)
	close(errs)
	return assets, errs
}

func init() {
	core.Register("costfake", func() (core.Provider, error) { return costFakeProvider{}, nil })
}

// runAudit executes the real cobra tree and returns what the JSON renderer
// wrote. --db points at a temp file so the run never touches the operator's
// secrets vault or audit cache.
func runAudit(t *testing.T, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "assets.json")

	root := newRootCmd()
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	root.SetArgs(append([]string{
		"audit", "--provider", "costfake", "-o", "json",
		"--output-file", out, "--db", filepath.Join(dir, "auditor.db"),
	}, extra...))
	if err := root.Execute(); err != nil {
		t.Fatalf("audit %v: %v", extra, err)
	}

	data, err := os.ReadFile(out) // #nosec G304 -- test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The off-by-default guarantee, asserted on the bytes rather than on the flag:
// a plain audit must be indistinguishable from one run before cost existed,
// which is also what keeps internal/output's golden files from moving.
func TestAuditCost_OffByDefaultLeavesTheOutputUntouched(t *testing.T) {
	t.Chdir(t.TempDir()) // no stray ./auditor.yaml from the repo root

	plain := runAudit(t)
	if strings.Contains(plain, "cost.") {
		t.Fatalf("a plain audit emitted cost tags:\n%s", plain)
	}

	// The strong form: the rendered bytes are exactly the fixture, so the
	// stream carries no trace of the pipeline stage at all — not an empty tags
	// map on the tagless assets, not a reordered key on the tagged one.
	want, err := json.Marshal(costFixture)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(plain) != string(want) {
		t.Errorf("a plain audit did not render the fixture verbatim\n got: %s\nwant: %s", plain, want)
	}

	if again := runAudit(t); again != plain {
		t.Error("two plain audits of the same fixture differ")
	}
}

func TestAuditCost_StampsTagsWithoutEverWritingAZero(t *testing.T) {
	t.Chdir(t.TempDir())

	var assets []core.Asset
	if err := json.Unmarshal([]byte(runAudit(t, "--cost")), &assets); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(assets) != len(costFixture) {
		t.Fatalf("got %d assets, want %d", len(assets), len(costFixture))
	}

	byID := map[string]core.Asset{}
	for _, a := range assets {
		byID[a.ID] = a
	}

	// The one rule that matters most: an unpriced resource must never render
	// as a number, and "0"/"0.00" must be unreachable except by the stopped-
	// instance path (which no fixture asset takes). $0.00 is a real price in
	// OCI's feed, so zero and unknown have to stay impossible to confuse.
	for _, a := range assets {
		monthly := a.Tags["cost.monthly"]
		if monthly == "" {
			t.Errorf("%s: no cost.monthly tag", a.ID)
		}
		if monthly == "0" || monthly == "0.00" || monthly == "~0.00" {
			t.Errorf("%s: cost.monthly = %q — a zero nobody asked for", a.ID, monthly)
		}
		if a.Tags["cost.basis"] == "" {
			t.Errorf("%s: no cost.basis tag", a.ID)
		}
	}

	priced := byID["ocid1.volume.a"]
	if got := priced.Tags["cost.monthly"]; got != "~8.50" {
		t.Errorf("priced asset cost.monthly = %q, want %q — 200 GB Balanced, with the ~ that says it is an estimate", got, "~8.50")
	}
	if got := priced.Tags["cost.currency"]; got == "" {
		t.Error("priced asset has no cost.currency; a number without its currency is not a price")
	}
	if got := byID["r2-assets"].Tags["cost.monthly"]; got != "metered" {
		t.Errorf("consumption-billed asset cost.monthly = %q, want %q", got, "metered")
	}
	if got := byID["ocid1.mystery.a"].Tags["cost.monthly"]; got != "unknown" {
		t.Errorf("unruled asset cost.monthly = %q, want %q", got, "unknown")
	}
}

// Annotation runs before the filter stage, which is the whole reason
// `--filter tag:cost.basis=...` is usable at all.
func TestAuditCost_FilterSeesTheAnnotatedTags(t *testing.T) {
	t.Chdir(t.TempDir())

	var assets []core.Asset
	out := runAudit(t, "--cost", "--filter", "tag:cost.monthly=metered")
	if err := json.Unmarshal([]byte(out), &assets); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(assets) != 1 || assets[0].ID != "r2-assets" {
		t.Fatalf("filter on an annotated tag selected %d assets (%s), want just r2-assets", len(assets), out)
	}
}

// The cache stores the UNANNOTATED snapshot and annotation happens on the way
// out, so a cache hit is priced with today's book rather than replaying the
// prices that were current when the snapshot was taken.
func TestAuditCost_CacheStoresUnannotatedAssets(t *testing.T) {
	t.Chdir(t.TempDir())

	dir := t.TempDir()
	db := filepath.Join(dir, "auditor.db")
	first := filepath.Join(dir, "first.json")
	second := filepath.Join(dir, "second.json")

	// Populate the cache from a run that WAS annotated...
	root := newRootCmd()
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	root.SetArgs([]string{"audit", "--provider", "costfake", "-o", "json",
		"--output-file", first, "--db", db, "--cost", "--cache"})
	if err := root.Execute(); err != nil {
		t.Fatalf("seeding audit: %v", err)
	}

	// ...then serve from it WITHOUT --cost. If the stored snapshot carried the
	// tags, they would reappear here.
	root = newRootCmd()
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	root.SetArgs([]string{"audit", "--provider", "costfake", "-o", "json",
		"--output-file", second, "--db", db, "--cache-max-age", "1h"})
	if err := root.Execute(); err != nil {
		t.Fatalf("cached audit: %v", err)
	}

	data, err := os.ReadFile(second) // #nosec G304 -- test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "cost.") {
		t.Errorf("cached snapshot carried cost tags:\n%s", data)
	}
}
