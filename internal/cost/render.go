package cost

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/pricing"
)

// Renderers for `auditor cost`. They live here rather than in internal/cli for
// the same reason internal/diff's do: they can be unit-tested against an
// io.Writer, and the cli package has no command test harness.
//
// One rule governs all four. Every total carries its caveat in the same visual
// unit as the number — a box above the table, a short line under the TOTAL row,
// a column on every CSV row, a required JSON field. Never a footnote: a
// shareable artifact travels without its author, and nobody scrolls to a
// footer.

// Render writes a report in the named format.
func Render(rep *Report, format string, w io.Writer) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "table":
		return renderTable(rep, w)
	case "json":
		return renderJSON(rep, w)
	case "csv":
		return renderCSV(rep, w)
	case "markdown", "md":
		return renderMarkdown(rep, w)
	default:
		return fmt.Errorf("unknown cost output format %q (want table|json|csv|markdown)", format)
	}
}

// renderJSON writes the machine-readable report. Every money value in it is a
// string, and disclaimer is a required top-level field — a client that wants to
// do arithmetic has to parse past a tilde to get there, and that friction is
// the point.
func renderJSON(rep *Report, w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func renderTable(rep *Report, w io.Writer) error {
	bw := bufio.NewWriter(w)

	fmt.Fprintf(bw, "Cost estimate — %s\n", rep.GeneratedAt.UTC().Format("2006-01-02T15:04:05Z"))
	if len(rep.Books) > 0 {
		fmt.Fprintf(bw, "  price book: %s\n", bookLine(rep.Books))
	}
	if rep.HoursPerMonth > 0 {
		fmt.Fprintf(bw, "  a month is %g hours; every rate is the marginal one\n", rep.HoursPerMonth)
	}
	fmt.Fprintln(bw)
	writeBox(bw, Disclaimer)
	fmt.Fprintln(bw)

	writeGroupTable(bw, rep)
	writeTopTable(bw, rep)
	writeUnpricedTable(bw, rep)
	writeMeshTable(bw, rep)
	writeKubeTable(bw, rep)

	for _, n := range rep.Notes {
		fmt.Fprintf(bw, "NOTE: %s\n", n)
	}
	return bw.Flush()
}

func writeGroupTable(bw *bufio.Writer, rep *Report) {
	fmt.Fprintf(bw, "BY %s\n", strings.ToUpper(rep.GroupBy))
	t := &textTable{
		head:  []string{rep.GroupBy, "assets", "priced", "metered", "unknown", "attributed", "monthly"},
		right: []bool{false, true, true, true, true, true, true},
	}
	for _, g := range rep.Groups {
		t.add(g.Key, formatInt(g.Assets), formatInt(g.Priced), formatInt(g.Metered),
			formatInt(g.Unknown), formatInt(g.Attributed), moneyLine(g.Monthly))
	}
	t.ruleBefore = len(t.rows)
	tt := rep.Totals
	t.add("TOTAL", formatInt(tt.Assets), formatInt(tt.Priced), formatInt(tt.Metered),
		formatInt(tt.Unknown), formatInt(tt.Attributed), moneyLine(tt.Monthly))
	t.render(bw, "  ")

	// The denominator sits directly beneath the number, always. A total shown
	// without its coverage is the failure mode this whole report is built around.
	if n := tt.UnpricedCount(); n > 0 {
		fmt.Fprintf(bw, "\n  %s of %s assets (%.0f%%) contribute no money to that total. That is not $0 — see NOT PRICED.\n",
			formatInt(n), formatInt(tt.Assets), tt.UnpricedPct())
	}
	fmt.Fprintf(bw, "  %s\n\n", DisclaimerShort)
}

func writeTopTable(bw *bufio.Writer, rep *Report) {
	if len(rep.Top) == 0 {
		return
	}
	fmt.Fprintf(bw, "TOP %d BY MONTHLY COST\n", len(rep.Top))
	t := &textTable{
		head:  []string{"monthly", "basis", "provider", "type", "name"},
		right: []bool{true, false, false, false, false},
	}
	for _, a := range rep.Top {
		t.add(withCurrency(a), basisMark(a.Basis), a.Provider, a.Type, displayName(a))
	}
	t.render(bw, "  ")
	fmt.Fprintf(bw, "  ? marks an assumed size: the quantity is a price-book default, not something the resource reported.\n\n")
}

func writeUnpricedTable(bw *bufio.Writer, rep *Report) {
	u := rep.Unpriced
	if u.Assets() == 0 {
		return
	}
	fmt.Fprintln(bw, "NOT PRICED")
	writeUnpricedBucket(bw, "metered", u.Metered,
		"billing is consumption-based; an inventory cannot see consumption")
	writeUnpricedBucket(bw, "unknown", u.Unknown,
		"no rule matched — a gap in the price book, not a free resource")
	writeUnpricedBucket(bw, "attributed", u.Attributed,
		"a share of another asset's cost, not spend of its own; never added to the total")
	fmt.Fprintln(bw)
}

func writeUnpricedBucket(bw *bufio.Writer, name string, b UnpricedBucket, why string) {
	if b.Assets == 0 {
		return
	}
	fmt.Fprintf(bw, "  %s (%s)  %s\n", name, formatInt(b.Assets), why)
	t := &textTable{right: []bool{false, true, false}}
	for _, ut := range b.Types {
		t.add(ut.Type, formatInt(ut.Assets), truncate(ut.Reason, 96))
		for _, ex := range ut.Examples {
			t.add("    "+displayName(ex), "", truncate(ex.Detail, 96))
		}
	}
	t.render(bw, "    ")
}

func writeMeshTable(bw *bufio.Writer, rep *Report) {
	if len(rep.Mesh) == 0 {
		return
	}
	fmt.Fprintln(bw, "MESH SEATS  (per-seat money is account-wide; it is NOT part of the totals above)")
	for _, m := range rep.Mesh {
		fmt.Fprintf(bw, "  %s (account: %s)\n", m.Provider, orNone(m.Account))
		t := &textTable{right: []bool{false, true, false}}
		for _, c := range m.Counts {
			t.add(c.Label, formatInt(c.Assets), "")
		}
		for _, p := range m.Plans {
			t.add("at "+p.Plan, meshMoney(p), p.Detail)
		}
		t.render(bw, "    ")
		for _, n := range m.Notes {
			fmt.Fprintf(bw, "    - %s\n", n)
		}
	}
	fmt.Fprintf(bw, "  %s\n\n", DisclaimerShort)
}

func writeKubeTable(bw *bufio.Writer, rep *Report) {
	k := rep.Kubernetes
	if k == nil || len(k.Clusters) == 0 {
		return
	}
	fmt.Fprintln(bw, "KUBERNETES")
	fmt.Fprintf(bw, "  %s\n", k.Note)
	for _, c := range k.Clusters {
		fmt.Fprintf(bw, "\n  cluster %s\n", orNone(c.Cluster))
		t := &textTable{right: []bool{false, true, true, false}}
		t.add("nodes", formatInt(c.Nodes), c.NodeMonthly.String(), rateSourceLine(c.RateSources))
		if c.CountedElsewhere.Amount > 0 {
			// Reported rather than netted off: the money is real, it is simply
			// already in another provider's row, and hiding that would make the
			// two sections irreconcilable.
			t.add("  counted elsewhere", "", c.CountedElsewhere.String(),
				"the machine is in this audit under its own provider")
		}
		t.add("pods attributed", formatInt(c.PodsAttributed), c.Attributed.String(),
			fmt.Sprintf("from resources.requests, over %s node%s",
				formatInt(c.AttributableNodes), plural(c.AttributableNodes)))
		if c.PodsSkipped > 0 {
			t.add("pods not attributed", formatInt(c.PodsSkipped), "—", "see NOT PRICED for why")
		}
		if c.AttributableNodes > 0 {
			t.add("unrequested", "", c.Unrequested.String(),
				fmt.Sprintf("%.0f%% of schedulable capacity has no pod request against it", c.UnrequestedPct))
		}
		t.render(bw, "    ")

		if len(c.TopPods) > 0 {
			fmt.Fprintln(bw, "    top pods by attributed cost")
			pt := &textTable{right: []bool{true, false, false}}
			for _, p := range c.TopPods {
				pt.add(Estimated{Currency: p.Currency, Amount: p.amount}.String(), basisMark(p.Basis), displayName(p))
			}
			pt.render(bw, "      ")
		}
	}
	fmt.Fprintf(bw, "  %s\n\n", DisclaimerShort)
}

func renderMarkdown(rep *Report, w io.Writer) error {
	bw := bufio.NewWriter(w)

	fmt.Fprintln(bw, "## Cost estimate")
	fmt.Fprintf(bw, "\n_%s_\n", rep.GeneratedAt.UTC().Format("2006-01-02T15:04:05Z"))
	if len(rep.Books) > 0 {
		fmt.Fprintf(bw, "\nPrice book: `%s`\n", bookLine(rep.Books))
	}
	// A blockquote above the first figure, not a footer.
	fmt.Fprintln(bw)
	for _, line := range strings.Split(Disclaimer, "\n") {
		fmt.Fprintf(bw, "> %s\n", line)
	}

	fmt.Fprintf(bw, "\n### By %s\n\n", rep.GroupBy)
	fmt.Fprintf(bw, "| %s | assets | priced | metered | unknown | attributed | monthly |\n", rep.GroupBy)
	fmt.Fprintln(bw, "| --- | ---: | ---: | ---: | ---: | ---: | ---: |")
	for _, g := range rep.Groups {
		fmt.Fprintf(bw, "| %s | %s | %s | %s | %s | %s | %s |\n", escapePipes(g.Key),
			formatInt(g.Assets), formatInt(g.Priced), formatInt(g.Metered),
			formatInt(g.Unknown), formatInt(g.Attributed), moneyLine(g.Monthly))
	}
	tt := rep.Totals
	fmt.Fprintf(bw, "| **TOTAL** | **%s** | **%s** | **%s** | **%s** | **%s** | **%s** |\n",
		formatInt(tt.Assets), formatInt(tt.Priced), formatInt(tt.Metered),
		formatInt(tt.Unknown), formatInt(tt.Attributed), moneyLine(tt.Monthly))
	if n := tt.UnpricedCount(); n > 0 {
		fmt.Fprintf(bw, "\n%s of %s assets (%.0f%%) contribute no money to that total. That is not $0 — see **Not priced**.\n",
			formatInt(n), formatInt(tt.Assets), tt.UnpricedPct())
	}
	fmt.Fprintf(bw, "\n%s\n", DisclaimerShort)

	if len(rep.Top) > 0 {
		fmt.Fprintf(bw, "\n### Top %d by monthly cost\n\n", len(rep.Top))
		fmt.Fprintln(bw, "| monthly | basis | provider | type | name |")
		fmt.Fprintln(bw, "| ---: | --- | --- | --- | --- |")
		for _, a := range rep.Top {
			fmt.Fprintf(bw, "| %s | %s | %s | `%s` | %s |\n",
				withCurrency(a), basisMark(a.Basis), a.Provider, a.Type, escapePipes(displayName(a)))
		}
	}

	if rep.Unpriced.Assets() > 0 {
		fmt.Fprint(bw, "\n### Not priced\n\n")
		markdownBucket(bw, "metered", rep.Unpriced.Metered)
		markdownBucket(bw, "unknown", rep.Unpriced.Unknown)
		markdownBucket(bw, "attributed", rep.Unpriced.Attributed)
	}

	for _, m := range rep.Mesh {
		fmt.Fprintf(bw, "\n### %s seats (account `%s`)\n\n", m.Provider, orNone(m.Account))
		for _, c := range m.Counts {
			fmt.Fprintf(bw, "- %s: **%s**\n", escapePipes(c.Label), formatInt(c.Assets))
		}
		for _, p := range m.Plans {
			fmt.Fprintf(bw, "- at %s: **%s** — %s\n", p.Plan, meshMoney(p), escapePipes(p.Detail))
		}
		for _, n := range m.Notes {
			fmt.Fprintf(bw, "  - %s\n", escapePipes(n))
		}
		fmt.Fprintf(bw, "\n%s\n", DisclaimerShort)
	}

	if k := rep.Kubernetes; k != nil {
		fmt.Fprint(bw, "\n### Kubernetes\n\n")
		fmt.Fprintf(bw, "> %s\n\n", k.Note)
		fmt.Fprintln(bw, "| cluster | nodes | node monthly | pods attributed | attributed | unrequested |")
		fmt.Fprintln(bw, "| --- | ---: | ---: | ---: | ---: | ---: |")
		for _, c := range k.Clusters {
			fmt.Fprintf(bw, "| %s | %s | %s | %s | %s | %s (%.0f%%) |\n",
				escapePipes(orNone(c.Cluster)), formatInt(c.Nodes), c.NodeMonthly.String(),
				formatInt(c.PodsAttributed), c.Attributed.String(), c.Unrequested.String(), c.UnrequestedPct)
		}
		fmt.Fprintf(bw, "\n%s\n", DisclaimerShort)
	}

	for _, n := range rep.Notes {
		fmt.Fprintf(bw, "\n> NOTE: %s\n", escapePipes(n))
	}
	return bw.Flush()
}

func markdownBucket(bw *bufio.Writer, name string, b UnpricedBucket) {
	if b.Assets == 0 {
		return
	}
	fmt.Fprintf(bw, "**%s (%s)**\n\n", name, formatInt(b.Assets))
	fmt.Fprintln(bw, "| type | assets | reason |")
	fmt.Fprintln(bw, "| --- | ---: | --- |")
	for _, ut := range b.Types {
		fmt.Fprintf(bw, "| `%s` | %s | %s |\n", ut.Type, formatInt(ut.Assets), escapePipes(truncate(ut.Reason, 160)))
	}
	fmt.Fprintln(bw)
}

// renderCSV writes one row per asset plus a TOTAL row per currency half.
//
// The caveat is a column rather than a header comment: a comment line breaks
// every CSV parser, and a spreadsheet's first act is to sort, which strips a
// header line from the row it was meant to qualify. A repeated column survives
// sorting, filtering, and a paste into a second sheet.
func renderCSV(rep *Report, w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"provider", "account_id", "region", "type", "id", "name",
		"monthly", "currency", "basis", "detail", "caveat",
	}); err != nil {
		return err
	}
	for _, a := range rep.Assets {
		if err := cw.Write([]string{
			a.Provider, a.AccountID, a.Region, a.Type, a.ID, a.Name,
			a.Monthly, a.Currency, string(a.Basis), a.Detail, DisclaimerShort,
		}); err != nil {
			return err
		}
	}
	// Totals last, and split by measured/estimated so the monthly column stays
	// homogeneous — a reader who sums it gets the same answer either way, and a
	// reader who filters on basis gets the halves separately.
	for _, m := range rep.Totals.Monthly {
		coverage := fmt.Sprintf("%d of %d assets priced; %d metered, %d unknown, %d attributed",
			rep.Totals.Priced, rep.Totals.Assets, rep.Totals.Metered, rep.Totals.Unknown, rep.Totals.Attributed)
		if m.Measured > 0 {
			if err := cw.Write([]string{"TOTAL", "", "", "", "", "measured",
				formatAmount(m.Measured), m.Currency, string(BasisMeasured), coverage, DisclaimerShort}); err != nil {
				return err
			}
		}
		if m.Estimated > 0 {
			if err := cw.Write([]string{"TOTAL", "", "", "", "", "estimated",
				EstimateMark + formatAmount(m.Estimated), m.Currency, "", coverage, DisclaimerShort}); err != nil {
				return err
			}
		}
	}
	cw.Flush()
	return cw.Error()
}

