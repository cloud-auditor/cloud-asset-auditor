package output_test

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/xuri/excelize/v2"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/output"
)

// renderXLSX renders assets to an in-memory workbook and reopens it for
// assertions. It exercises the real excelize read path, so a malformed file
// fails here.
func renderXLSX(t *testing.T, r *output.XLSX, assets []core.Asset) *excelize.File {
	t.Helper()
	var buf bytes.Buffer
	if err := r.Render(context.Background(), feedAssets(assets), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("reopen xlsx: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func rowsOf(t *testing.T, f *excelize.File, sheet string) [][]string {
	t.Helper()
	rows, err := f.GetRows(sheet)
	if err != nil {
		t.Fatalf("GetRows(%q): %v", sheet, err)
	}
	return rows
}

func headerIndex(t *testing.T, rows [][]string, name string) int {
	t.Helper()
	for i, h := range rows[0] {
		if h == name {
			return i
		}
	}
	t.Fatalf("header %q not found in %v", name, rows[0])
	return -1
}

// cellAt tolerates GetRows trimming trailing empty cells.
func cellAt(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return row[i]
}

func TestXLSX_GroupByProvider(t *testing.T) {
	f := renderXLSX(t, &output.XLSX{SheetBy: "provider"}, fixtureAssets(t))

	if got, want := f.GetSheetList(), []string{"cloudflare", "kubernetes"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sheets = %v, want %v", got, want)
	}

	cf := rowsOf(t, f, "cloudflare")
	if len(cf) != 2 { // header + 1 asset
		t.Fatalf("cloudflare rows = %d, want 2", len(cf))
	}
	nameCol := headerIndex(t, cf, "Name")
	if got := cellAt(cf[1], nameCol); got != "example.com" {
		t.Errorf("cloudflare Name = %q, want example.com", got)
	}
	// The cloudflare asset's freeform tags become their own columns.
	headerIndex(t, cf, "Env")
	headerIndex(t, cf, "Team")

	if k := rowsOf(t, f, "kubernetes"); len(k) != 2 {
		t.Errorf("kubernetes rows = %d, want 2", len(k))
	}
}

func TestXLSX_None_SingleSheet(t *testing.T) {
	f := renderXLSX(t, &output.XLSX{SheetBy: "none"}, fixtureAssets(t))
	if got, want := f.GetSheetList(), []string{"Assets"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sheets = %v, want %v", got, want)
	}
	if rows := rowsOf(t, f, "Assets"); len(rows) != 3 { // header + 2 assets
		t.Errorf("rows = %d, want 3", len(rows))
	}
}

func TestXLSX_TagGrouping_ResolvesNameAndPlacesContainer(t *testing.T) {
	assets := []core.Asset{
		{Provider: "oci", Type: "oci.compartment", ID: "ocid.comp.A", Name: "production"},
		{Provider: "oci", Type: "oci.vcn", ID: "v1", Name: "prod-vcn",
			Tags: map[string]string{"compartment_id": "ocid.comp.A"}},
		{Provider: "oci", Type: "oci.subnet", ID: "s1", Name: "prod-subnet",
			Tags: map[string]string{"compartment_id": "ocid.comp.A"}},
	}
	f := renderXLSX(t, &output.XLSX{SheetBy: "tag:compartment_id"}, assets)

	// Sheet labelled by the compartment's NAME (resolved from the OCID), and
	// the compartment asset itself (which lacks the tag) lands in the sheet.
	if got, want := f.GetSheetList(), []string{"production"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sheets = %v, want %v", got, want)
	}
	rows := rowsOf(t, f, "production")
	if len(rows) != 4 { // header + compartment + vcn + subnet
		t.Fatalf("rows = %d, want 4", len(rows))
	}
	// The grouping tag is excluded from the columns (redundant with the sheet).
	for _, h := range rows[0] {
		if h == "Compartment Id" {
			t.Errorf("grouping tag should not appear as a column: %v", rows[0])
		}
	}
	nameCol := headerIndex(t, rows, "Name")
	names := map[string]bool{}
	for _, r := range rows[1:] {
		names[cellAt(r, nameCol)] = true
	}
	for _, want := range []string{"production", "prod-vcn", "prod-subnet"} {
		if !names[want] {
			t.Errorf("missing %q in sheet; got %v", want, names)
		}
	}
}

func TestXLSX_TagColumnsAreUnionPerSheet(t *testing.T) {
	assets := []core.Asset{
		{Provider: "oci", Type: "oci.compute.instance", ID: "i1", Name: "vm",
			Tags: map[string]string{"compartment_id": "C", "shape": "E4"}},
		{Provider: "oci", Type: "oci.iam.user", ID: "u1", Name: "alice",
			Tags: map[string]string{"compartment_id": "C", "email": "a@x"}},
	}
	f := renderXLSX(t, &output.XLSX{SheetBy: "tag:compartment_id"}, assets)
	rows := rowsOf(t, f, "C")

	emailCol := headerIndex(t, rows, "Email")
	shapeCol := headerIndex(t, rows, "Shape")
	nameCol := headerIndex(t, rows, "Name")

	// rows sorted by type: compute (vm) before iam.user (alice)
	if cellAt(rows[1], nameCol) != "vm" || cellAt(rows[1], shapeCol) != "E4" || cellAt(rows[1], emailCol) != "" {
		t.Errorf("vm row wrong: %v", rows[1])
	}
	if cellAt(rows[2], nameCol) != "alice" || cellAt(rows[2], emailCol) != "a@x" || cellAt(rows[2], shapeCol) != "" {
		t.Errorf("alice row wrong: %v", rows[2])
	}
}

func TestXLSX_Empty_ProducesValidWorkbook(t *testing.T) {
	f := renderXLSX(t, &output.XLSX{SheetBy: "provider"}, nil)
	if got, want := f.GetSheetList(), []string{"Assets"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sheets = %v, want %v", got, want)
	}
	rows := rowsOf(t, f, "Assets")
	if len(rows) != 1 || len(rows[0]) != len([]string{"Provider", "Account ID", "Region", "Type", "ID", "Name", "Status", "Created At (UTC)"}) {
		t.Errorf("expected header-only sheet with 8 core columns, got %v", rows)
	}
}

func TestXLSX_InvalidSheetBy(t *testing.T) {
	r := &output.XLSX{SheetBy: "bogus"}
	if err := r.Validate(); err == nil {
		t.Fatal("Validate accepted bogus sheet-by")
	}
	if err := r.Render(context.Background(), feedAssets(nil), &bytes.Buffer{}); err == nil {
		t.Fatal("Render accepted bogus sheet-by")
	}
}

func TestXLSX_SheetNamesSanitizedAndUnique(t *testing.T) {
	long := ""
	for i := 0; i < 40; i++ {
		long += "x"
	}
	assets := []core.Asset{
		{Provider: "oci", Type: "t", ID: "a", Tags: map[string]string{"k": "a/b:c*d?e[f]"}},
		{Provider: "oci", Type: "t", ID: "b", Tags: map[string]string{"k": long}},
		{Provider: "oci", Type: "t", ID: "c", Tags: map[string]string{"k": "dup"}},
		{Provider: "oci", Type: "t", ID: "d", Tags: map[string]string{"k": "dup"}}, // same group → same sheet, not a dup name
	}
	f := renderXLSX(t, &output.XLSX{SheetBy: "tag:k"}, assets)
	seen := map[string]bool{}
	for _, name := range f.GetSheetList() {
		if len([]rune(name)) > 31 {
			t.Errorf("sheet name too long: %q", name)
		}
		for _, bad := range []rune{':', '\\', '/', '?', '*', '[', ']'} {
			if containsRune(name, bad) {
				t.Errorf("sheet name %q contains illegal rune %q", name, bad)
			}
		}
		if seen[name] {
			t.Errorf("duplicate sheet name %q", name)
		}
		seen[name] = true
	}
	// "a/b:c*d?e[f]" and the 40-char value and "dup" → 3 distinct sheets.
	if len(seen) != 3 {
		t.Errorf("expected 3 sheets, got %d: %v", len(seen), f.GetSheetList())
	}
}

func TestXLSX_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// An open, never-fed channel: drain must bail on the cancelled context.
	err := (&output.XLSX{SheetBy: "provider"}).Render(ctx, make(chan core.Asset), &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
