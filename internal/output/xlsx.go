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
// SheetBy selects how assets are partitioned across worksheets:
//
//	""/"none"  → a single "Assets" sheet
//	"provider" → one sheet per provider
//	"type"     → one sheet per asset type
//	"region"   → one sheet per region
//	"account"  → one sheet per account_id
//	"tag:KEY"  → one sheet per distinct Tags[KEY] value
//
// Two conveniences apply to tag grouping. When a group value matches a known
// asset ID (e.g. tag:compartment_id values are compartment OCIDs), the sheet
// is labelled with that asset's Name instead of the raw ID. And a "container"
// asset that lacks the grouping tag but whose own ID is referenced by it (the
// compartment that owns the resources) is placed in that group's sheet rather
// than a catch-all.
type XLSX struct {
	SheetBy string
}

var _ Renderer = (*XLSX)(nil)

// coreXLSXHeaders mirror the CSV core columns (minus the flattened "tags"
// column, which xlsx expands into one column per tag key). "Created At" is the
// 8th column (H) — kept fixed so the date number-format targets it by letter.
var coreXLSXHeaders = []string{
	"Provider", "Account ID", "Region", "Type", "ID", "Name", "Status", "Created At (UTC)",
}

const createdAtCol = "H" // 8th core column

// Validate reports whether SheetBy names a supported partition dimension.
func (r *XLSX) Validate() error {
	switch r.SheetBy {
	case "", "none", "provider", "type", "region", "account":
		return nil
	}
	if strings.HasPrefix(r.SheetBy, "tag:") && len(r.SheetBy) > len("tag:") {
		return nil
	}
	return fmt.Errorf("invalid sheet-by %q (want none|provider|type|region|account|tag:KEY)", r.SheetBy)
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

	used := map[string]bool{}
	for i, g := range sheets {
		name := sanitizeSheetName(g.label, used)
		if i == 0 {
			if name != "Sheet1" {
				if err := f.SetSheetName("Sheet1", name); err != nil {
					return fmt.Errorf("name sheet: %w", err)
				}
			}
		} else if _, err := f.NewSheet(name); err != nil {
			return fmt.Errorf("new sheet: %w", err)
		}
		if err := r.writeSheet(f, name, g.assets); err != nil {
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

	tagKey := ""
	referenced := map[string]bool{} // values used by the grouping tag
	if strings.HasPrefix(r.SheetBy, "tag:") {
		tagKey = strings.TrimPrefix(r.SheetBy, "tag:")
		for _, a := range assets {
			if v := a.Tags[tagKey]; v != "" {
				referenced[v] = true
			}
		}
	}

	keyOf := func(a core.Asset) string {
		if tagKey != "" {
			v := a.Tags[tagKey]
			if v == "" && a.ID != "" && referenced[a.ID] {
				return a.ID // a container of this group (e.g. the compartment itself)
			}
			return v
		}
		switch r.SheetBy {
		case "provider":
			return a.Provider
		case "type":
			return a.Type
		case "region":
			return a.Region
		case "account":
			return a.AccountID
		default: // "" / "none"
			return "Assets"
		}
	}

	byKey := map[string]*assetGroup{}
	for _, a := range assets {
		k := keyOf(a)
		g, ok := byKey[k]
		if !ok {
			g = &assetGroup{rawKey: k, label: r.labelFor(k, idToName)}
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

func (r *XLSX) labelFor(key string, idToName map[string]string) string {
	if key == "" {
		if strings.HasPrefix(r.SheetBy, "tag:") {
			return "(no " + strings.TrimPrefix(r.SheetBy, "tag:") + ")"
		}
		if r.SheetBy == "" || r.SheetBy == "none" {
			return "Assets"
		}
		return "(no " + r.SheetBy + ")"
	}
	if name, ok := idToName[key]; ok && name != "" {
		return name
	}
	return key
}

// writeSheet lays out one worksheet: header (core columns + the union of tag
// keys present), data rows, then styling (bold frozen header, autofilter,
// date format, column widths).
func (r *XLSX) writeSheet(f *excelize.File, sheet string, assets []core.Asset) error {
	exclude := ""
	if strings.HasPrefix(r.SheetBy, "tag:") {
		exclude = strings.TrimPrefix(r.SheetBy, "tag:") // redundant with the sheet
	}
	tagKeys := unionTagKeys(assets, exclude)

	ncols := len(coreXLSXHeaders) + len(tagKeys)
	widths := make([]float64, ncols)

	header := make([]interface{}, 0, ncols)
	for _, h := range coreXLSXHeaders {
		header = append(header, h)
	}
	for _, k := range tagKeys {
		header = append(header, prettyHeader(k))
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

func unionTagKeys(assets []core.Asset, exclude string) []string {
	set := map[string]struct{}{}
	for _, a := range assets {
		for k := range a.Tags {
			if k != exclude {
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
