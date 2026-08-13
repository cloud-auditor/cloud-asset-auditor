package insight

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/cost"
)

// Renderers for an insight report. They live here rather than in internal/cli
// for the same reason internal/diff's and internal/cost's do: they can be
// unit-tested against an io.Writer, and the cli package has no command test
// harness.
//
// One rule governs all three, and it is the rule the whole feature rests on:
// a finding's Caveat is rendered in the same visual unit as the finding, every
// time, in every format. Never pooled into a "notes" section at the bottom,
// never behind a flag. A caveat printed after the thing it qualifies is a
// footnote, and nobody reads footnotes — least of all the person who screen-
// shots one table out of the report and pastes it into an incident channel.

// pageWidth is the column the counts right-align to, and the width prose wraps
// within. 78 fits an 80-column terminal with room for a quoting "> " prefix,
// which is where half of this output ends up.
const pageWidth = 78

// DefaultMaxRows caps the detail rows the human formats print per finding. A
// finding with 400 rows has a shape, not a list, and printing all of them
// buries every other finding on the page. The JSON carries everything.
const DefaultMaxRows = 12

// RenderOptions configures the human formats.
type RenderOptions struct {
	// MaxRows caps detail rows per finding; 0 means DefaultMaxRows and a
	// negative value means no cap.
	MaxRows int
}

// RenderOption sets a rendering knob.
type RenderOption func(*RenderOptions)

// WithMaxRows caps the detail rows printed per finding. Negative means print
// every row.
func WithMaxRows(n int) RenderOption {
	return func(o *RenderOptions) { o.MaxRows = n }
}

func (o RenderOptions) maxRows() int {
	if o.MaxRows == 0 {
		return DefaultMaxRows
	}
	return o.MaxRows
}

// Render writes a report in the named format.
func Render(rep *Report, format string, w io.Writer, opts ...RenderOption) error {
	var o RenderOptions
	for _, opt := range opts {
		opt(&o)
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "table":
		return renderTable(rep, w, o)
	case "json":
		return renderJSON(rep, w)
	case "markdown", "md":
		return renderMarkdown(rep, w, o)
	default:
		return fmt.Errorf("unknown insight output format %q (want table|json|markdown)", format)
	}
}

