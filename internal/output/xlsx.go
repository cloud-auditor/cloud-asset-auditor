package output

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// XLSX renders assets into a multi-sheet Excel workbook (.xlsx).
//
// Unlike the JSON and CSV renderers, XLSX CANNOT stream: an .xlsx file is a
// ZIP whose central directory is written last, and the full asset set must be
// seen before sheets and per-sheet columns are known. This renderer therefore
// buffers the entire stream in memory — a deliberate, documented exception to
// the project's "stream end-to-end" invariant, justified by xlsx being an
// explicit human-facing export rather than a pipe-friendly wire format. Memory
// is O(total assets) and excelize buffers the workbook XML too; for very large
// inventories (50k+ objects) prefer CSV or NDJSON.
//
// SheetBy selects how assets are partitioned across worksheets. It names one
// or more dimensions joined by "+"; each dimension is one of:
//
//	""/"none"  → a single "Assets" sheet
//	"provider" → one sheet per provider
//	"type"     → one sheet per asset type
//	"region"   → one sheet per region
//	"account"  → one sheet per account_id
//	"tag:KEY"  → one sheet per distinct Tags[KEY] value
//
// Composite example: "region+tag:compartment_id" yields one sheet per
// (region, compartment) pair, labelled "<region> (<compartment>)" — the head
// dimension bare, the rest parenthesised. This is the OCI "sheet per
// region/compartment" layout.
//
// Two conveniences apply to tag grouping (per tag dimension). When a group
// value matches a known asset ID (e.g. tag:compartment_id values are
// compartment OCIDs), that part of the label uses the asset's Name instead of
// the raw ID. And a "container" asset that lacks the grouping tag but whose own
// ID is referenced by it (the compartment that owns the resources) is placed in
// that group rather than a catch-all.
type XLSX struct {
	SheetBy string
	// Summary, when true, prepends a "Summary" worksheet: total asset count,
	// a per-sheet breakdown (each row hyperlinked to its sheet), and a per-type
	// breakdown. It's a navigable index for multi-sheet workbooks.
	Summary bool
}

// dimension is one partition axis parsed from SheetBy.
type dimension struct {
	kind   string // provider | type | region | account | tag
	tagKey string // set only when kind == "tag"
}

// parseDimensions splits SheetBy into its "+"-joined dimensions. "" and "none"
// mean a single sheet and return (nil, nil). Any unrecognised or empty part is
// an error, which is how Validate rejects bad input.
func parseDimensions(sheetBy string) ([]dimension, error) {
	if sheetBy == "" || sheetBy == "none" {
		return nil, nil
	}
	parts := strings.Split(sheetBy, "+")
	dims := make([]dimension, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	add := func(d dimension) error {
		id := d.kind + ":" + d.tagKey
		if seen[id] {
			return fmt.Errorf("invalid sheet-by %q: dimension %q appears more than once", sheetBy, id)
		}
		seen[id] = true
		dims = append(dims, d)
		return nil
	}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		switch p {
		case "provider", "type", "region", "account":
			if err := add(dimension{kind: p}); err != nil {
				return nil, err
			}
		case "tag:":
			return nil, fmt.Errorf("invalid sheet-by %q: tag dimension needs a key (e.g. tag:compartment_id)", sheetBy)
		default:
			if strings.HasPrefix(p, "tag:") {
				if err := add(dimension{kind: "tag", tagKey: strings.TrimPrefix(p, "tag:")}); err != nil {
					return nil, err
				}
				continue
			}
			return nil, fmt.Errorf("invalid sheet-by %q (want none|provider|type|region|account|tag:KEY, optionally joined with +)", sheetBy)
		}
	}
	return dims, nil
}

var _ Renderer = (*XLSX)(nil)

// coreXLSXHeaders mirror the CSV core columns (minus the flattened "tags"
// column, which xlsx expands into one column per tag key). "Created At" is the
// 8th column (H) — kept fixed so the date number-format targets it by letter.
var coreXLSXHeaders = []string{
	"Provider", "Account ID", "Region", "Type", "ID", "Name", "Status", "Created At (UTC)",
}

const createdAtCol = "H" // 8th core column

// Validate reports whether SheetBy names supported partition dimension(s).
func (r *XLSX) Validate() error {
	_, err := parseDimensions(r.SheetBy)
	return err
}

