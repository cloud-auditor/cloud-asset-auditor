package insight

// Cost insights: the money-shaped questions.
//
// Everything here is derived from the same estimator `auditor cost` runs, over
// the same price book, and it deliberately speaks that command's vocabulary
// rather than inventing a second one. A figure carries the ~ glyph when it came
// from list price; an aggregate with nothing in it renders as cost.NoMoney and
// never as 0.00; currencies are segregated and never combined; and the coverage
// denominator ("46 of 164 assets carry a price") travels with every total,
// because a total quoted without it invites the reader to assume it is the
// total.
//
// The one thing these findings add over `auditor cost` is a question. That
// report answers "what does this estate cost"; these answer "which part of that
// number would a human want to look at" — where the spend is concentrated,
// which of it cannot be attributed to anything, which of it is attached to
// resources the graph relates to nothing, and which of it is still being
// charged for something whose own status says it stopped.
//
// None of that is a claim about waste. An inventory cannot see consumption, so
// "this costs a lot" is a fact and "this is wasted" is a guess; every Caveat in
// this file is written to keep those apart.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/cost"
)

func init() {
	Register(costRunRate{})
	Register(costConcentration{})
	Register(costByDimension{})
	Register(costShowback{})
	Register(costOrphanedSpend{})
	Register(costStoppedBilled{})
	Register(costUnpriced{})
}

// ----------------------------------------------------------------------
// the shared pass
// ----------------------------------------------------------------------

// spendItem is one asset and what the estimator said about it.
type spendItem struct {
	asset core.Asset
	est   cost.Estimate
	// money is meaningful only for a priced item. It is the Input's own
	// measured/estimated split, so no insight in this file re-derives it — that
	// split is exactly what keeps a list-price guess from being laundered into
	// an invoice.
	money Money
}

// spendBucket is which of internal/cost's four buckets an asset landed in. The
// buckets are that package's, not a parallel set: an insight that invented its
// own "unpriced" definition would disagree with `auditor cost` on the same
// inventory, and the reader would have no way to tell which was wrong.
type spendBucket int

const (
	bucketPriced spendBucket = iota
	bucketUnknown
	bucketMetered
	bucketAttributed
)

// String is the word internal/cost's report prints for each bucket, so a
// reader moving between the two commands meets one vocabulary.
func (b spendBucket) String() string {
	switch b {
	case bucketPriced:
		return "priced"
	case bucketMetered:
		return "metered"
	case bucketAttributed:
		return "attributed"
	default:
		return "unknown"
	}
}

// why is the one-line explanation of what a bucket means, matching the text
// internal/cost's NOT PRICED table prints beside each one.
func (b spendBucket) why() string {
	switch b {
	case bucketMetered:
		return "billing is consumption-based; an inventory cannot see consumption"
	case bucketAttributed:
		return "a share of another asset's cost, not spend of its own; never added to a total"
	default:
		return "no rule matched — a gap in the price book, not a free resource"
	}
}

func (i spendItem) bucket() spendBucket {
	switch {
	case i.est.Priced:
		return bucketPriced
	case i.est.Attributed:
		return bucketAttributed
	case i.est.Basis == cost.BasisUnpriceable:
		return bucketMetered
	default:
		return bucketUnknown
	}
}

// spendIndex is one pass over the inventory with every asset priced once.
//
// Each insight in this file builds its own — the estimator is a pure function
// and the Input memoizes nothing about it, so sharing one would mean package
// state that outlives a run. The pass is cheap: for most types the rule lookup
// misses or the rule is declared, and neither touches Asset.Raw.
type spendIndex struct {
	// items is every asset in the Input's canonical (provider, type, id) order,
	// which is what makes every finding below deterministic without sorting.
	items  []spendItem
	counts [4]int
	total  *spendBag
}

func newSpendIndex(in *Input) *spendIndex {
	x := &spendIndex{total: newSpendBag()}
	if !in.Priced() {
		// Requirements keep every insight here from running with cost off, so
		// this is only reachable from a hand-assembled Input. An empty index is
		// the honest answer: no estimates means no money-shaped findings.
		return x
	}
	x.items = make([]spendItem, 0, len(in.Assets))
	for _, a := range in.Assets {
		est, _ := in.Estimate(a)
		item := spendItem{asset: a, est: est}
		if est.Priced {
			item.money, _ = in.Monthly(a)
			x.total.add(item.money)
		}
		x.counts[item.bucket()]++
		x.items = append(x.items, item)
	}
	return x
}

func (x *spendIndex) assets() int  { return len(x.items) }
func (x *spendIndex) priced() int  { return x.counts[bucketPriced] }
func (x *spendIndex) unknown() int { return x.counts[bucketUnknown] }

// unpriced is how many assets contribute nothing to any total in this file.
func (x *spendIndex) unpriced() int {
	return x.counts[bucketUnknown] + x.counts[bucketMetered] + x.counts[bucketAttributed]
}

// pricedItems returns the assets that carry a figure, in canonical order.
//
// A status-zeroed asset (0.00 with a zero_note) is priced and stays in: the
// price book made a positive claim about it, which is a different thing from
// having no answer, and cost.stopped-but-billed reads exactly that difference.
func (x *spendIndex) pricedItems() []spendItem {
	out := make([]spendItem, 0, x.priced())
	for _, i := range x.items {
		if i.est.Priced {
			out = append(out, i)
		}
	}
	return out
}

// coverage is the sentence that has to accompany every total in this file.
func (x *spendIndex) coverage() string {
	return fmt.Sprintf("%s of %s assets could be priced at all, and the other %s contribute nothing "+
		"rather than zero", formatInt(x.priced()), formatInt(x.assets()), formatInt(x.unpriced()))
}

// ----------------------------------------------------------------------
// money
// ----------------------------------------------------------------------

// spendBag keeps money apart by currency.
//
// It is internal/cost's moneyBag in miniature, and it exists for the same
// reason: NetBird publishes in EUR and everything else in USD, no exchange rate
// is applied anywhere in this tool, so a grand total across currencies is not a
// number that can be computed — only a list of per-currency totals.
type spendBag struct {
	byCurrency map[string]*Money
}

func newSpendBag() *spendBag { return &spendBag{byCurrency: map[string]*Money{}} }

