package diff

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/cost"
)

// mkAsset builds a minimal asset; tests mutate copies for the "after" side.
func mkAsset(provider, typ, id, name string) core.Asset {
	return core.Asset{
		Provider:  provider,
		AccountID: "acct-1",
		Region:    "eu-frankfurt-1",
		Type:      typ,
		ID:        id,
		Name:      name,
	}
}

func TestCompute(t *testing.T) {
	base := mkAsset("oci", "compute_instance", "ocid1.instance.a", "web-1")

	statusChanged := base
	statusChanged.Status = "STOPPED"

	tagged := base
	tagged.Tags = map[string]string{"env": "staging", "team": "core"}
	retagged := base
	retagged.Tags = map[string]string{"env": "prod", "owner": "sre"}

	retyped := base
	retyped.Type = "compute_instance_v2"

	now := time.Now()
	withNoise := base
	withNoise.CreatedAt = &now
	withNoise.Raw = json.RawMessage(`{"etag":"different"}`)

	tests := []struct {
		name        string
		oldAssets   []core.Asset
		newAssets   []core.Asset
		wantAdded   []string // identity keys
		wantRemoved []string
		wantChanged map[string][]FieldChange // key -> expected field changes
	}{
		{
			name:        "both empty",
			oldAssets:   nil,
			newAssets:   nil,
			wantChanged: map[string][]FieldChange{},
		},
		{
			name:        "no change",
			oldAssets:   []core.Asset{base},
			newAssets:   []core.Asset{base},
			wantChanged: map[string][]FieldChange{},
		},
		{
			name:        "added",
			oldAssets:   nil,
			newAssets:   []core.Asset{base},
			wantAdded:   []string{"oci|ocid1.instance.a"},
			wantChanged: map[string][]FieldChange{},
		},
		{
			name:        "removed",
			oldAssets:   []core.Asset{base},
			newAssets:   nil,
			wantRemoved: []string{"oci|ocid1.instance.a"},
			wantChanged: map[string][]FieldChange{},
		},
		{
			name:      "status changed",
			oldAssets: []core.Asset{base},
			newAssets: []core.Asset{statusChanged},
			wantChanged: map[string][]FieldChange{
				"oci|ocid1.instance.a": {
					{Field: "status", Old: "", New: "STOPPED"},
				},
			},
		},
		{
			name:      "tag add remove and value change",
			oldAssets: []core.Asset{tagged},
			newAssets: []core.Asset{retagged},
			wantChanged: map[string][]FieldChange{
				"oci|ocid1.instance.a": {
					{Field: "tags.env", Old: "staging", New: "prod"},
					{Field: "tags.owner", Old: "", New: "sre"},
					{Field: "tags.team", Old: "core", New: ""},
				},
			},
		},
		{
			// (provider, id) is the identity, so a type change is a field
			// change on one asset — not a remove + add pair.
			name:      "type change is a change not add plus remove",
			oldAssets: []core.Asset{base},
			newAssets: []core.Asset{retyped},
			wantChanged: map[string][]FieldChange{
				"oci|ocid1.instance.a": {
					{Field: "type", Old: "compute_instance", New: "compute_instance_v2"},
				},
			},
		},
		{
			name:        "raw and created_at differences are not drift",
			oldAssets:   []core.Asset{base},
			newAssets:   []core.Asset{withNoise},
			wantChanged: map[string][]FieldChange{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Compute(tt.oldAssets, tt.newAssets)

			if res.Added == nil || res.Removed == nil || res.Changed == nil {
				t.Fatalf("Result slices must be non-nil: %+v", res)
			}

			gotAdded := keysOf(res.Added)
			if !equalStrings(gotAdded, tt.wantAdded) {
				t.Errorf("Added = %v, want %v", gotAdded, tt.wantAdded)
			}
			gotRemoved := keysOf(res.Removed)
			if !equalStrings(gotRemoved, tt.wantRemoved) {
				t.Errorf("Removed = %v, want %v", gotRemoved, tt.wantRemoved)
			}

			if len(res.Changed) != len(tt.wantChanged) {
				t.Fatalf("Changed count = %d, want %d (%+v)", len(res.Changed), len(tt.wantChanged), res.Changed)
			}
			for _, c := range res.Changed {
				want, ok := tt.wantChanged[Key(c.After)]
				if !ok {
					t.Errorf("unexpected change for %s", Key(c.After))
					continue
				}
				if len(c.Fields) != len(want) {
					t.Fatalf("fields for %s = %+v, want %+v", Key(c.After), c.Fields, want)
				}
				for i := range want {
					if c.Fields[i] != want[i] {
						t.Errorf("field[%d] = %+v, want %+v", i, c.Fields[i], want[i])
					}
				}
			}
		})
	}
}