// renderJSON writes the machine-readable report: every finding with every row,
// no caps, and the disclaimer as a required top-level field.
func renderJSON(rep *Report, w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// ----------------------------------------------------------------------
// table
// ----------------------------------------------------------------------

func renderTable(rep *Report, w io.Writer, o RenderOptions) error {
	bw := bufio.NewWriter(w)

	fmt.Fprintf(bw, "INSIGHTS — %s\n", rep.GeneratedAt.UTC().Format("2006-01-02T15:04:05Z"))
	fmt.Fprintf(bw, "%s\n\n", scopeLine(rep.Scope))

	// The disclaimer is boxed so it is a visual peer of the tables rather than
	// prose above them, and it comes before the first number on the page.
	writeBox(bw, Disclaimer)
	fmt.Fprintln(bw)

	for _, n := range rep.Notes {
		for i, line := range wrapText(n, pageWidth-8) {
			if i == 0 {
				fmt.Fprintf(bw, "  NOTE  %s\n", line)
				continue
			}
			fmt.Fprintf(bw, "        %s\n", line)
		}
		fmt.Fprintln(bw)
	}

	if len(rep.Findings) == 0 {
		fmt.Fprintln(bw, "No findings.")
		fmt.Fprintln(bw)
	}

	for _, sec := range rep.Families() {
		writeSectionHead(bw, sec)
		for _, f := range sec.Findings {
			writeFinding(bw, f, o)
		}
	}

	writeSkipped(bw, rep)
	writeSuppressed(bw, rep)

	if !rep.Complete {
		fmt.Fprintln(bw, "This report is PARTIAL — the run was cancelled before every insight had gone.")
	}
	if rep.Hidden > 0 {
		fmt.Fprintf(bw, "%d finding%s hidden by the severity filter.\n", rep.Hidden, plural(rep.Hidden))
	}
	return bw.Flush()
}

func writeSectionHead(bw *bufio.Writer, sec Section) {
	title := sec.Family.Title()
	count := fmt.Sprintf("%d finding%s", len(sec.Findings), plural(len(sec.Findings)))
	rule := pageWidth - runeLen(title) - runeLen(count) - 2
	if rule < 1 {
		rule = 1
	}
	fmt.Fprintf(bw, "%s %s %s\n\n", title, strings.Repeat("─", rule), count)
}

// writeFinding lays out one finding: the headline with its count right-aligned
// on the page's edge, then the summary, then the two lines that qualify it,
// then the detail.
//
// The order is deliberate. Basis and cannot-know sit *above* the rows, so a
// reader meets the qualification before the list of names they are about to go
// and act on.
func writeFinding(bw *bufio.Writer, f Finding, o RenderOptions) {
	const indent = "     " // aligns under the severity word

	// Headline: mark, severity word, title, dot leader, count. The dot leader
	// is what makes a column of counts scannable when the titles differ in
	// length by 30 characters.
	head := fmt.Sprintf("  %s %-7s ", f.Severity.Mark(), string(f.Severity))
	count := formatInt(f.Count)
	lead := pageWidth - runeLen(head) - runeLen(f.Title) - runeLen(count) - 2
	if lead < 1 {
		fmt.Fprintf(bw, "%s%s  %s\n", head, f.Title, count)
	} else {
		fmt.Fprintf(bw, "%s%s %s %s\n", head, f.Title, strings.Repeat(".", lead), count)
	}

	for _, line := range wrapText(f.Summary, pageWidth-len(indent)) {
		fmt.Fprintf(bw, "%s%s\n", indent, line)
	}
	fmt.Fprintln(bw)

	writeQualifier(bw, indent, "basis", f.Basis)
	writeQualifier(bw, indent, "cannot know", f.Caveat)
	fmt.Fprintln(bw)

	writeRows(bw, f, o)
	fmt.Fprintf(bw, "%sid: %s\n\n", indent, f.ID)
}

// writeQualifier prints one labelled paragraph with a hanging indent, so the
// wrapped continuation lines stay inside the label's column and the two
// qualifiers read as a block rather than as prose.
func writeQualifier(bw *bufio.Writer, indent, label, text string) {
	const labelWidth = 13
	hang := indent + strings.Repeat(" ", labelWidth)
	lines := wrapText(text, pageWidth-len(hang))
	for i, line := range lines {
		if i == 0 {
			fmt.Fprintf(bw, "%s%-*s%s\n", indent, labelWidth, label, line)
			continue
		}
		fmt.Fprintf(bw, "%s%s\n", hang, line)
	}
}

// writeRows prints the detail table. Columns that no row fills are dropped
// entirely — a header over an empty column is noise, and every insight fills a
// different subset.
func writeRows(bw *bufio.Writer, f Finding, o RenderOptions) {
	if len(f.Rows) == 0 && f.Total == nil {
		return
	}
	const indent = "     "

	rows := f.Rows
	capped := 0
	if maxRows := o.maxRows(); maxRows > 0 && len(rows) > maxRows {
		capped = len(rows) - maxRows
		rows = rows[:maxRows]
	}

	var hasFact, hasValue, hasMoney, allAssets bool
	allAssets = len(rows) > 0
	for _, r := range rows {
		hasFact = hasFact || r.Fact != "" || len(r.Related) > 0
		hasValue = hasValue || r.Value != ""
		hasMoney = hasMoney || r.Money != nil
		allAssets = allAssets && r.Asset != nil
	}

	subject := "subject"
	if allAssets {
		subject = "asset"
	}
	t := &textTable{}
	t.head = append(t.head, subject)
	t.right = append(t.right, false)
	if hasValue {
		t.head = append(t.head, "value")
		t.right = append(t.right, true)
	}
	if hasMoney {
		t.head = append(t.head, "monthly")
		t.right = append(t.right, true)
	}
	if hasFact {
		t.head = append(t.head, "detail")
		t.right = append(t.right, false)
	}

	cells := func(label, value, money, fact string) []string {
		out := []string{label}
		if hasValue {
			out = append(out, value)
		}
		if hasMoney {
			out = append(out, money)
		}
		if hasFact {
			out = append(out, fact)
		}
		return out
	}
	for _, r := range rows {
		t.add(cells(truncate(r.Label, 40), r.Value, moneyCell(r.Money), truncate(factCell(r), 44))...)
	}

	// The total goes inside the table, under a rule, so it lines up on the
	// decimal point with the rows it totals. A total printed beside the table
	// is a number the eye has to re-find.
	if f.Total != nil && hasMoney {
		t.ruleBefore = len(t.rows)
		t.add(cells("TOTAL", "", f.Total.String(), "")...)
	}
	t.render(bw, indent)

	if capped > 0 {
		fmt.Fprintf(bw, "%s… and %s more — the full list is in -o json.\n", indent, formatInt(capped))
	}
	if f.Total != nil {
		if !hasMoney {
			fmt.Fprintf(bw, "%sTOTAL  %s\n", indent, f.Total.String())
		}
		// The money line carries internal/cost's short disclaimer rather than a
		// second sentence written here. One copy of that text exists in this
		// repo and this is not it.
		for _, line := range wrapText(cost.DisclaimerShort, pageWidth-len(indent)-2) {
			fmt.Fprintf(bw, "%s  %s\n", indent, line)
		}
	}
	fmt.Fprintln(bw)
}

func writeSkipped(bw *bufio.Writer, rep *Report) {
	if len(rep.Skipped) == 0 {
		return
	}
	// Printed as its own section, not as a debug aside: an insight that did not
	// run is the single likeliest reason a section a reader expected is
	// missing, and "nothing found" and "never looked" must not look alike.
	fmt.Fprintf(bw, "NOT RUN %s %d\n\n", strings.Repeat("─", pageWidth-10-len(formatInt(len(rep.Skipped)))), len(rep.Skipped))
	t := &textTable{head: []string{"insight", "why it could not answer"}}
	for _, s := range rep.Skipped {
		t.add(s.Insight, s.Reason)
	}
	t.render(bw, "  ")
	fmt.Fprintln(bw)
}

func writeSuppressed(bw *bufio.Writer, rep *Report) {
	if len(rep.Suppressed) == 0 {
		return
	}
	fmt.Fprintf(bw, "REFUSED %s %d\n", strings.Repeat("─", pageWidth-10-len(formatInt(len(rep.Suppressed)))), len(rep.Suppressed))
	fmt.Fprintln(bw, "  These findings were produced but not published, because they do not meet the")
	fmt.Fprintln(bw, "  contract every finding in this tool must meet. This is a bug in the insight,")
	fmt.Fprintln(bw, "  not a property of your estate — please report it.")
	fmt.Fprintln(bw)
	t := &textTable{head: []string{"insight", "finding", "reason"}}
	for _, s := range rep.Suppressed {
		t.add(s.Insight, s.Finding, s.Reason)
	}
	t.render(bw, "  ")
	fmt.Fprintln(bw)
}

// ----------------------------------------------------------------------
// markdown
// ----------------------------------------------------------------------

func renderMarkdown(rep *Report, w io.Writer, o RenderOptions) error {
	bw := bufio.NewWriter(w)

	fmt.Fprintln(bw, "## Insights")
	fmt.Fprintf(bw, "\n_%s — %s_\n", rep.GeneratedAt.UTC().Format("2006-01-02T15:04:05Z"), scopeLine(rep.Scope))

	// A blockquote above the first finding, not a footer.
	fmt.Fprintln(bw)
	for _, line := range strings.Split(Disclaimer, "\n") {
		fmt.Fprintf(bw, "> %s\n", line)
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(bw, "\n> **Note** — %s\n", escapePipes(n))
	}

	if len(rep.Findings) == 0 {
		fmt.Fprintln(bw, "\nNo findings.")
	}

	for _, sec := range rep.Families() {
		fmt.Fprintf(bw, "\n### %s\n", sec.Family.Title())
		for _, f := range sec.Findings {
			markdownFinding(bw, f, o)
		}
	}

	if len(rep.Skipped) > 0 {
		fmt.Fprint(bw, "\n### Not run\n\n")
		fmt.Fprintln(bw, "| insight | why it could not answer |")
		fmt.Fprintln(bw, "| --- | --- |")
		for _, s := range rep.Skipped {
			fmt.Fprintf(bw, "| `%s` | %s |\n", s.Insight, escapePipes(s.Reason))
		}
	}
	if len(rep.Suppressed) > 0 {
		fmt.Fprint(bw, "\n### Refused\n\n")
		fmt.Fprintln(bw, "These findings were produced but not published: they do not meet the contract "+
			"every finding must meet. That is a bug in the insight, not a property of the estate.")
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "| insight | finding | reason |")
		fmt.Fprintln(bw, "| --- | --- | --- |")
		for _, s := range rep.Suppressed {
			fmt.Fprintf(bw, "| `%s` | `%s` | %s |\n", s.Insight, s.Finding, escapePipes(s.Reason))
		}
	}
	if !rep.Complete {
		fmt.Fprintln(bw, "\n> This report is **partial** — the run was cancelled before every insight had gone.")
	}
	if rep.Hidden > 0 {
		fmt.Fprintf(bw, "\n_%d finding%s hidden by the severity filter._\n", rep.Hidden, plural(rep.Hidden))
	}
	return bw.Flush()
}