// textTable lays out fixed-width columns. tabwriter cannot right-align some
// columns and left-align others in one block, and money that does not line up
// on the decimal point is money nobody scans.
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
			if n := len([]rune(c)); n > width[i] {
				width[i] = n
			}
		}
	}
	if len(t.head) > 0 {
		measure(t.head)
	}
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
			pad := width[i] - len([]rune(cell))
			if i < len(t.right) && t.right[i] {
				b.WriteString(strings.Repeat(" ", pad) + cell)
			} else {
				b.WriteString(cell + strings.Repeat(" ", pad))
			}
			if i < len(width)-1 {
				b.WriteString("  ")
			}
		}
		// Trailing padding on a row whose last cells are empty is invisible
		// here and very visible in a diff or a paste.
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

// writeBox draws the disclaimer inside a border. The border is the point: it
// makes the caveat a visual peer of the table rather than prose above it.
func writeBox(bw *bufio.Writer, text string) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	width := 0
	for _, l := range lines {
		if n := len([]rune(l)); n > width {
			width = n
		}
	}
	fmt.Fprintln(bw, "  ┌─"+strings.Repeat("─", width)+"─┐")
	for _, l := range lines {
		fmt.Fprintf(bw, "  │ %s%s │\n", l, strings.Repeat(" ", width-len([]rune(l))))
	}
	fmt.Fprintln(bw, "  └─"+strings.Repeat("─", width)+"─┘")
}

