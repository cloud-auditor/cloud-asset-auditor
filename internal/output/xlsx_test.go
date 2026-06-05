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

func TestXLSX_CompositeRegionAndCompartment(t *testing.T) {
	assets := []core.Asset{
		// The compartment container is region-less; its resources span regions.
		{Provider: "oci", Type: "oci.compartment", ID: "ocid.comp.A", Name: "production"},
		{Provider: "oci", Type: "oci.vcn", ID: "v1", Name: "jed-vcn", Region: "me-jeddah-1",
			Tags: map[string]string{"compartment_id": "ocid.comp.A"}},
		{Provider: "oci", Type: "oci.subnet", ID: "s1", Name: "jed-subnet", Region: "me-jeddah-1",
			Tags: map[string]string{"compartment_id": "ocid.comp.A"}},
		{Provider: "oci", Type: "oci.vcn", ID: "v2", Name: "riy-vcn", Region: "me-riyadh-1",
			Tags: map[string]string{"compartment_id": "ocid.comp.A"}},
	}
	f := renderXLSX(t, &output.XLSX{SheetBy: "region+tag:compartment_id"}, assets)

	got := f.GetSheetList()
	// One sheet per (region, compartment); the region-less compartment asset
	// lands in its own "(no region) (production)" sheet.
	want := []string{"(no region) (production)", "me-jeddah-1 (production)", "me-riyadh-1 (production)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sheets = %v, want %v", got, want)
	}

	jed := rowsOf(t, f, "me-jeddah-1 (production)")
	if len(jed) != 3 { // header + vcn + subnet
		t.Fatalf("jeddah rows = %d, want 3", len(jed))
	}
	// The grouping tag is dropped from the columns (redundant with the sheet).
	for _, h := range jed[0] {
		if h == "Compartment Id" {
			t.Errorf("grouping tag should not be a column: %v", jed[0])
		}
	}
	if riy := rowsOf(t, f, "me-riyadh-1 (production)"); len(riy) != 2 { // header + 1 vcn
		t.Errorf("riyadh rows = %d, want 2", len(riy))
	}
	if comp := rowsOf(t, f, "(no region) (production)"); len(comp) != 2 { // header + compartment
		t.Errorf("compartment sheet rows = %d, want 2", len(comp))
	}
}

func TestXLSX_CompositeLabelResolvesEmptyAndName(t *testing.T) {
	// provider+region: head bare, region parenthesised; empty region → "(no region)".
	assets := []core.Asset{
		{Provider: "oci", Type: "t", ID: "a", Region: "me-jeddah-1"},
		{Provider: "oci", Type: "t", ID: "b", Region: ""},
	}
	f := renderXLSX(t, &output.XLSX{SheetBy: "provider+region"}, assets)
	got := f.GetSheetList()
	want := []string{"oci (me-jeddah-1)", "oci (no region)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sheets = %v, want %v", got, want)
	}
}