func markdownFinding(bw *bufio.Writer, f Finding, o RenderOptions) {
	fmt.Fprintf(bw, "\n#### %s %s — %s (%s)\n", f.Severity.Mark(), f.Severity, escapePipes(f.Title), formatInt(f.Count))
	fmt.Fprintf(bw, "\n%s\n", escapePipes(f.Summary))
	fmt.Fprintf(bw, "\n- **Basis** — %s\n", escapePipes(f.Basis))
	// The caveat is a blockquote so it survives being skimmed, and it sits
	// above the table for the same reason it does in the table renderer.
	fmt.Fprintf(bw, "\n> **Cannot know** — %s\n", escapePipes(f.Caveat))

	rows := f.Rows
	capped := 0
	if maxRows := o.maxRows(); maxRows > 0 && len(rows) > maxRows {
		capped = len(rows) - maxRows
		rows = rows[:maxRows]
	}
	if len(rows) > 0 {
		fmt.Fprintln(bw, "\n| subject | value | monthly | detail |")
		fmt.Fprintln(bw, "| --- | ---: | ---: | --- |")
		for _, r := range rows {
			fmt.Fprintf(bw, "| %s | %s | %s | %s |\n",
				escapePipes(r.Label), escapePipes(r.Value), moneyCell(r.Money), escapePipes(factCell(r)))
		}
	}
	if capped > 0 {
		fmt.Fprintf(bw, "\n_… and %s more — the full list is in `-o json`._\n", formatInt(capped))
	}
	if f.Total != nil {
		fmt.Fprintf(bw, "\n**Total: %s** — %s\n", f.Total.String(), cost.DisclaimerShort)
	}
	fmt.Fprintf(bw, "\n`%s`\n", f.ID)
}