func (r *XLSX) Render(ctx context.Context, in <-chan core.Asset, w io.Writer) error {
	if err := r.Validate(); err != nil {
		return err
	}

	assets, err := drain(ctx, in)
	if err != nil {
		return err
	}

	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	sheets := r.groupAssets(assets)
	if len(sheets) == 0 {
		// Always emit a valid, non-empty workbook — even for a zero-asset audit.
		sheets = []assetGroup{{label: "Assets"}}
	}

	// Reserve sheet names up front so the Summary sheet (created first) can
	// hyperlink to each group sheet by its final, sanitized name.
	used := map[string]bool{}
	var summaryName string
	if r.Summary {
		summaryName = sanitizeSheetName("Summary", used)
	}
	groupNames := make([]string, len(sheets))
	for i, g := range sheets {
		groupNames[i] = sanitizeSheetName(g.label, used)
	}

	first := true
	createSheet := func(name string) error {
		if first {
			first = false
			if name != "Sheet1" {
				if err := f.SetSheetName("Sheet1", name); err != nil {
					return fmt.Errorf("name sheet: %w", err)
				}
			}
			return nil
		}
		if _, err := f.NewSheet(name); err != nil {
			return fmt.Errorf("new sheet: %w", err)
		}
		return nil
	}

	if r.Summary {
		if err := createSheet(summaryName); err != nil {
			return err
		}
		if err := r.writeSummarySheet(f, summaryName, sheets, groupNames, len(assets)); err != nil {
			return err
		}
	}
	for i, g := range sheets {
		if err := createSheet(groupNames[i]); err != nil {
			return err
		}
		if err := r.writeSheet(f, groupNames[i], g.assets); err != nil {
			return err
		}
	}

	f.SetActiveSheet(0)
	if err := f.Write(w); err != nil {
		return fmt.Errorf("write xlsx: %w", err)
	}
	return nil
}

type assetGroup struct {
	rawKey string
	label  string
	assets []core.Asset
}

// keySep joins composite dimension values into a single map key. NUL never
// appears in a region, OCID, or tag value, so it can't collide two distinct
// tuples into one group. It's internal only — labels are built separately.
const keySep = "\x00"

// groupAssets partitions assets into worksheet groups per SheetBy, with the
// label resolution and container-placement behaviour described on the type.
func (r *XLSX) groupAssets(assets []core.Asset) []assetGroup {
	idToName := make(map[string]string, len(assets))
	for _, a := range assets {
		if a.ID != "" && a.Name != "" {
			if _, ok := idToName[a.ID]; !ok {
				idToName[a.ID] = a.Name
			}
		}
	}

	dims, _ := parseDimensions(r.SheetBy) // already validated in Render
	if len(dims) == 0 {                   // "" / "none" → a single sheet
		if len(assets) == 0 {
			return nil // let Render emit the placeholder "Assets" sheet
		}
		return []assetGroup{{label: "Assets", assets: assets}}
	}

	// For each tag dimension, the set of values it actually takes — used to
	// detect "container" assets (an asset whose own ID is referenced by the tag
	// but that lacks the tag itself, e.g. the compartment owning the resources).
	referenced := map[string]map[string]bool{}
	for _, d := range dims {
		if d.kind != "tag" || referenced[d.tagKey] != nil {
			continue
		}
		set := map[string]bool{}
		for _, a := range assets {
			if v := a.Tags[d.tagKey]; v != "" {
				set[v] = true
			}
		}
		referenced[d.tagKey] = set
	}

	byKey := map[string]*assetGroup{}
	for _, a := range assets {
		vals := make([]string, len(dims))
		for i, d := range dims {
			vals[i] = dimValue(d, a, referenced)
		}
		k := strings.Join(vals, keySep)
		g, ok := byKey[k]
		if !ok {
			g = &assetGroup{rawKey: k, label: r.compositeLabel(dims, vals, idToName)}
			byKey[k] = g
		}
		g.assets = append(g.assets, a)
	}

	out := make([]assetGroup, 0, len(byKey))
	for _, g := range byKey {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].label != out[j].label {
			return out[i].label < out[j].label
		}
		return out[i].rawKey < out[j].rawKey
	})
	return out
}