// TestDiff_IgnoresCostTags guards the `audit --cost` integration: cost.* tags
// are computed by this tool from a price book, not read from the provider, so
// they move whenever the book's vintage or the flags do. If they were diffed,
// re-pricing an unchanged tenancy would report drift on every single asset and
// `auditor diff` would be useless the moment anyone enabled cost.
func TestDiff_IgnoresCostTags(t *testing.T) {
	before := mkAsset("oci", "oci.compute.instance", "ocid1.instance.a", "web-1")
	before.Tags = map[string]string{
		"env":           "prod",
		"cost.monthly":  "~29.20",
		"cost.currency": "USD",
		"cost.basis":    "assumed",
		"cost.detail":   "B92306×1 OCPU-hr @0.025 (730h/mo, marginal tier)",
	}

	// Same infrastructure, re-priced against a newer book: every cost tag
	// changes, one appears, one disappears. None of it is drift.
	after := before
	after.Tags = map[string]string{
		"env":          "prod",
		"cost.monthly": "~31.39",
		"cost.basis":   "inferred",
		"cost.detail":  "B92306×1 OCPU-hr @0.0269 (730h/mo, marginal tier)",
		"cost.vintage": "2027-01-04",
	}

	if res := Compute([]core.Asset{before}, []core.Asset{after}); !res.Empty() {
		t.Errorf("cost tags reported as drift: %+v", res.Changed)
	}

	// The skip must be scoped to the prefix, not to "anything containing
	// cost": a user's own cost-allocation tags are real infrastructure
	// metadata and losing them would hide the tagging drift that FinOps
	// allocation depends on.
	realDrift := before
	realDrift.Tags = maps.Clone(before.Tags)
	realDrift.Tags["cost-center"] = "eng-42"
	realDrift.Tags["costcenter"] = "eng-42"

	res := Compute([]core.Asset{before}, []core.Asset{realDrift})
	if len(res.Changed) != 1 {
		t.Fatalf("Changed count = %d, want 1", len(res.Changed))
	}
	want := []FieldChange{
		{Field: "tags.cost-center", Old: "", New: "eng-42"},
		{Field: "tags.costcenter", Old: "", New: "eng-42"},
	}
	if !slices.Equal(res.Changed[0].Fields, want) {
		t.Errorf("Fields = %+v, want %+v", res.Changed[0].Fields, want)
	}
}

// TestDiff_CostTagPrefixMatchesAnnotator pins the two ends of the guard rail
// together. internal/diff deliberately does not import internal/cost in
// production code — that would drag the embedded price books in for one string
// — so the constants are duplicated, and this test is what stops the copy from
// drifting. If the annotator renames its namespace, this fails rather than the
// skip silently ceasing to match.
func TestDiff_CostTagPrefixMatchesAnnotator(t *testing.T) {
	if costTagPrefix != cost.TagPrefix {
		t.Errorf("costTagPrefix = %q, but cost.TagPrefix = %q", costTagPrefix, cost.TagPrefix)
	}
	for _, key := range []string{cost.TagMonthly, cost.TagCurrency, cost.TagBasis, cost.TagDetail} {
		if !isDerivedTag(key) {
			t.Errorf("annotator tag %q is not skipped by the diff", key)
		}
	}
}