func (b *spendBag) add(m Money) {
	cur := b.byCurrency[m.Currency]
	if cur == nil {
		cur = &Money{Currency: m.Currency}
		b.byCurrency[m.Currency] = cur
	}
	cur.Measured += m.Measured
	cur.Estimated += m.Estimated
}

// sorted lists the per-currency totals, biggest first.
func (b *spendBag) sorted() []Money {
	out := make([]Money, 0, len(b.byCurrency))
	for _, m := range b.byCurrency {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total() != out[j].Total() {
			return out[i].Total() > out[j].Total()
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// single returns the one currency's money, and false when the bag holds none or
// several. A Finding.Total is single-currency by construction, so a finding
// whose assets span two currencies must drop its Total and say so in its
// Caveat — see SumMoney for why nothing here converts.
func (b *spendBag) single() (Money, bool) {
	if len(b.byCurrency) != 1 {
		return Money{}, false
	}
	for _, m := range b.byCurrency {
		return *m, true
	}
	return Money{}, false
}

// rank sums across currencies FOR ORDERING ONLY. The figure it produces is
// never rendered and never quoted: it exists so that a by-region table has a
// deterministic biggest-first order even in a mixed-currency estate.
// internal/cost's groupTotal makes the same trade for the same table, and its
// report notes it whenever more than one currency is present.
func (b *spendBag) rank() float64 {
	var sum float64
	for _, m := range b.byCurrency {
		sum += m.Total()
	}
	return sum
}

func (b *spendBag) currencies() []string {
	out := make([]string, 0, len(b.byCurrency))
	for c := range b.byCurrency {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// money renders the bag for a row: one figure per currency, joined, and
// cost.NoMoney when there is nothing. Never "0.00" — an aggregate that summed
// to nothing and an aggregate of things that are free must not look alike.
func (b *spendBag) money() string {
	ms := b.sorted()
	if len(ms) == 0 {
		return cost.NoMoney
	}
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, m.String())
	}
	return strings.Join(parts, " / ")
}

// totalOf is the Finding.Total for a bag, or nil when there is no defensible
// single figure — several currencies, or nothing to total.
func totalOf(b *spendBag) *Money {
	m, ok := b.single()
	if !ok || m.Total() <= 0 {
		return nil
	}
	return &m
}

// rowMoney is a bag as a row's money cell, or nil when it spans currencies (the
// renderer would have to pick one, and picking is converting).
func rowMoney(b *spendBag) *Money {
	m, ok := b.single()
	if !ok {
		return nil
	}
	return &m
}

// yearly is the monthly figure multiplied by twelve.
//
// The measured and estimated halves are scaled separately so the split survives
// into the yearly number: twelve times a billed month is still a billed month's
// worth of fact, and twelve times a list-price guess is still a guess. Merging
// them here would produce one number that looks more certain than either half.
func yearly(m Money) Money {
	return Money{Currency: m.Currency, Measured: m.Measured * 12, Estimated: m.Estimated * 12}
}

// formatShare renders part/whole as a percentage. Sub-1% shares become "<1%"
// rather than "0%", because a row that reads 0% next to a real figure reads as
// a bug in the arithmetic.
func formatShare(part, whole float64) string {
	if whole <= 0 || part < 0 {
		return ""
	}
	v := part / whole * 100
	switch {
	case v == 0:
		// A group with no money at all. "0%" beside an em-dash money cell says
		// "nothing here"; "0.0%" reads as a rounded-down figure.
		return "0%"
	case v < 1:
		return "<1%"
	case v < 10:
		return fmt.Sprintf("%.1f%%", v)
	default:
		return fmt.Sprintf("%.0f%%", v)
	}
}

// shareFunc renders a figure's share of the estate's estimated total — or
// nothing at all when several currencies are present, because a percentage of a
// cross-currency sum is a percentage of a number this tool refuses to compute.
func (x *spendIndex) shareFunc() func(float64) string {
	if len(x.total.byCurrency) != 1 {
		return func(float64) string { return "" }
	}
	whole := x.total.rank()
	return func(part float64) string { return formatShare(part, whole) }
}

// currencyClause is the sentence a Caveat appends when the assets it counted are
// billed in more than one currency. Empty otherwise, so it can be concatenated
// unconditionally.
func currencyClause(b *spendBag) string {
	if len(b.byCurrency) < 2 {
		return ""
	}
	return fmt.Sprintf(" The assets counted here are billed in %s: no exchange rate is applied "+
		"anywhere in this tool, so the figures are reported separately and this finding carries no "+
		"combined total.", strings.Join(b.currencies(), " and "))
}

// ----------------------------------------------------------------------
// grouping
// ----------------------------------------------------------------------

// spendGroup is one row of a by-dimension rollup.
type spendGroup struct {
	key    string
	assets int
	priced int
	bag    *spendBag
}

// groupSpend buckets every asset — priced or not — by a dimension.
//
// Unpriced assets are counted deliberately: a region holding forty resources of
// which two could be priced is a materially different row from one holding two,
// and a rollup that showed only the priced ones would make the cheapest-looking
// region the one this tool knows least about.
func groupSpend(items []spendItem, key func(core.Asset) string) []spendGroup {
	groups := map[string]*spendGroup{}
	var order []string
	for _, i := range items {
		k := key(i.asset)
		if k == "" {
			// "(none)" rather than a blank cell, matching internal/cost's
			// groupKey. A rollup row that is an empty string reads as a bug.
			k = "(none)"
		}
		g := groups[k]
		if g == nil {
			g = &spendGroup{key: k, bag: newSpendBag()}
			groups[k] = g
			order = append(order, k)
		}
		g.assets++
		if i.est.Priced {
			g.priced++
			g.bag.add(i.money)
		}
	}
	out := make([]spendGroup, 0, len(groups))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	sort.SliceStable(out, func(a, b int) bool {
		if ra, rb := out[a].bag.rank(), out[b].bag.rank(); ra != rb {
			return ra > rb
		}
		return out[a].key < out[b].key
	})
	return out
}

// ----------------------------------------------------------------------
// run rate
// ----------------------------------------------------------------------

// costRunRate reports what the estate costs to run for a month, and for a year
// at the same configuration.
type costRunRate struct{}

func (costRunRate) ID() string             { return "cost.run-rate" }
func (costRunRate) Title() string          { return "Run rate" }
func (costRunRate) Family() Family         { return FamilyCost }
func (costRunRate) Requires() Requirements { return Requirements{Cost: true} }
func (costRunRate) Run(_ context.Context, in *Input) []Finding {
	x := newSpendIndex(in)

	var (
		rows  []Row
		parts []string
	)
	for _, m := range x.total.sorted() {
		if m.Total() <= 0 {
			// A currency whose assets all priced to zero has no run rate to
			// quote. Saying "—/mo" would be a figure-shaped way of saying
			// nothing; cost.stopped-but-billed and cost.unpriced carry that story.
			continue
		}
		monthly, year := m, yearly(m)
		// "at this rate", not "a year". The Summary is the one line built to be
		// quoted on its own — it is what lands in a Slack paste, detached from
		// the Caveat two lines below it — and a bare "$54,648 a year" reads as a
		// budget for next year rather than as this month multiplied by twelve.
		// Three words keep the projection self-limiting wherever it travels.
		parts = append(parts, fmt.Sprintf("%s a month (%s a year at this rate)",
			monthly.String(), year.String()))
		rows = append(rows,
			Row{
				Label: monthly.Currency + " monthly run rate",
				Money: &monthly,
				Fact:  fmt.Sprintf("%s of %s assets priced", formatInt(x.priced()), formatInt(x.assets())),
			},
			Row{
				Label: year.Currency + " yearly run rate",
				Money: &year,
				Fact:  "monthly × 12 at today's configuration",
			},
		)
	}
	if len(rows) == 0 {
		return nil
	}

	return []Finding{{
		ID:       "cost.run-rate",
		Title:    "Estimated run rate",
		Severity: SeverityInfo,
		Count:    x.priced(),
		Summary: fmt.Sprintf("%s across %s priced resource%s; %s of %s assets carry no price at all.",
			strings.Join(parts, " and "), formatInt(x.priced()), plural(x.priced()),
			formatInt(x.unpriced()), formatInt(x.assets())),
		Basis: "every collected asset priced by the same estimator and price book `auditor cost` uses, " +
			"summed per currency; the yearly figure is that monthly sum multiplied by 12.",
		// The yearly-projection caveat. It is the sentence this finding lives
		// or dies by: a number labelled "a year" is read as a budget unless it
		// is told, in the same breath, that it is arithmetic on one snapshot.
		Caveat: "The yearly figure is the monthly one multiplied by 12: a run-rate projection of " +
			"today's configuration held constant, not a forecast and not what you will be invoiced. " +
			"It assumes no resource is created, deleted, resized, stopped or repriced for twelve " +
			"months, and it inherits every exclusion the monthly estimate carries. " +
			x.coverage() + ", so this is a run rate for the part of the estate that could be priced, " +
			"not for the estate. " + cost.DisclaimerShort,
		Rows: rows,
	}}
}

// ----------------------------------------------------------------------
// concentration
// ----------------------------------------------------------------------

// costConcentration answers "how few resources are most of the money".
type costConcentration struct{}

func (costConcentration) ID() string             { return "cost.concentration" }
func (costConcentration) Title() string          { return "Cost concentration" }
func (costConcentration) Family() Family         { return FamilyCost }
func (costConcentration) Requires() Requirements { return Requirements{Cost: true} }

// concentrationTiers are the prefixes reported. One, five and ten because they
// are the sizes a person can act on: one resource is a decision, five is a
// morning, ten is a project.
var concentrationTiers = []int{1, 5, 10}

// lopsidedFloor is how many priced resources there must be before a small
// half-of-the-spend prefix means anything. Below it, "three resources are half
// the estimated total" is arithmetic about a tiny estate rather than a shape
// worth remarking on.
const lopsidedFloor = 10

func (costConcentration) Run(_ context.Context, in *Input) []Finding {
	x := newSpendIndex(in)

	// Ranking happens inside one currency. Sorting a EUR figure against a USD
	// one orders them as if they were comparable, which they are not without a
	// rate this tool refuses to apply — so the largest currency is ranked and
	// the Caveat names what was left out.
	totals := x.total.sorted()
	if len(totals) == 0 || totals[0].Total() <= 0 {
		return nil
	}
	currency := totals[0].Currency
	total := totals[0].Total()

	var ranked []spendItem
	for _, i := range x.pricedItems() {
		if i.money.Currency == currency && i.money.Total() > 0 {
			ranked = append(ranked, i)
		}
	}
	if len(ranked) == 0 {
		return nil
	}
	sort.SliceStable(ranked, func(a, b int) bool {
		if ta, tb := ranked[a].money.Total(), ranked[b].money.Total(); ta != tb {
			return ta > tb
		}
		// Equal figures are common — twelve identical volumes — so the id
		// breaks the tie and two runs list them in the same order.
		return ranked[a].asset.ID < ranked[b].asset.ID
	})

	// half is the smallest prefix that accounts for 50% of the estimated total.
	// It is the headline number: "3" says more about an estate than any
	// percentage does.
	var (
		running float64
		half    int
		clauses []string
	)
	for _, i := range ranked {
		running += i.money.Total()
		half++
		if running >= total/2 {
			break
		}
	}

	running = 0
	shown := newSpendBag()
	var rows []Row
	share := func(v float64) string { return formatShare(v, total) }
	for n, i := range ranked {
		running += i.money.Total()
		for _, tier := range concentrationTiers {
			if n+1 != tier || tier > len(ranked) {
				continue
			}
			if tier == 1 {
				clauses = append(clauses, "the largest single resource is "+share(running))
				continue
			}
			clauses = append(clauses, fmt.Sprintf("the top %d are %s", tier, share(running)))
		}
		if n < concentrationTiers[len(concentrationTiers)-1] {
			money := i.money
			shown.add(money)
			rows = append(rows, Row{
				Label: DisplayName(i.asset),
				Asset: refOf(i.asset),
				Value: share(money.Total()),
				Money: &money,
				Fact:  i.asset.Type,
			})
		}
	}

	// A shape worth remarking on, not a defect: an estate whose spend sits in
	// three machines is usually that way on purpose, and the reader is the one
	// who knows whether it should be.
	severity := SeverityInfo
	if len(ranked) >= lopsidedFloor && half <= 5 {
		severity = SeverityNotable
	}

	return []Finding{{
		ID:       "cost.concentration",
		Title:    "Where the estimated cost sits",
		Severity: severity,
		Count:    half,
		Summary: fmt.Sprintf("Half the estimated %s total (%s a month) sits in %s of %s priced "+
			"resource%s; %s.", currency, totals[0].String(), formatInt(half), formatInt(len(ranked)),
			plural(len(ranked)), strings.Join(clauses, ", ")),
		// The denominator here is smaller than the "N priced" figure the other
		// cost findings quote, and the difference has to be stated or it reads as
		// two findings disagreeing: assets that priced to exactly zero (a stopped
		// instance the book zeroes) are priced, but they hold no share of a total
		// and ranking them would pad the list with rows that cannot move it.
		Basis: fmt.Sprintf("the %s priced assets billed in %s that carry a figure above zero, ranked "+
			"by estimated monthly figure; shares are of that currency's estimated total. Assets priced "+
			"at exactly zero hold no share and are excluded from this ranking, so this denominator is "+
			"smaller than the priced count elsewhere in this report. Flagged notable when half the "+
			"total sits in five resources or fewer out of at least %d.",
			formatInt(len(ranked)), currency, lopsidedFloor),
		Caveat: "A share of an estimate is not a share of an invoice. The denominator is only the assets " +
			"this price book could price — " + x.coverage() + " — so a large unpriced share inflates every " +
			"percentage here, and the total beneath the table covers the listed resources rather than the " +
			"estate. Rank is by list price at today's configuration, not by usage: the most expensive " +
			"resource is the one to understand first, not the one to remove first, and an inventory " +
			"cannot see whether any of it is earning its keep." + concentrationCurrencyClause(totals),
		Rows:  rows,
		Total: totalOf(shown),
	}}
}

// verb agrees the summary's verb with its count, so a single-resource estate
// does not read "1 resources are".
func verb(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// concentrationCurrencyClause names the currencies this ranking left out.
func concentrationCurrencyClause(totals []Money) string {
	if len(totals) < 2 {
		return ""
	}
	rest := make([]string, 0, len(totals)-1)
	for _, m := range totals[1:] {
		rest = append(rest, m.Currency)
	}
	return fmt.Sprintf(" Assets billed in %s are excluded from this ranking entirely: no exchange rate "+
		"is applied anywhere in this tool, so they cannot be ordered against %s.",
		strings.Join(rest, " and "), totals[0].Currency)
}

// ----------------------------------------------------------------------
// by dimension
// ----------------------------------------------------------------------

// costByDimension rolls the estimate up by the three fields every asset has.
type costByDimension struct{}

func (costByDimension) ID() string             { return "cost.by-dimension" }
func (costByDimension) Title() string          { return "Cost by provider, region and account" }
func (costByDimension) Family() Family         { return FamilyCost }
func (costByDimension) Requires() Requirements { return Requirements{Cost: true} }

func (costByDimension) Run(_ context.Context, in *Input) []Finding {
	x := newSpendIndex(in)
	if x.priced() == 0 {
		return nil
	}

	var out []Finding
	dims := []struct {
		id, noun, title string
		key             func(core.Asset) string
		caveat          string
	}{
		{
			id: "cost.by-provider", noun: "provider", title: "Estimated cost by provider",
			key: func(a core.Asset) string { return a.Provider },
			caveat: "This is what this tool could price per provider, not what each provider bills you. " +
				"Price-book coverage differs sharply by provider — Cloudflare and the mesh providers are " +
				"largely per-seat or consumption-based and priced elsewhere or not at all — so a provider " +
				"with few rules looks cheap here rather than unknown.",
		},
		{
			id: "cost.by-region", noun: "region", title: "Estimated cost by region",
			key: func(a core.Asset) string { return a.Region },
			caveat: "Region is whatever the provider reported for the resource. A large \"(none)\" row is " +
				"not missing data: account-scoped and global resources genuinely have no region, and " +
				"every Cloudflare, Tailscale and NetBird asset is one. Rates also differ by region in the " +
				"published price lists, and this book carries one rate per SKU.",
		},
		{
			id: "cost.by-account", noun: "account", title: "Estimated cost by account",
			key: func(a core.Asset) string { return a.AccountID },
			caveat: "AccountID means a different thing per provider — an OCI tenancy OCID, a Cloudflare " +
				"account id, a Kubernetes cluster name — so this column mixes several kinds of identifier " +
				"and is a billing boundary only where the provider makes it one. For OCI the boundary you " +
				"are probably after is the compartment, which is a tag rather than this field.",
		},
	}

	share := x.shareFunc()
	for _, d := range dims {
		groups := groupSpend(x.items, d.key)
		if len(groups) < 2 {
			// One group is not a breakdown. The scope note already says when
			// only one provider contributed, and a single 100% row adds nothing.
			continue
		}

		rows := make([]Row, 0, len(groups))
		for _, g := range groups {
			rows = append(rows, Row{
				Label: g.key,
				Value: share(g.bag.rank()),
				Money: rowMoney(g.bag),
				Fact: fmt.Sprintf("%s of %s asset%s priced",
					formatInt(g.priced), formatInt(g.assets), plural(g.assets)),
			})
		}

		// Count the groups that actually carry a figure, not the groups. A
		// breakdown over 8 accounts where 6 priced to nothing was summarising
		// itself as "8 accounts carry estimated spend" while the table directly
		// beneath it showed those 6 at 0% with an empty money column — the
		// summary is the line that travels without the table, so it has to be the
		// one that is true on its own.
		withSpend := 0
		for _, g := range groups {
			if g.bag.rank() > 0 {
				withSpend++
			}
		}

		lead := groups[0]
		summary := fmt.Sprintf("%s of %s %ss carry estimated spend; %s is the largest at %s a month",
			formatInt(withSpend), formatInt(len(groups)), d.noun, lead.key, lead.bag.money())
		if s := share(lead.bag.rank()); s != "" {
			summary += " (" + s + " of the estimated total)"
		}

		out = append(out, Finding{
			ID:       d.id,
			Title:    d.title,
			Severity: SeverityInfo,
			Count:    len(groups),
			Summary:  summary + ".",
			Basis: fmt.Sprintf("every collected asset grouped by its %s field, priced by the price book "+
				"and summed per currency; a group with no value for the field is \"(none)\".", d.noun),
			Caveat: d.caveat + " " + x.coverage() + "; an unpriced asset lands in its group's asset " +
				"count while contributing nothing to its money, so a group can be large and cheap here " +
				"only because this book has no rule for what is in it." + currencyClause(x.total),
			Rows:  rows,
			Total: totalOf(x.total),
		})
	}
	return out
}

// ----------------------------------------------------------------------
// showback
// ----------------------------------------------------------------------

// costShowback answers the question a finance team actually asks: whose is
// this? It reports what fraction of the estimated spend carries a
// cost-allocation tag, and breaks the best-covered one down by value.
type costShowback struct{}

func (costShowback) ID() string             { return "cost.showback" }
func (costShowback) Title() string          { return "Cost allocation by tag" }
func (costShowback) Family() Family         { return FamilyCost }
func (costShowback) Requires() Requirements { return Requirements{Cost: true} }

// allocationTagKeys are the tag keys this tool will treat as a cost-allocation
// dimension, matched case-insensitively against the keys assets actually carry.
//
// A closed list rather than "every tag key present" for one reason: an
// inventory's tag space is dominated by Kubernetes labels and provider-injected
// metadata, and a showback report that offered `pod-template-hash` as an
// allocation dimension would bury the one key somebody actually tags with.
// compartment_id earns its place because it is OCI's native billing boundary
// and this tool stamps it on every OCI asset.
var allocationTagKeys = map[string]bool{
	"app": true, "application": true, "business-unit": true, "business_unit": true,
	"compartment_id": true, "component": true, "cost-center": true, "cost_center": true,
	"costcenter": true, "customer": true, "department": true, "env": true,
	"environment": true, "owner": true, "product": true, "project": true,
	"service": true, "stage": true, "team": true, "tenant": true, "workload": true,
}

// allocation is one candidate tag key's coverage of the priced estate.
type allocation struct {
	key      string
	covered  int      // priced assets carrying a non-empty value
	values   []string // distinct values, sorted
	byValue  map[string]*spendBag
	valueN   map[string]int
	tagged   *spendBag // money that carries a value for this key
	untagged *spendBag // money that does not — the showback gap
}

func (costShowback) Run(_ context.Context, in *Input) []Finding {
	x := newSpendIndex(in)
	priced := x.pricedItems()
	if len(priced) == 0 {
		return nil
	}

	keys := allocationKeys(priced)
	if len(keys) == 0 {
		// Silence here would be read as "everything is attributed", which is the
		// opposite of the truth. An estate with no allocation convention is the
		// commonest showback answer there is, and it has to be said out loud.
		return []Finding{{
			ID:       "cost.unattributed",
			Title:    "No cost-allocation tag is in use",
			Severity: SeverityNotable,
			Count:    len(priced),
			Summary: fmt.Sprintf("None of the %s priced resources carries a recognised cost-allocation "+
				"tag, so none of the %s a month estimated here can be attributed to a team, environment "+
				"or service.", formatInt(len(priced)), x.total.money()),
			Basis: "the tag keys carried by priced assets, matched case-insensitively against this " +
				"tool's list of allocation keys (" + allocationKeyList() + ").",
			Caveat: "This checks a fixed list of conventional key names, so an estate that allocates " +
				"cost under a key not on that list — a `bu:` prefix, a numeric code, an OCI defined-tag " +
				"namespace this tool flattens differently — reads as untagged here when it is not. It " +
				"also says nothing about whether the tags that do exist are correct; an inventory can " +
				"see that a tag is present, not that it names the right owner.",
			Total: totalOf(x.total),
		}}
	}

	out := []Finding{unattributedFinding(x, priced, keys)}
	if f, ok := byTagFinding(in, x, priced, keys); ok {
		out = append(out, f)
	}
	return out
}

// allocationKeys collects the candidate keys present on priced assets and
// measures each one's coverage. Keys are returned sorted by the share of money
// they attribute, best first, so the caller can pick a breakdown dimension
// deterministically.
func allocationKeys(priced []spendItem) []allocation {
	present := map[string]*allocation{}
	var order []string
	for _, i := range priced {
		for k := range i.asset.Tags {
			if !allocationTagKeys[strings.ToLower(k)] {
				continue
			}
			if present[k] == nil {
				present[k] = &allocation{
					key:      k,
					byValue:  map[string]*spendBag{},
					valueN:   map[string]int{},
					tagged:   newSpendBag(),
					untagged: newSpendBag(),
				}
				order = append(order, k)
			}
		}
	}
	sort.Strings(order)

	for _, k := range order {
		a := present[k]
		for _, i := range priced {
			v := strings.TrimSpace(i.asset.Tags[k])
			if v == "" {
				a.untagged.add(i.money)
				continue
			}
			a.covered++
			a.tagged.add(i.money)
			if a.byValue[v] == nil {
				a.byValue[v] = newSpendBag()
				a.values = append(a.values, v)
			}
			a.byValue[v].add(i.money)
			a.valueN[v]++
		}
		sort.Strings(a.values)
	}

	out := make([]allocation, 0, len(order))
	for _, k := range order {
		out = append(out, *present[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Best-attributing key first: by money, then by assets covered, then by
		// name — a total order, so the chosen breakdown dimension does not move
		// between two runs over the same inventory.
		if ta, tb := out[i].tagged.rank(), out[j].tagged.rank(); ta != tb {
			return ta > tb
		}
		if out[i].covered != out[j].covered {
			return out[i].covered > out[j].covered
		}
		return out[i].key < out[j].key
	})
	return out
}

func allocationKeyList() string {
	keys := make([]string, 0, len(allocationTagKeys))
	for k := range allocationTagKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// unattributedFinding is the showback headline: how much of the estimate lands
// on nobody.
func unattributedFinding(x *spendIndex, priced []spendItem, keys []allocation) Finding {
	// Assets carrying none of the candidate keys — the spend that cannot be
	// attributed under any convention this estate uses, which is a stronger
	// statement than any single key's gap.
	orphaned := newSpendBag()
	unattributed := 0
	for _, i := range priced {
		tagged := false
		for _, a := range keys {
			if strings.TrimSpace(i.asset.Tags[a.key]) != "" {
				tagged = true
				break
			}
		}
		if !tagged {
			unattributed++
			orphaned.add(i.money)
		}
	}

	share := x.shareFunc()
	rows := make([]Row, 0, len(keys)+1)
	for _, a := range keys {
		rows = append(rows, Row{
			Label: a.key,
			Value: share(a.tagged.rank()),
			Money: rowMoney(a.untagged),
			Fact: fmt.Sprintf("%s of %s priced assets carry it",
				formatInt(a.covered), formatInt(len(priced))),
		})
	}
	if unattributed > 0 {
		// The headline figure as its own row. It is not the sum of the rows
		// above — those overlap, since one resource missing two keys appears in
		// both — which is also why this finding carries no TOTAL.
		rows = append(rows, Row{
			Label: "(none of these keys)",
			Value: share(orphaned.rank()),
			Money: rowMoney(orphaned),
			Fact:  fmt.Sprintf("%s of %s priced assets", formatInt(unattributed), formatInt(len(priced))),
		})
	}

	// Warn only when a convention exists and is applied unevenly — that is the
	// case that is probably unintended and cheap to check. A key on everything
	// is information, not a defect.
	severity := SeverityInfo
	if orphaned.rank() > 0 {
		severity = SeverityWarn
	}

	summary := fmt.Sprintf("%s of %s priced resources carry none of the %s allocation tag key%s in use here",
		formatInt(unattributed), formatInt(len(priced)), formatInt(len(keys)), plural(len(keys)))
	if orphaned.rank() > 0 {
		summary += fmt.Sprintf(", leaving %s a month", orphaned.money())
		if s := share(orphaned.rank()); s != "" {
			summary += " (" + s + " of the estimated total)"
		}
		summary += " attributable to nobody"
	}

	return Finding{
		ID:       "cost.unattributed",
		Title:    "Estimated spend that cannot be attributed",
		Severity: severity,
		Count:    unattributed,
		Summary:  summary + ".",
		Basis: "priced assets grouped by whether they carry a value for each allocation tag key " +
			"present in this inventory; the money column is the spend that carries NO value for that " +
			"key, the value column is the share of the estimated total that does, and the last row is " +
			"the spend carrying none of the keys at all.",
		Caveat: "An untagged resource is not an unowned one — this says the tag is absent, not that " +
			"nobody is accountable for it, and a key this tool does not recognise as an allocation " +
			"dimension reads as absent here. It also cannot check that a tag is right: a resource " +
			"labelled team=platform is counted against that team whether or not it belongs to them. " +
			"The per-key gaps overlap, so the money column does not add up to anything and this " +
			"finding carries no total. " + x.coverage() + "; unpriced resources are outside this " +
			"accounting entirely." + currencyClause(orphaned) + " " + cost.DisclaimerShort,
		Rows: rows,
	}
}

// byTagFinding breaks the best-covered allocation key down by value, with the
// untagged remainder as an explicit row rather than a silent shortfall.
func byTagFinding(in *Input, x *spendIndex, priced []spendItem, keys []allocation) (Finding, bool) {
	best := keys[0]
	if len(best.values) == 0 {
		return Finding{}, false
	}
	untaggedN := len(priced) - best.covered
	if len(best.values) < 2 && untaggedN == 0 {
		// One value covering everything is a constant, not a breakdown.
		return Finding{}, false
	}

	type valueRow struct {
		value string
		bag   *spendBag
		n     int
	}
	vals := make([]valueRow, 0, len(best.values))
	for _, v := range best.values {
		vals = append(vals, valueRow{value: v, bag: best.byValue[v], n: best.valueN[v]})
	}
	sort.SliceStable(vals, func(a, b int) bool {
		if ra, rb := vals[a].bag.rank(), vals[b].bag.rank(); ra != rb {
			return ra > rb
		}
		return vals[a].value < vals[b].value
	})

	share := x.shareFunc()
	rows := make([]Row, 0, len(vals)+1)
	for _, v := range vals {
		rows = append(rows, Row{
			Label: allocationLabel(in, v.value),
			Value: share(v.bag.rank()),
			Money: rowMoney(v.bag),
			Fact:  fmt.Sprintf("%s asset%s", formatInt(v.n), plural(v.n)),
		})
	}
	if untaggedN > 0 {
		// Last, and never omitted. A breakdown whose rows do not add up to the
		// total is the classic way to make an attribution problem invisible.
		rows = append(rows, Row{
			Label: "(no " + best.key + ")",
			Value: share(best.untagged.rank()),
			Money: rowMoney(best.untagged),
			Fact:  fmt.Sprintf("%s asset%s", formatInt(untaggedN), plural(untaggedN)),
		})
	}

	return Finding{
		ID:       "cost.by-tag",
		Title:    "Estimated cost by " + best.key,
		Severity: SeverityInfo,
		Count:    len(vals),
		Summary: fmt.Sprintf("%s distinct %s value%s account for %s a month; %s priced resource%s "+
			"carry no %s at all.", formatInt(len(vals)), best.key, plural(len(vals)),
			best.tagged.money(), formatInt(untaggedN), plural(untaggedN), best.key),
		Basis: fmt.Sprintf("priced assets grouped by their %q tag — the allocation key covering the "+
			"largest share of estimated spend in this inventory, chosen by money then by assets covered "+
			"then alphabetically. Values that are themselves an asset id are shown by that asset's name. "+
			"For any other key, `auditor cost --group-by tag:KEY` produces the same rollup.", best.key),
		Caveat: "This attributes list-price estimates, not charges: it is a view of what this tool " +
			"thinks each group's resources would cost, and a group whose resources are mostly unpriced " +
			"looks small rather than unknown. " + x.coverage() + ". Nothing here checks that the tag is " +
			"applied correctly, and a resource shared between two groups is counted whole against the " +
			"one it is tagged with." + currencyClause(x.total),
		Rows: rows,
		// The rows partition the priced estate — every value plus the untagged
		// remainder — so the total is the estate's, and the column adds up to it.
		Total: totalOf(x.total),
	}, true
}

// allocationLabel resolves a tag value that is itself an asset id to that
// asset's name — an OCI compartment_id is an OCID, and a rollup of OCIDs is a
// rollup nobody can read. The same join internal/output's XLSX sheet grouping
// makes, for the same reason.
func allocationLabel(in *Input, value string) string {
	if a, ok := in.AssetByID(value); ok && a.Name != "" {
		return a.Name
	}
	return value
}

// ----------------------------------------------------------------------
// spend the graph connects to nothing
// ----------------------------------------------------------------------

// costOrphanedSpend intersects two facts this tool already holds: an asset the
// price book put a figure on, and an asset the topology graph relates to
// nothing.
type costOrphanedSpend struct{}

func (costOrphanedSpend) ID() string     { return "cost.orphaned-spend" }
func (costOrphanedSpend) Title() string  { return "Priced resources the graph connects to nothing" }
func (costOrphanedSpend) Family() Family { return FamilyCost }
func (costOrphanedSpend) Requires() Requirements {
	return Requirements{Cost: true, Topology: true}
}

func (costOrphanedSpend) Run(_ context.Context, in *Input) []Finding {
	x := newSpendIndex(in)
	if x.priced() == 0 {
		return nil
	}

	// The classification is internal/topology's, not a second one: Unconnected
	// holds the types that graph demonstrably can connect (some other asset of
	// the same type has an edge, or a resolver names the type), which is the
	// difference between a load balancer that lost its DNS record and five
	// hundred ConfigMaps that were never going to have an edge.
	orphans := in.Graph.Orphans("provider")
	connectable := make(map[string]bool, len(orphans.Unconnected))
	for _, g := range orphans.Unconnected {
		connectable[g.Type] = true
	}

	// Only assets that are actually nodes of this graph can be judged by their
	// degree in it. The guard matters: a collapsed graph (Topology.Collapse
	// replaces assets with per-group summary nodes) would otherwise report every
	// priced asset as degree 0, turning the whole estate into a finding.
	nodes := make(map[core.AssetRef]bool, len(in.Graph.Nodes))
	for _, n := range in.Graph.Nodes {
		nodes[n.AsRef()] = true
	}

	bag := newSpendBag()
	var rows []Row
	for _, i := range x.pricedItems() {
		if i.money.Total() <= 0 || !connectable[i.asset.Type] {
			continue
		}
		ref := i.asset.AsRef()
		if !nodes[ref] || in.Degree(ref) != 0 {
			continue
		}
		money := i.money
		bag.add(money)
		rows = append(rows, Row{
			Label: DisplayName(i.asset),
			Asset: refOf(i.asset),
			Money: &money,
			Fact:  i.asset.Type,
		})
	}
	if len(rows) == 0 {
		return nil
	}

	return []Finding{{
		ID: "cost.orphaned-spend",
		// Not "paying for nothing". The graph's silence is not evidence that
		// nothing uses these, and the title is the part of a finding that gets
		// quoted without its caveat.
		Title:    "Priced resources with no inferred relationship",
		Severity: SeverityNotable,
		Count:    len(rows),
		Summary: fmt.Sprintf("%s priced resource%s costing %s a month %s degree 0 in the inferred "+
			"graph — nothing this tool models points at %s.", formatInt(len(rows)), plural(len(rows)),
			bag.money(), verb(len(rows)), them(len(rows))),
		Basis: fmt.Sprintf("the topology graph's %s degree-0 node%s in types internal/topology "+
			"classifies as connectable — some other asset of that type does have an edge, or a resolver "+
			"names it — intersected with the assets this price book put a non-zero figure on.",
			formatInt(orphans.UnconnectedCount()), plural(orphans.UnconnectedCount())),
		// The orphan report's own words, verbatim. Restating them in this
		// file's voice would let a cost finding sound more certain about a
		// degree-0 node than the report that computes them is — and that report
		// is the one that had to think hard about it.
		Caveat: "This joins a price to a degree-0 node; the price is the reliable half. " +
			strings.Join(orphans.Caveat, " "),
		Rows:  rows,
		Total: totalOf(bag),
	}}
}

// them agrees the summary's pronoun with its count.
func them(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// ----------------------------------------------------------------------
// stopped, and still priced
// ----------------------------------------------------------------------

// costStoppedBilled finds resources whose own status says they are not running
// while the price book still prices them.
type costStoppedBilled struct{}

func (costStoppedBilled) ID() string             { return "cost.stopped-but-billed" }
func (costStoppedBilled) Title() string          { return "Stopped, and still priced" }
func (costStoppedBilled) Family() Family         { return FamilyCost }
func (costStoppedBilled) Requires() Requirements { return Requirements{Cost: true} }

// stoppedStatuses is the closed vocabulary of lifecycle words that mean "this
// resource is not doing work".
//
// A table rather than a substring match, because a substring rule reads
// "stopping" out of "not_stopping" and "inactive" out of "reactivating", and a
// finding that names the wrong resource is worse than one that misses a
// spelling. Comparison is case- and separator-insensitive: OCI shouts its
// LifecycleState, GCP shouts its state, and the mesh providers use lower-case
// words, but that is a convention rather than a guarantee.
var stoppedStatuses = map[string]bool{
	"archived": true, "deallocated": true, "disabled": true, "inactive": true,
	"paused": true, "poweroff": true, "powered_off": true, "shutdown": true,
	"shutoff": true, "shut_off": true, "stopped": true, "stopping": true,
	"suspended": true, "terminated": true,
}

func stoppedStatus(s string) bool {
	n := strings.ToLower(strings.TrimSpace(s))
	n = strings.ReplaceAll(n, "-", "_")
	n = strings.ReplaceAll(n, " ", "_")
	return stoppedStatuses[n]
}

func (costStoppedBilled) Run(_ context.Context, in *Input) []Finding {
	x := newSpendIndex(in)

	bag := newSpendBag()
	var (
		rows   []Row
		zeroed int
	)
	for _, i := range x.items {
		if !stoppedStatus(i.asset.Status) || !i.est.Priced {
			continue
		}
		if i.money.Total() <= 0 {
			// The price book has a zero_when_status for this rule and it fired:
			// the book knows this status stops the charge. Counted, not listed —
			// it is the control group that makes the rows below mean something.
			zeroed++
			continue
		}
		money := i.money
		bag.add(money)
		rows = append(rows, Row{
			Label: DisplayName(i.asset),
			Asset: refOf(i.asset),
			Value: i.asset.Status,
			Money: &money,
			Fact:  i.asset.Type,
		})
	}
	if len(rows) == 0 {
		return nil
	}

	report, are := "report", "are"
	if len(rows) == 1 {
		report, are = "reports", "is"
	}
	summary := fmt.Sprintf("%s resource%s %s a stopped or inactive status and %s still priced at "+
		"%s a month.", formatInt(len(rows)), plural(len(rows)), report, are, bag.money())
	if zeroed > 0 {
		summary = strings.TrimSuffix(summary, ".") +
			fmt.Sprintf("; the price book zeroes %s other%s on their status.",
				formatInt(zeroed), plural(zeroed))
	}

	return []Finding{{
		ID:       "cost.stopped-but-billed",
		Title:    "Stopped, and still priced",
		Severity: SeverityWarn,
		Count:    len(rows),
		Summary:  summary,
		Basis: "assets whose Status is one of a fixed set of stopped/inactive lifecycle words, whose " +
			"price-book rule produced a non-zero monthly figure anyway — i.e. the rule declares no " +
			"zero_when_status for that status, which the estimator would otherwise have applied.",
		Caveat: "Two different things produce a row here, and this cannot tell them apart: a charge " +
			"that genuinely continues while the resource is stopped, and a price-book rule that was " +
			"never told this status matters. Neither means the resource is free, and neither is a " +
			"reading of your bill. Status is also a single point in time — a resource stopped when this " +
			"audit ran may have been running for most of the month, and one running now may have been " +
			"stopped all of it. The converse case is the expensive one and is invisible here: when a " +
			"rule does zero on status, the compute stops billing but the boot volume, block volumes, " +
			"reserved addresses and licences attached to it do not, and this tool cannot join a stopped " +
			"instance to its storage." + currencyClause(bag),
		Rows:  rows,
		Total: totalOf(bag),
	}}
}

// ----------------------------------------------------------------------
// unpriced share
// ----------------------------------------------------------------------

// costUnpriced restates the coverage accounting `auditor cost` prints beneath
// its total. It is repeated here because every other finding in this family
// quotes a figure, and a figure quoted without its denominator invites the
// reader to assume the total is the total.
type costUnpriced struct{}

func (costUnpriced) ID() string             { return "cost.unpriced" }
func (costUnpriced) Title() string          { return "Unpriced share of the estate" }
func (costUnpriced) Family() Family         { return FamilyCost }
func (costUnpriced) Requires() Requirements { return Requirements{Cost: true} }

func (costUnpriced) Run(_ context.Context, in *Input) []Finding {
	x := newSpendIndex(in)
	if x.assets() == 0 || x.unpriced() == 0 {
		return nil
	}

	type key struct {
		bucket spendBucket
		typ    string
	}
	counts := map[key]int{}
	reasons := map[key]map[string]int{}
	var order []key
	for _, i := range x.items {
		b := i.bucket()
		if b == bucketPriced {
			continue
		}
		k := key{bucket: b, typ: i.asset.Type}
		if _, seen := counts[k]; !seen {
			order = append(order, k)
			reasons[k] = map[string]int{}
		}
		counts[k]++
		reasons[k][i.est.Detail]++
	}
	sort.SliceStable(order, func(a, b int) bool {
		if counts[order[a]] != counts[order[b]] {
			return counts[order[a]] > counts[order[b]]
		}
		if order[a].bucket != order[b].bucket {
			return order[a].bucket < order[b].bucket
		}
		return order[a].typ < order[b].typ
	})

	rows := make([]Row, 0, len(order))
	for _, k := range order {
		rows = append(rows, Row{
			Label: k.typ,
			Value: formatInt(counts[k]),
			Fact:  k.bucket.String() + ": " + commonestDetail(reasons[k]),
		})
	}

	// unknown is the fixable bucket — a missing rule is a pull request away
	// from being a number — while metered and attributed are permanent facts
	// about how the thing is billed. Only the first is worth a reader's
	// attention beyond orientation.
	severity := SeverityInfo
	if x.unknown() > 0 {
		severity = SeverityNotable
	}

	return []Finding{{
		ID:       "cost.unpriced",
		Title:    "Assets carrying no price",
		Severity: severity,
		Count:    x.unpriced(),
		Summary: fmt.Sprintf("%s of %s assets (%s) carry no price: %s unknown, %s metered, %s "+
			"attributed — that is not $0, and every total in this report covers only the other %s.",
			formatInt(x.unpriced()), formatInt(x.assets()),
			formatShare(float64(x.unpriced()), float64(x.assets())),
			formatInt(x.counts[bucketUnknown]), formatInt(x.counts[bucketMetered]),
			formatInt(x.counts[bucketAttributed]), formatInt(x.priced())),
		Basis: "every asset the estimator could not put a figure on, grouped by type and by which of " +
			"internal/cost's buckets it landed in (" + bucketUnknown.String() + ": " + bucketUnknown.why() +
			"; " + bucketMetered.String() + ": " + bucketMetered.why() + "; " + bucketAttributed.String() +
			": " + bucketAttributed.why() + ").",
		Caveat: "An unpriced asset is not a free one, and this cannot say which kind of nothing it is " +
			"worth: a metered resource may be the largest line on your bill — object storage, egress, " +
			"Workers requests — and an unknown one is a gap in this price book rather than a statement " +
			"about the resource. Nothing here estimates how much money the unpriced share represents; " +
			"doing that would require exactly the consumption data an inventory does not have.",
		Rows: rows,
	}}
}

// commonestDetail picks the most frequent explanation among assets of one type
// and says how many others there were — the same shape internal/cost's
// commonestReason produces, so one unknown shape does not look like five.
func commonestDetail(reasons map[string]int) string {
	keys := make([]string, 0, len(reasons))
	for r := range reasons {
		keys = append(keys, r)
	}
	sort.Strings(keys)

	var (
		best  string
		bestN int
	)
	for _, r := range keys {
		if reasons[r] > bestN {
			best, bestN = r, reasons[r]
		}
	}
	if best == "" {
		best = "no reason recorded"
	}
	if others := len(reasons) - 1; others > 0 {
		best += fmt.Sprintf(" (+%d other reason%s)", others, plural(others))
	}
	return best
}

// ----------------------------------------------------------------------
// shared
// ----------------------------------------------------------------------

// refOf returns an asset's reference as a pointer for a Row. A helper rather
// than an inline `ref := a.AsRef(); &ref` at nine call sites, which is three
// lines of ceremony each and one chance to capture the wrong loop variable.
func refOf(a core.Asset) *core.AssetRef {
	ref := a.AsRef()
	return &ref
}