func TestXLSX_CompositeValidation(t *testing.T) {
	ok := []string{"region+tag:compartment_id", "provider+type+region", "tag:a+tag:b"}
	for _, s := range ok {
		if err := (&output.XLSX{SheetBy: s}).Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{"region+bogus", "region+tag:", "region+", "+region", "tag:",
		"region+region", "tag:a+tag:a"} // duplicate dimensions are rejected
	for _, s := range bad {
		if err := (&output.XLSX{SheetBy: s}).Validate(); err == nil {
			t.Errorf("Validate(%q) = nil, want error", s)
		}
	}
}

func TestXLSX_CompositeExcludesAllGroupingTagColumns(t *testing.T) {
	// With two tag dimensions, BOTH grouping tag keys are dropped from the
	// per-sheet columns; a third, non-grouping tag survives as a column.
	assets := []core.Asset{
		{Provider: "oci", Type: "t", ID: "x1", Name: "one",
			Tags: map[string]string{"a": "A1", "b": "B1", "c": "C1"}},
	}
	f := renderXLSX(t, &output.XLSX{SheetBy: "tag:a+tag:b"}, assets)
	rows := rowsOf(t, f, "A1 (B1)")
	for _, h := range rows[0] {
		if h == "A" || h == "B" {
			t.Errorf("grouping tag column %q should be excluded: %v", h, rows[0])
		}
	}
	if headerIndex(t, rows, "C") < 0 {
		t.Errorf("non-grouping tag column C missing: %v", rows[0])
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

func TestXLSX_TagHeaderCollidesWithCoreColumn(t *testing.T) {
	// A label/tag key whose pretty header equals a core column ("name" -> "Name",
	// "status" -> "Status") must not produce a duplicate column header; the tag
	// column is disambiguated and the value still lands under it.
	assets := []core.Asset{
		{Provider: "kubernetes", Type: "v1.Secret", ID: "x1", Name: "argocd",
			Status: "Active", Tags: map[string]string{"name": "argocd-root-apps", "status": "synced"}},
	}
	f := renderXLSX(t, &output.XLSX{SheetBy: "none"}, assets)
	rows := rowsOf(t, f, "Assets")

	// No header appears twice.
	seen := map[string]int{}
	for _, h := range rows[0] {
		seen[h]++
	}
	for h, n := range seen {
		if n > 1 {
			t.Errorf("duplicate column header %q (x%d): %v", h, n, rows[0])
		}
	}

	// The core columns keep their resource values...
	nameCol := headerIndex(t, rows, "Name")
	statusCol := headerIndex(t, rows, "Status")
	if cellAt(rows[1], nameCol) != "argocd" {
		t.Errorf("core Name = %q, want argocd", cellAt(rows[1], nameCol))
	}
	if cellAt(rows[1], statusCol) != "Active" {
		t.Errorf("core Status = %q, want Active", cellAt(rows[1], statusCol))
	}
	// ...and the colliding label values appear under the disambiguated columns.
	nameTagCol := headerIndex(t, rows, "Name (tag)")
	statusTagCol := headerIndex(t, rows, "Status (tag)")
	if cellAt(rows[1], nameTagCol) != "argocd-root-apps" {
		t.Errorf("Name (tag) = %q, want argocd-root-apps", cellAt(rows[1], nameTagCol))
	}
	if cellAt(rows[1], statusTagCol) != "synced" {
		t.Errorf("Status (tag) = %q, want synced", cellAt(rows[1], statusTagCol))
	}
}

func TestXLSX_SummarySheet(t *testing.T) {
	assets := []core.Asset{
		{Provider: "kubernetes", Type: "v1.Pod", ID: "p1", Name: "pod-a",
			Tags: map[string]string{"namespace": "argocd"}},
		{Provider: "kubernetes", Type: "v1.Pod", ID: "p2", Name: "pod-b",
			Tags: map[string]string{"namespace": "argocd"}},
		{Provider: "kubernetes", Type: "v1.Service", ID: "s1", Name: "svc",
			Tags: map[string]string{"namespace": "loki"}},
	}
	f := renderXLSX(t, &output.XLSX{SheetBy: "tag:namespace", Summary: true}, assets)

	// Summary is the FIRST sheet, followed by the group sheets.
	got := f.GetSheetList()
	want := []string{"Summary", "argocd", "loki"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sheets = %v, want %v", got, want)
	}

	rows := rowsOf(t, f, "Summary")
	// Flatten all summary cells for simple content assertions.
	flat := map[string]string{}
	for _, r := range rows {
		for i := 0; i+1 < len(r); i++ {
			if r[i] != "" {
				flat[r[i]] = r[i+1]
			}
		}
	}
	if flat["Total assets"] != "3" {
		t.Errorf("Total assets = %q, want 3 (cells: %v)", flat["Total assets"], rows)
	}
	if flat["Worksheets"] != "2" {
		t.Errorf("Worksheets = %q, want 2", flat["Worksheets"])
	}
	// Per-sheet section uses the tag's pretty name and lists each namespace count.
	if _, ok := flat["By Namespace"]; !ok {
		t.Errorf("missing 'By Namespace' header: %v", rows)
	}
	if flat["argocd"] != "2" || flat["loki"] != "1" {
		t.Errorf("per-namespace counts wrong: argocd=%q loki=%q", flat["argocd"], flat["loki"])
	}
	// Per-type section.
	if flat["v1.Pod"] != "2" || flat["v1.Service"] != "1" {
		t.Errorf("per-type counts wrong: Pod=%q Service=%q", flat["v1.Pod"], flat["v1.Service"])
	}
}

func TestXLSX_SummaryHyperlinkTargetsSheet(t *testing.T) {
	assets := []core.Asset{
		{Provider: "oci", Type: "t", ID: "a", Region: "me-jeddah-1"},
	}
	f := renderXLSX(t, &output.XLSX{SheetBy: "region", Summary: true}, assets)
	rows := rowsOf(t, f, "Summary")
	// Find the cell holding the group label and assert it carries a hyperlink
	// to that sheet.
	for ri, r := range rows {
		for ci, c := range r {
			if c == "me-jeddah-1" {
				axis, _ := excelize.CoordinatesToCellName(ci+1, ri+1)
				ok, target, err := f.GetCellHyperLink("Summary", axis)
				if err != nil {
					t.Fatal(err)
				}
				if !ok || target != "'me-jeddah-1'!A1" {
					t.Errorf("hyperlink = (%v, %q), want true, 'me-jeddah-1'!A1", ok, target)
				}
				return
			}
		}
	}
	t.Fatal("group label 'me-jeddah-1' not found in summary")
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