func TestCompute_DeterministicOrder(t *testing.T) {
	a := mkAsset("cloudflare", "dns_record", "rec-1", "api")
	b := mkAsset("kubernetes", "Service", "svc-1", "gateway")
	c := mkAsset("oci", "compute_instance", "ocid1.a", "web-1")
	d := mkAsset("oci", "load_balancer", "ocid1.b", "lb-1")

	// Feed in scrambled order; expect (provider, type, id) ordering out.
	res := Compute(nil, []core.Asset{d, b, c, a})
	got := keysOf(res.Added)
	want := []string{"cloudflare|rec-1", "kubernetes|svc-1", "oci|ocid1.a", "oci|ocid1.b"}
	if !equalStrings(got, want) {
		t.Errorf("Added order = %v, want %v", got, want)
	}
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantIDs []string
		wantErr bool
	}{
		{
			name:    "json array",
			input:   `[{"provider":"oci","account_id":"a","type":"t","id":"1","name":"one"},{"provider":"oci","account_id":"a","type":"t","id":"2","name":"two"}]`,
			wantIDs: []string{"1", "2"},
		},
		{
			name:    "json array with leading whitespace",
			input:   "\n\t  [{\"provider\":\"oci\",\"account_id\":\"a\",\"type\":\"t\",\"id\":\"1\",\"name\":\"one\"}]",
			wantIDs: []string{"1"},
		},
		{
			name:    "empty array",
			input:   "[]\n",
			wantIDs: []string{},
		},
		{
			name: "ndjson",
			input: `{"provider":"oci","account_id":"a","type":"t","id":"1","name":"one"}
{"provider":"cf","account_id":"a","type":"t","id":"2","name":"two"}
`,
			wantIDs: []string{"1", "2"},
		},
		{
			name:    "ndjson single object no trailing newline",
			input:   `{"provider":"oci","account_id":"a","type":"t","id":"1","name":"one"}`,
			wantIDs: []string{"1"},
		},
		{
			// A zero-asset --stream run writes nothing at all.
			name:    "empty input is a valid empty snapshot",
			input:   "",
			wantIDs: []string{},
		},
		{
			name:    "whitespace-only input",
			input:   " \n\t ",
			wantIDs: []string{},
		},
		{
			name:    "garbage",
			input:   "definitely not json",
			wantErr: true,
		},
		{
			name:    "truncated array",
			input:   `[{"provider":"oci"`,
			wantErr: true,
		},
		{
			name:    "garbage after valid ndjson object",
			input:   "{\"provider\":\"oci\",\"id\":\"1\"}\nnope\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assets, err := Load(strings.NewReader(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if assets == nil {
				t.Fatal("Load() must return a non-nil slice")
			}
			got := make([]string, 0, len(assets))
			for _, a := range assets {
				got = append(got, a.ID)
			}
			if !equalStrings(got, tt.wantIDs) {
				t.Errorf("Load() IDs = %v, want %v", got, tt.wantIDs)
			}
		})
	}
}

// canonicalResult is the fixture shared by the renderer tests: one of each
// category, with a multi-field change.
func canonicalResult() Result {
	before := mkAsset("oci", "compute_instance", "ocid1.instance.a", "web-1")
	before.Status = "RUNNING"
	before.Tags = map[string]string{"env": "staging"}
	after := before
	after.Status = "STOPPED"
	after.Tags = map[string]string{"env": "prod"}

	added := mkAsset("kubernetes", "Service", "default/gateway", "gateway")
	removed := mkAsset("cloudflare", "dns_record", "rec-1", "api.example.com")
	removed.Region = ""

	return Compute([]core.Asset{before, removed}, []core.Asset{after, added})
}