// dimValue resolves one asset's raw value for one dimension. For a tag
// dimension, a container asset (no tag, but its own ID is referenced by the
// tag) groups under its own ID so it co-locates with the resources it owns.
func dimValue(d dimension, a core.Asset, referenced map[string]map[string]bool) string {
	switch d.kind {
	case "provider":
		return a.Provider
	case "type":
		return a.Type
	case "region":
		return a.Region
	case "account":
		return a.AccountID
	case "tag":
		v := a.Tags[d.tagKey]
		if v == "" && a.ID != "" && referenced[d.tagKey][a.ID] {
			return a.ID
		}
		return v
	}
	return ""
}

// compositeLabel renders a worksheet label from the resolved dimension values.
// A single dimension is shown as-is (an empty value parenthesised, e.g.
// "(no region)"). Multiple dimensions read "head (rest / ...)" — the head bare,
// the rest slash-joined (" / ") inside one set of parens. Only the head's empty
// value is wrapped in its own parens; an empty tail value stays inside the
// shared parens, so a 2-dimension composite reads "oci (no region)" rather than
// a doubled "oci ((no region))" (with 3+ dims it reads e.g. "oci (no region / x)").
func (r *XLSX) compositeLabel(dims []dimension, vals []string, idToName map[string]string) string {
	if len(dims) == 1 {
		l := dimLabel(dims[0], vals[0], idToName)
		if vals[0] == "" {
			return "(" + l + ")"
		}
		return l
	}
	head := dimLabel(dims[0], vals[0], idToName)
	if vals[0] == "" {
		head = "(" + head + ")"
	}
	tail := make([]string, len(dims)-1)
	for i := 1; i < len(dims); i++ {
		tail[i-1] = dimLabel(dims[i], vals[i], idToName)
	}
	return head + " (" + strings.Join(tail, " / ") + ")"
}

// dimLabel is the display string for one dimension value: an empty value reads
// as "no <dim>" (callers add parens as the position warrants), and a value
// matching an asset ID resolves to that Name.
func dimLabel(d dimension, val string, idToName map[string]string) string {
	if val == "" {
		if d.kind == "tag" {
			return "no " + d.tagKey
		}
		return "no " + d.kind
	}
	if name, ok := idToName[val]; ok && name != "" {
		return name
	}
	return val
}

// writeSheet lays out one worksheet: header (core columns + the union of tag
// keys present), data rows, then styling (bold frozen header, autofilter,
// date format, column widths).
func (r *XLSX) writeSheet(f *excelize.File, sheet string, assets []core.Asset) error {
	// Tag keys used as grouping dimensions are redundant with the sheet itself,
	// so drop them from the per-sheet tag columns.
	exclude := map[string]bool{}
	dims, _ := parseDimensions(r.SheetBy)
	for _, d := range dims {
		if d.kind == "tag" {
			exclude[d.tagKey] = true
		}
	}
	tagKeys := unionTagKeys(assets, exclude)

	ncols := len(coreXLSXHeaders) + len(tagKeys)
	widths := make([]float64, ncols)

	header := make([]interface{}, 0, ncols)
	for _, h := range coreXLSXHeaders {
		header = append(header, h)
	}
	for _, h := range tagHeaderNames(tagKeys) {
		header = append(header, h)
	}
	for i, h := range header {
		widths[i] = float64(len(h.(string)) + 2)
	}
	if err := f.SetSheetRow(sheet, "A1", &header); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	rows := append([]core.Asset(nil), assets...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Type != rows[j].Type {
			return rows[i].Type < rows[j].Type
		}
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].ID < rows[j].ID
	})

	for ri, a := range rows {
		row := make([]interface{}, 0, ncols)
		row = append(row, a.Provider, a.AccountID, a.Region, a.Type, a.ID, a.Name, a.Status)
		if a.CreatedAt != nil {
			row = append(row, a.CreatedAt.UTC())
		} else {
			row = append(row, "")
		}
		for _, k := range tagKeys {
			row = append(row, a.Tags[k])
		}
		for i, v := range row {
			if n := cellLen(v); float64(n) > widths[i] {
				widths[i] = float64(n)
			}
		}
		cell, _ := excelize.CoordinatesToCellName(1, ri+2)
		if err := f.SetSheetRow(sheet, cell, &row); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}

	return styleSheet(f, sheet, ncols, len(rows), widths)
}