// ----------------------------------------------------------------------
// shared bits
// ----------------------------------------------------------------------

// scopeLine is the one-line header describing what the report was computed
// over. It is not decoration: "3 assets from 1 provider" explains an empty
// report faster than any individual finding's caveat can.
func scopeLine(s Scope) string {
	parts := []string{
		fmt.Sprintf("%s assets", formatInt(s.Assets)),
		fmt.Sprintf("%s types", formatInt(s.Types)),
		fmt.Sprintf("%s inferred edges", formatInt(s.Edges)),
	}
	if len(s.Providers) > 0 {
		parts = append(parts, strings.Join(s.Providers, "+"))
	}
	if s.RawAvailable() {
		parts = append(parts, fmt.Sprintf("raw on %s", formatInt(s.RawAssets)))
	} else {
		parts = append(parts, "no raw payloads")
	}
	if s.Priced {
		parts = append(parts, "cost on")
	}
	return strings.Join(parts, " · ")
}

// factCell folds a row's related assets into the detail column as a count.
// The ids themselves are in the JSON: a table that printed three OCIDs per row
// would be unreadable, but a reader who cannot tell that a row has more behind
// it will never go and look.
func factCell(r Row) string {
	if n := len(r.Related); n > 0 {
		if r.Fact == "" {
			return fmt.Sprintf("+%d related", n)
		}
		return fmt.Sprintf("%s (+%d related)", r.Fact, n)
	}
	return r.Fact
}