func bookLine(books []pricing.Source) string {
	parts := make([]string, 0, len(books))
	for _, b := range books {
		parts = append(parts, b.ID+"@"+b.Vintage)
	}
	return strings.Join(parts, ", ")
}

func moneyLine(ms []Money) string {
	if len(ms) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, m.String())
	}
	return strings.Join(parts, " / ")
}

// withCurrency prefixes a per-asset figure with its currency symbol, keeping
// the ~ outermost so the glyph is the first thing read.
func withCurrency(a AssetCost) string {
	sym := currencySymbol(a.Currency)
	if sym == "" {
		return a.Monthly
	}
	if rest, ok := strings.CutPrefix(a.Monthly, EstimateMark); ok {
		return EstimateMark + sym + rest
	}
	if _, err := strconv.ParseFloat(a.Monthly, 64); err != nil {
		return a.Monthly // "unknown" / "metered" / "attributed" take no symbol
	}
	return sym + a.Monthly
}

// basisMark appends the ? that marks an assumed quantity. The ~ already says
// "estimated"; this second glyph says "and we guessed the size", which is a
// materially weaker claim and deserves to be visible in the same row.
func basisMark(b Basis) string {
	if b == BasisAssumed {
		return string(b) + " ?"
	}
	return string(b)
}

func displayName(a AssetCost) string {
	if a.Name != "" {
		return a.Name
	}
	return a.ID
}

// meshMoney puts the currency symbol inside the tilde, so the glyph stays the
// first character read. MeshPlan.Monthly already carries the tilde.
func meshMoney(p MeshPlan) string {
	return EstimateMark + currencySymbol(p.Currency) + strings.TrimPrefix(p.Monthly, EstimateMark)
}

func rateSourceLine(rs []RateSource) string {
	if len(rs) == 0 {
		return "no rate resolved"
	}
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("%d via %s", r.Nodes, r.Source))
	}
	return strings.Join(parts, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func formatInt(n int) string { return groupDigits(strconv.Itoa(n)) }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func escapePipes(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