// writeSummarySheet lays out the leading "Summary" worksheet: a total, a
// per-sheet breakdown (each group label hyperlinked to its sheet, by the final
// sanitized name in groupNames), and a per-type breakdown. groupNames is
// index-aligned with groups. Output is deterministic: groups are already
// sorted, and types are sorted by descending count then name.
func (r *XLSX) writeSummarySheet(f *excelize.File, sheet string, groups []assetGroup, groupNames []string, total int) error {
	title, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 14}})
	if err != nil {
		return err
	}
	bold, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return err
	}
	link, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Color: "1F4E78", Underline: "single"}})
	if err != nil {
		return err
	}

	var ferr error
	row := 1
	cell := func(col string, n int) string { return fmt.Sprintf("%s%d", col, n) }
	set := func(col string, n int, v interface{}) {
		if ferr == nil {
			ferr = f.SetCellValue(sheet, cell(col, n), v)
		}
	}
	style := func(col string, n int, s int) {
		if ferr == nil {
			ferr = f.SetCellStyle(sheet, cell(col, n), cell(col, n), s)
		}
	}

	set("A", row, "Asset Inventory — Summary")
	style("A", row, title)
	row += 2

	set("A", row, "Total assets")
	style("A", row, bold)
	set("B", row, total)
	row++
	set("A", row, "Worksheets")
	style("A", row, bold)
	set("B", row, len(groups))
	row += 2

	// Per-sheet breakdown, each label linking to its worksheet.
	set("A", row, "By "+dimensionTitle(r.SheetBy))
	style("A", row, bold)
	set("B", row, "Count")
	style("B", row, bold)
	row++
	for i, g := range groups {
		set("A", row, g.label)
		set("B", row, len(g.assets))
		if ferr == nil {
			ferr = f.SetCellHyperLink(sheet, cell("A", row), "'"+groupNames[i]+"'!A1", "Location")
		}
		style("A", row, link)
		row++
	}
	set("A", row, "Total")
	style("A", row, bold)
	set("B", row, total)
	style("B", row, bold)
	row += 2

	// Per-type breakdown across all assets.
	set("A", row, "By Type")
	style("A", row, bold)
	set("B", row, "Count")
	style("B", row, bold)
	row++
	for _, tc := range typeCounts(groups) {
		set("A", row, tc.name)
		set("B", row, tc.count)
		row++
	}

	if ferr != nil {
		return fmt.Errorf("write summary: %w", ferr)
	}
	if err := f.SetColWidth(sheet, "A", "A", 44); err != nil {
		return err
	}
	return f.SetColWidth(sheet, "B", "B", 12)
}

// dimensionTitle is a human label for the SheetBy dimension(s), used in the
// summary's per-sheet section header ("By Namespace", "By Provider", …).
func dimensionTitle(sheetBy string) string {
	dims, err := parseDimensions(sheetBy)
	if err != nil || len(dims) == 0 {
		return "Sheet"
	}
	if len(dims) > 1 {
		return "Group"
	}
	d := dims[0]
	if d.kind == "tag" {
		return prettyHeader(d.tagKey)
	}
	return strings.ToUpper(d.kind[:1]) + d.kind[1:]
}

type typeCount struct {
	name  string
	count int
}

// typeCounts tallies assets by Type across all groups, sorted by descending
// count then name so the summary is deterministic and "biggest first".
func typeCounts(groups []assetGroup) []typeCount {
	counts := map[string]int{}
	for _, g := range groups {
		for _, a := range g.assets {
			counts[a.Type]++
		}
	}
	out := make([]typeCount, 0, len(counts))
	for name, c := range counts {
		out = append(out, typeCount{name: name, count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].name < out[j].name
	})
	return out
}