// moneyCell renders a row's money, or an em dash. Never "0.00" — see
// cost.NoMoney for why a zero and an absence must not look alike.
func moneyCell(m *Money) string {
	if m == nil {
		return ""
	}
	return m.String()
}

// textTable lays out fixed-width columns with per-column alignment. The same
// shape as internal/cost's, and hand-rolled for the same reason: tabwriter
// cannot right-align one column and left-align another in a single block, and
// a column of counts that does not line up is a column nobody scans.
type textTable struct {
	head  []string
	right []bool
	rows  [][]string
	// ruleBefore inserts a horizontal rule before this row index, separating a
	// TOTAL from the rows it totals. Zero — the zero value — means no rule,
	// which is right: a rule above the first row would separate nothing.
	ruleBefore int
}

func (t *textTable) add(cells ...string) { t.rows = append(t.rows, cells) }

func (t *textTable) render(bw *bufio.Writer, indent string) {
	if len(t.rows) == 0 {
		return
	}
	cols := len(t.head)
	for _, r := range t.rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	width := make([]int, cols)
	measure := func(r []string) {
		for i, c := range r {
			if n := runeLen(c); i < len(width) && n > width[i] {
				width[i] = n
			}
		}
	}
	measure(t.head)
	for _, r := range t.rows {
		measure(r)
	}

	line := func(cells []string) {
		var b strings.Builder
		b.WriteString(indent)
		for i := range width {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			pad := strings.Repeat(" ", width[i]-runeLen(cell))
			if i < len(t.right) && t.right[i] {
				b.WriteString(pad + cell)
			} else {
				b.WriteString(cell + pad)
			}
			if i < len(width)-1 {
				b.WriteString("  ")
			}
		}
		// Trailing padding is invisible here and very visible in a diff or a
		// paste into a code block.
		fmt.Fprintln(bw, strings.TrimRight(b.String(), " "))
	}

	if len(t.head) > 0 {
		line(t.head)
	}
	for i, r := range t.rows {
		if t.ruleBefore > 0 && i == t.ruleBefore {
			total := 0
			for _, x := range width {
				total += x + 2
			}
			fmt.Fprintln(bw, indent+strings.Repeat("─", total-2))
		}
		line(r)
	}
}

// writeBox draws text inside a border, making the disclaimer a visual peer of
// the tables rather than prose to be skipped over.
func writeBox(bw *bufio.Writer, text string) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	width := 0
	for _, l := range lines {
		if n := runeLen(l); n > width {
			width = n
		}
	}
	fmt.Fprintln(bw, "  ┌─"+strings.Repeat("─", width)+"─┐")
	for _, l := range lines {
		fmt.Fprintf(bw, "  │ %s%s │\n", l, strings.Repeat(" ", width-runeLen(l)))
	}
	fmt.Fprintln(bw, "  └─"+strings.Repeat("─", width)+"─┘")
}

// wrapText greedily wraps a paragraph to width columns. The caveats are the
// one part of this output that is prose rather than data, and prose printed as
// one 400-column line is prose nobody reads.
func wrapText(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	if width < 8 {
		width = 8
	}
	var (
		out  []string
		line = words[0]
	)
	for _, word := range words[1:] {
		if runeLen(line)+1+runeLen(word) > width {
			out = append(out, line)
			line = word
			continue
		}
		line += " " + word
	}
	return append(out, line)
}

func runeLen(s string) int { return len([]rune(s)) }

// formatInt groups thousands. A five-figure asset count is common and "17432"
// is a number people misread.
func formatInt(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n || n < 2 {
		return s
	}
	return string(r[:n-1]) + "…"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func escapePipes(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