func TestRenderTable(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderTable(&buf, canonicalResult(), 2, 2); err != nil {
		t.Fatalf("RenderTable() error = %v", err)
	}
	want := `1 added, 1 removed, 1 changed (old: 2 assets, new: 2 assets)

Added:
  + kubernetes/Service default/gateway (gateway)

Removed:
  - cloudflare/dns_record rec-1 (api.example.com)

Changed:
  ~ oci/compute_instance ocid1.instance.a (web-1)
      status: "RUNNING" -> "STOPPED"
      tags.env: "staging" -> "prod"
`
	if got := buf.String(); got != want {
		t.Errorf("RenderTable() =\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderTable_NoDrift(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderTable(&buf, Compute(nil, nil), 3, 3); err != nil {
		t.Fatalf("RenderTable() error = %v", err)
	}
	want := "No drift (old: 3 assets, new: 3 assets).\n"
	if got := buf.String(); got != want {
		t.Errorf("RenderTable() = %q, want %q", got, want)
	}
}

func TestRenderMarkdown(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderMarkdown(&buf, canonicalResult(), 2, 2); err != nil {
		t.Fatalf("RenderMarkdown() error = %v", err)
	}
	want := "## Audit drift\n\n" +
		"**1 added, 1 removed, 1 changed** (old: 2 assets, new: 2 assets)\n" +
		"\n### Added (1)\n\n" +
		"- `kubernetes/Service` `default/gateway` — gateway\n" +
		"\n### Removed (1)\n\n" +
		"- `cloudflare/dns_record` `rec-1` — api.example.com\n" +
		"\n### Changed (1)\n\n" +
		"- `oci/compute_instance` `ocid1.instance.a` — web-1\n" +
		"  - `status`: `\"RUNNING\"` → `\"STOPPED\"`\n" +
		"  - `tags.env`: `\"staging\"` → `\"prod\"`\n"
	if got := buf.String(); got != want {
		t.Errorf("RenderMarkdown() =\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderMarkdown_NoDrift(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderMarkdown(&buf, Compute(nil, nil), 0, 0); err != nil {
		t.Fatalf("RenderMarkdown() error = %v", err)
	}
	want := "## Audit drift\n\nNo drift (old: 0 assets, new: 0 assets).\n"
	if got := buf.String(); got != want {
		t.Errorf("RenderMarkdown() = %q, want %q", got, want)
	}
}

func TestRenderJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, canonicalResult(), 2, 2); err != nil {
		t.Fatalf("RenderJSON() error = %v", err)
	}

	var report struct {
		Added   []core.Asset `json:"added"`
		Removed []core.Asset `json:"removed"`
		Changed []Change     `json:"changed"`
		Summary struct {
			Added    int `json:"added"`
			Removed  int `json:"removed"`
			Changed  int `json:"changed"`
			OldTotal int `json:"old_total"`
			NewTotal int `json:"new_total"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if report.Summary.Added != 1 || report.Summary.Removed != 1 || report.Summary.Changed != 1 {
		t.Errorf("summary counts = %+v, want 1/1/1", report.Summary)
	}
	if report.Summary.OldTotal != 2 || report.Summary.NewTotal != 2 {
		t.Errorf("summary totals = %+v, want old 2 / new 2", report.Summary)
	}
	if len(report.Added) != 1 || report.Added[0].ID != "default/gateway" {
		t.Errorf("added = %+v", report.Added)
	}
	if len(report.Changed) != 1 || len(report.Changed[0].Fields) != 2 {
		t.Errorf("changed = %+v", report.Changed)
	}
}

func TestRenderJSON_EmptyResultUsesArraysNotNull(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, Compute(nil, nil), 0, 0); err != nil {
		t.Fatalf("RenderJSON() error = %v", err)
	}
	out := buf.String()
	// The API contract: empty categories are [] so consumers can iterate
	// without nil checks.
	for _, want := range []string{`"added":[]`, `"removed":[]`, `"changed":[]`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %s:\n%s", want, out)
		}
	}
}

func keysOf(assets []core.Asset) []string {
	out := make([]string, 0, len(assets))
	for _, a := range assets {
		out = append(out, Key(a))
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