func styleSheet(f *excelize.File, sheet string, ncols, nrows int, widths []float64) error {
	lastCol, err := excelize.ColumnNumberToName(ncols)
	if err != nil {
		return err
	}

	headStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"1F4E78"}},
		Alignment: &excelize.Alignment{Vertical: "center"},
	})
	if err != nil {
		return err
	}
	if err := f.SetCellStyle(sheet, "A1", lastCol+"1", headStyle); err != nil {
		return err
	}

	if err := f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	}); err != nil {
		return err
	}

	filterRange := "A1:" + lastCol + "1"
	if nrows > 0 {
		filterRange = fmt.Sprintf("A1:%s%d", lastCol, nrows+1)
	}
	if err := f.AutoFilter(sheet, filterRange, nil); err != nil {
		return err
	}

	if nrows > 0 {
		fmtStr := "yyyy-mm-dd hh:mm:ss"
		dateStyle, err := f.NewStyle(&excelize.Style{CustomNumFmt: &fmtStr})
		if err != nil {
			return err
		}
		if err := f.SetCellStyle(sheet, createdAtCol+"2", fmt.Sprintf("%s%d", createdAtCol, nrows+1), dateStyle); err != nil {
			return err
		}
	}

	for i := 0; i < ncols; i++ {
		col, err := excelize.ColumnNumberToName(i + 1)
		if err != nil {
			return err
		}
		w := widths[i] + 2 // a little padding
		if w < 10 {
			w = 10
		}
		if w > 60 {
			w = 60
		}
		if err := f.SetColWidth(sheet, col, col, w); err != nil {
			return err
		}
	}
	return nil
}

// drain reads the whole channel into a slice, honouring context cancellation.
func drain(ctx context.Context, in <-chan core.Asset) ([]core.Asset, error) {
	var assets []core.Asset
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case a, ok := <-in:
			if !ok {
				return assets, nil
			}
			assets = append(assets, a)
		}
	}
}

func unionTagKeys(assets []core.Asset, exclude map[string]bool) []string {
	set := map[string]struct{}{}
	for _, a := range assets {
		for k := range a.Tags {
			if !exclude[k] {
				set[k] = struct{}{}
			}
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// tagHeaderNames returns the display headers for the tag columns: each tag key
// prettified, then disambiguated so no header collides with a core column or
// another tag column. A label key like "name" prettifies to "Name", clashing
// with the core "Name" column; such clashes get a " (tag)" suffix (then
// " (tag 2)", …). Comparison is case-insensitive ("id" vs core "ID"). Data
// extraction still keys on the raw tag keys, so only the header text changes.
// tagKeys arrives sorted, so the output is deterministic.
func tagHeaderNames(tagKeys []string) []string {
	used := make(map[string]bool, len(coreXLSXHeaders)+len(tagKeys))
	for _, h := range coreXLSXHeaders {
		used[strings.ToLower(h)] = true
	}
	out := make([]string, len(tagKeys))
	for i, k := range tagKeys {
		h := prettyHeader(k)
		if used[strings.ToLower(h)] {
			base := h
			h = base + " (tag)"
			for n := 2; used[strings.ToLower(h)]; n++ {
				h = fmt.Sprintf("%s (tag %d)", base, n)
			}
		}
		used[strings.ToLower(h)] = true
		out[i] = h
	}
	return out
}

// prettyHeader turns a snake_case tag key into a Title Case column header
// ("cidr_block" → "Cidr Block"). Tag keys are ASCII, so byte slicing is safe.
func prettyHeader(k string) string {
	parts := strings.Split(k, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

func cellLen(v interface{}) int {
	switch t := v.(type) {
	case string:
		return len(t)
	default:
		return len("2006-01-02 15:04:05") // a formatted timestamp's width
	}
}

// sanitizeSheetName enforces Excel's worksheet-name rules: ≤31 chars, none of
// : \ / ? * [ ], no leading/trailing apostrophe, non-empty, and unique
// (case-insensitively) within the workbook.
func sanitizeSheetName(label string, used map[string]bool) string {
	name := strings.Map(func(r rune) rune {
		switch r {
		case ':', '\\', '/', '?', '*', '[', ']':
			return '_'
		}
		return r
	}, label)
	name = strings.TrimSpace(strings.Trim(name, "'"))
	if name == "" {
		name = "Sheet"
	}
	name = truncRunes(name, 31)

	base := name
	for n := 1; used[strings.ToLower(name)]; n++ {
		suffix := fmt.Sprintf("~%d", n)
		name = truncRunes(base, 31-len(suffix)) + suffix
	}
	used[strings.ToLower(name)] = true
	return name
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
