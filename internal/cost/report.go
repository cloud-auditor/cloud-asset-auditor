package cost

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/pricing"
)

// The buffered half of the feature: rollups, the unpriced accounting, and the
// two attributions that need the whole asset set (Kubernetes in kube.go, mesh
// seats below).
//
// The design constraint that shapes everything here is that a total which
// silently omits a third of the estate is the failure mode. So every asset
// lands in exactly one of four buckets — priced, metered, unknown, attributed —
// the buckets are reported next to the total rather than below it, and
// Totals.Balances asserts they add back up to the asset count.

// staleAfter is when a price book starts warning about its own age. Ninety days
// is long enough that a hand-transcribed book is unlikely to have moved, and
// short enough that a year-old binary says so.
const staleAfter = 90 * 24 * time.Hour

// Options configures a report.
type Options struct {
	// GroupBy is provider|type|region|account|tag:KEY. Empty means provider.
	GroupBy string
	// TopN caps the most-expensive-assets list. Zero means all, matching the
	// --top flag's documented sentinel.
	TopN int
	// ShowUnpriced lists every unpriced asset individually instead of only
	// counting them by type.
	ShowUnpriced bool
	// Now fixes the report timestamp and the book-staleness comparison. Zero
	// means time.Now, and tests set it to keep output deterministic.
	Now time.Time
}

func (o Options) normalized() Options {
	if o.GroupBy == "" {
		o.GroupBy = "provider"
	}
	if o.Now.IsZero() {
		o.Now = time.Now().UTC()
	}
	return o
}

// ParseGroupBy validates a --group-by value, returning it lower-cased. Exported
// so the CLI rejects a typo before running a ten-minute audit rather than after.
func ParseGroupBy(dim string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(dim))
	switch {
	case d == "":
		return "provider", nil
	case d == "provider", d == "type", d == "region", d == "account", d == "account_id":
		return d, nil
	case strings.HasPrefix(d, "tag:"):
		if len(d) == len("tag:") {
			return "", fmt.Errorf("invalid --group-by %q: tag key is empty (want tag:KEY)", dim)
		}
		// Tag keys are case-sensitive, so only the prefix is normalised.
		return "tag:" + strings.TrimSpace(dim)[len("tag:"):], nil
	}
	return "", fmt.Errorf("invalid --group-by %q (want provider|type|region|account|tag:KEY)", dim)
}

// Report is the whole answer, and the shape the JSON renderer and the HTTP API
// serialise. Disclaimer is a required field rather than an optional one: a
// consumer that drops it has to do so deliberately.
type Report struct {
	GeneratedAt     time.Time        `json:"generated_at"`
	Disclaimer      string           `json:"disclaimer"`
	DisclaimerShort string           `json:"disclaimer_short"`
	Books           []pricing.Source `json:"books"`
	StaleBooks      []pricing.Source `json:"stale_books,omitempty"`
	HoursPerMonth   float64          `json:"hours_per_month"`

	GroupBy  string       `json:"group_by"`
	Totals   Totals       `json:"totals"`
	Groups   []Group      `json:"by_group"`
	Top      []AssetCost  `json:"top"`
	Assets   []AssetCost  `json:"assets"`
	Unpriced Unpriced     `json:"unpriced"`
	Mesh     []MeshRollup `json:"mesh,omitempty"`
	// Kubernetes is nil when the audit contained no nodes or pods.
	Kubernetes *KubeSection `json:"kubernetes,omitempty"`
	// Notes are report-level caveats that only apply to this particular run —
	// a second currency in the ranking, a book older than staleAfter.
	Notes []string `json:"notes,omitempty"`
}

// Totals is the coverage accounting. The four counts exist so that no number in
// this report can be shown without its denominator.
type Totals struct {
	Assets int `json:"assets"`
	// Priced assets contribute to Monthly.
	Priced int `json:"priced"`
	// Measured is the subset of Priced that came from a provider's billing API.
	Measured int `json:"measured"`
	// Metered assets have a known, consumption-based billing model that an
	// inventory cannot evaluate. Permanent.
	Metered int `json:"metered"`
	// Unknown assets matched no rule, shape or quantity source. A gap in the
	// book, and a pull request away from being fixed.
	Unknown int `json:"unknown"`
	// Attributed assets carry a figure that is a share of another asset's cost.
	// It is deliberately excluded from Monthly — see ValueAttributed.
	Attributed int `json:"attributed"`

	Monthly []Money `json:"monthly"`
}

// Balances reports whether every asset landed in exactly one bucket. The
// accounting is the point of the report, so it is asserted rather than assumed.
func (t Totals) Balances() bool {
	return t.Priced+t.Metered+t.Unknown+t.Attributed == t.Assets
}

// UnpricedCount is how many assets contribute nothing to the total.
func (t Totals) UnpricedCount() int { return t.Metered + t.Unknown + t.Attributed }

// UnpricedPct is that count as a percentage, for the line that always sits
// directly beneath the total.
func (t Totals) UnpricedPct() float64 {
	if t.Assets == 0 {
		return 0
	}
	return float64(t.UnpricedCount()) / float64(t.Assets) * 100
}

// Group is one row of the by-provider (or by-type, by-region, ...) rollup.
type Group struct {
	Key        string  `json:"key"`
	Assets     int     `json:"assets"`
	Priced     int     `json:"priced"`
	Metered    int     `json:"metered"`
	Unknown    int     `json:"unknown"`
	Attributed int     `json:"attributed"`
	Monthly    []Money `json:"monthly"`
}

// AssetCost is one asset's line in the report. Monthly is a string in every
// format including JSON — "412.90", "~8.50", "unknown", "metered",
// "attributed" — so that a consumer cannot default a missing key to zero or
// quietly sum an estimate alongside an invoice.
type AssetCost struct {
	Provider  string `json:"provider"`
	AccountID string `json:"account_id,omitempty"`
	Region    string `json:"region,omitempty"`
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Monthly   string `json:"monthly"`
	Currency  string `json:"currency,omitempty"`
	Basis     Basis  `json:"basis"`
	Detail    string `json:"detail,omitempty"`

	// amount is the sortable figure. Unexported on purpose: it must not reach
	// any serialised surface, where it would be a number an estimate should
	// never present as.
	amount float64
}

func assetCost(a core.Asset, e Estimate) AssetCost {
	return AssetCost{
		Provider:  a.Provider,
		AccountID: a.AccountID,
		Region:    a.Region,
		Type:      a.Type,
		ID:        a.ID,
		Name:      a.Name,
		Monthly:   e.MonthlyString(),
		Currency:  currencyOf(e),
		Basis:     e.Basis,
		Detail:    normalizeSpace(e.Detail),
		amount:    e.Monthly,
	}
}

func currencyOf(e Estimate) string {
	if e.Priced || e.Attributed {
		return e.Currency
	}
	return ""
}

// Unpriced is the accounting for everything that contributes no money. It is
// required, not optional: without it a report that priced 12% of an estate
// looks exactly like one that priced all of it.
type Unpriced struct {
	Metered    UnpricedBucket `json:"metered"`
	Unknown    UnpricedBucket `json:"unknown"`
	Attributed UnpricedBucket `json:"attributed"`
}

// Assets is the total number of unpriced assets across the three buckets.
func (u Unpriced) Assets() int {
	return u.Metered.Assets + u.Unknown.Assets + u.Attributed.Assets
}

// UnpricedBucket groups one class of unpriced asset by type, commonest first.
type UnpricedBucket struct {
	Assets int            `json:"assets"`
	Types  []UnpricedType `json:"types"`
}

// UnpricedType is one asset type that could not be priced, with the reason.
type UnpricedType struct {
	Type   string `json:"type"`
	Assets int    `json:"assets"`
	// Reason is the commonest explanation among assets of this type. When they
	// disagree — two different unknown shapes, say — the count of the others is
	// appended rather than dropped.
	Reason string `json:"reason"`
	// Examples is populated only with Options.ShowUnpriced.
	Examples []AssetCost `json:"examples,omitempty"`
}

// Report prices every asset, attributes what has to be attributed across the
// whole set, and rolls the result up.
//
// This is the buffered path. It is a reporting command in the same family as
// `auditor diff` and `auditor check`, which emit a summary rather than a
// stream; the streaming half of cost is Estimator.Annotate.
func (e *Estimator) Report(assets []core.Asset, opts Options) *Report {
	opts = opts.normalized()

	est := make([]Estimate, len(assets))
	for i, a := range assets {
		est[i] = e.Estimate(a)
	}

	// Whole-set attribution, before any rollup: it rewrites node and pod
	// estimates, and the accounting has to reflect the rewritten ones.
	kube := e.attributeKubernetes(assets, est, opts)

	rep := &Report{
		GeneratedAt:     opts.Now,
		Disclaimer:      Disclaimer,
		DisclaimerShort: DisclaimerShort,
		GroupBy:         opts.GroupBy,
		Kubernetes:      kube,
		Assets:          make([]AssetCost, 0, len(assets)),
	}
	if e.book != nil {
		rep.Books = e.book.Books
		rep.StaleBooks = e.book.Stale(opts.Now, staleAfter)
		rep.HoursPerMonth = e.book.HoursPerMonth
	}

	total := newMoneyBag()
	groups := map[string]*Group{}
	groupMoney := map[string]*moneyBag{}
	metered := newUnpricedBuilder()
	unknown := newUnpricedBuilder()
	attributed := newUnpricedBuilder()
	var priced []AssetCost

	for i, a := range assets {
		ac := assetCost(a, est[i])
		rep.Assets = append(rep.Assets, ac)

		key := groupKey(a, opts.GroupBy)
		g := groups[key]
		if g == nil {
			g = &Group{Key: key}
			groups[key] = g
			groupMoney[key] = newMoneyBag()
		}
		g.Assets++
		rep.Totals.Assets++

		switch {
		case est[i].Priced:
			g.Priced++
			rep.Totals.Priced++
			if est[i].Basis == BasisMeasured {
				rep.Totals.Measured++
			}
			total.add(est[i])
			groupMoney[key].add(est[i])
			priced = append(priced, ac)
		case est[i].Attributed:
			g.Attributed++
			rep.Totals.Attributed++
			attributed.add(ac)
		case est[i].Basis == BasisUnpriceable:
			g.Metered++
			rep.Totals.Metered++
			metered.add(ac)
		default:
			g.Unknown++
			rep.Totals.Unknown++
			unknown.add(ac)
		}
	}

	rep.Totals.Monthly = total.sorted()
	rep.Groups = sortedGroups(groups, groupMoney)
	rep.Top = topN(priced, opts.TopN)
	rep.Unpriced = Unpriced{
		Metered:    metered.build(opts.ShowUnpriced),
		Unknown:    unknown.build(opts.ShowUnpriced),
		Attributed: attributed.build(opts.ShowUnpriced),
	}
	rep.Mesh = e.meshRollup(assets)
	rep.Notes = e.reportNotes(rep, priced)
	return rep
}

func (e *Estimator) reportNotes(rep *Report, priced []AssetCost) []string {
	var notes []string
	if len(rep.Totals.Monthly) > 1 {
		// Ranking across currencies would sort a EUR figure against a USD one
		// as if they were comparable, which they are not without a rate this
		// tool refuses to apply.
		notes = append(notes, "More than one currency is present. Totals are segregated by currency "+
			"and never combined; the top-N ranking sorts on the bare number, so compare within a currency only.")
	}
	for _, s := range rep.StaleBooks {
		notes = append(notes, fmt.Sprintf("Price book %q has vintage %q, older than %d days — "+
			"run `auditor cost --refresh-prices`, or re-check the published pages it was transcribed from.",
			s.ID, s.Vintage, int(staleAfter.Hours()/24)))
	}
	if len(priced) == 0 && rep.Totals.Assets > 0 {
		notes = append(notes, "Nothing could be priced. That is a statement about this price book's coverage "+
			"and about which providers ran — it is not a statement that the estate is free.")
	}
	return notes
}

// groupKey resolves one asset's group. An absent value groups under "(none)"
// rather than the empty string, so a rollup row is never a blank line.
func groupKey(a core.Asset, dim string) string {
	var v string
	switch {
	case dim == "provider":
		v = a.Provider
	case dim == "type":
		v = a.Type
	case dim == "region":
		v = a.Region
	case dim == "account", dim == "account_id":
		v = a.AccountID
	case strings.HasPrefix(dim, "tag:"):
		v = a.Tags[dim[len("tag:"):]]
	default:
		v = a.Provider
	}
	if v == "" {
		return "(none)"
	}
	return v
}

// sortedGroups orders rollup rows by money descending, then by key, so the
// expensive things are at the top and the order is stable across runs.
func sortedGroups(groups map[string]*Group, money map[string]*moneyBag) []Group {
	out := make([]Group, 0, len(groups))
	for k, g := range groups {
		g.Monthly = money[k].sorted()
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := groupTotal(out[i]), groupTotal(out[j])
		if a != b {
			return a > b
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func groupTotal(g Group) float64 {
	var sum float64
	for _, m := range g.Monthly {
		sum += m.Measured + m.Estimated
	}
	return sum
}

// topN ranks the most expensive assets. n <= 0 returns them all, matching the
// --top flag's documented sentinel.
func topN(in []AssetCost, n int) []AssetCost {
	out := make([]AssetCost, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].amount != out[j].amount {
			return out[i].amount > out[j].amount
		}
		return out[i].ID < out[j].ID
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// unpricedBuilder accumulates one bucket of the unpriced accounting.
type unpricedBuilder struct {
	total   int
	byType  map[string]int
	reasons map[string]map[string]int
	items   map[string][]AssetCost
	order   []string
}

func newUnpricedBuilder() *unpricedBuilder {
	return &unpricedBuilder{
		byType:  map[string]int{},
		reasons: map[string]map[string]int{},
		items:   map[string][]AssetCost{},
	}
}

func (b *unpricedBuilder) add(ac AssetCost) {
	if _, seen := b.byType[ac.Type]; !seen {
		b.order = append(b.order, ac.Type)
		b.reasons[ac.Type] = map[string]int{}
	}
	b.total++
	b.byType[ac.Type]++
	b.reasons[ac.Type][ac.Detail]++
	b.items[ac.Type] = append(b.items[ac.Type], ac)
}

func (b *unpricedBuilder) build(withExamples bool) UnpricedBucket {
	out := UnpricedBucket{Assets: b.total}
	for _, t := range b.order {
		ut := UnpricedType{Type: t, Assets: b.byType[t], Reason: commonestReason(b.reasons[t])}
		if withExamples {
			ut.Examples = b.items[t]
		}
		out.Types = append(out.Types, ut)
	}
	sort.SliceStable(out.Types, func(i, j int) bool {
		if out.Types[i].Assets != out.Types[j].Assets {
			return out.Types[i].Assets > out.Types[j].Assets
		}
		return out.Types[i].Type < out.Types[j].Type
	})
	return out
}

// commonestReason picks the most frequent explanation for a type and says how
// many others there were. Collapsing to one reason keeps the summary readable;
// naming the count of the rest keeps it from hiding a second, different problem
// (one unknown shape looks the same as five distinct ones otherwise).
func commonestReason(reasons map[string]int) string {
	var best string
	var bestN int
	for r, n := range reasons {
		if n > bestN || (n == bestN && r < best) {
			best, bestN = r, n
		}
	}
	if others := len(reasons) - 1; others > 0 {
		best += fmt.Sprintf(" (+%d other reason%s)", others, plural(others))
	}
	return best
}

// Money is one currency's share of a total, split by whether it was measured or
// estimated. The split is not cosmetic: "$1,204.55 + ~$209.14" says something
// materially different from "$1,413.69", and collapsing the two would launder
// an estimate into an invoice.
type Money struct {
	Currency  string
	Measured  float64
	Estimated float64
}

// add folds one estimate in, adopting its currency the first time. A Money is
// single-currency by construction — moneyBag is what keeps several apart — so
// the first estimate names it and later ones of a different currency would be a
// caller bug rather than something to silently convert.
func (m *Money) add(e Estimate) {
	if m.Currency == "" {
		m.Currency = e.Currency
	}
	if e.Basis == BasisMeasured {
		m.Measured += e.Monthly
		return
	}
	m.Estimated += e.Monthly
}

// Total is the combined figure, for sorting only. Renderers print String.
func (m Money) Total() float64 { return m.Measured + m.Estimated }

// String renders the pair with the ~ glyph on the estimated half only.
func (m Money) String() string {
	sym := currencySymbol(m.Currency)
	switch {
	case m.Measured > 0 && m.Estimated > 0:
		return sym + withCommas(m.Measured) + " + " + EstimateMark + sym + withCommas(m.Estimated)
	case m.Measured > 0:
		return sym + withCommas(m.Measured)
	case m.Estimated > 0:
		return EstimateMark + sym + withCommas(m.Estimated)
	default:
		return NoMoney
	}
}

// NoMoney is what an aggregate with nothing in it renders as, on every surface
// and in every format. An em dash cannot be mistaken for a figure the way
// "0.00" can, and every rollup in this package that could otherwise print a
// zero prints this instead.
//
// Note the asymmetry with a single asset, which is deliberate. An asset whose
// rule zeroed it on status renders "~0.00" and carries the zero_note that says
// why; that is a claim this tool is willing to make about one resource. A
// rollup has no such note to carry — "0.00" against a cluster where nothing
// could be attributed is indistinguishable from "0.00" against a cluster that
// genuinely costs nothing, so it makes no claim at all.
const NoMoney = "—"

// MarshalJSON emits strings, never numbers, for the same reason MonthlyString
// does: a consumer that wants to add these up has to notice the tilde first.
func (m Money) MarshalJSON() ([]byte, error) {
	out := struct {
		Currency  string `json:"currency"`
		Measured  string `json:"measured,omitempty"`
		Estimated string `json:"estimated,omitempty"`
		Display   string `json:"display"`
	}{Currency: m.Currency, Display: m.String()}
	if m.Measured > 0 {
		out.Measured = formatAmount(m.Measured)
	}
	if m.Estimated > 0 {
		out.Estimated = EstimateMark + formatAmount(m.Estimated)
	}
	return json.Marshal(out)
}

// Estimated is a single estimated figure — one that is always list-price
// derived, so it always carries the glyph. It exists so that figures which are
// not a Money split (a pod attribution, idle node capacity) still serialise as
// strings rather than as bare JSON numbers: those are precisely the numbers a
// dashboard would otherwise add to an invoice.
type Estimated struct {
	Currency string
	Amount   float64
}

// String renders the figure, or NoMoney when there is none. The guard is the
// whole point: a zero-valued Estimated is what a cluster where nothing could be
// attributed produces, and "~$0.00" against 29 unattributed pods reads as "these
// pods are free" rather than "this tool attributed nothing".
func (e Estimated) String() string {
	if e.Amount <= 0 {
		return NoMoney
	}
	return EstimateMark + currencySymbol(e.Currency) + withCommas(e.Amount)
}

// MarshalJSON emits strings only, matching Money. monthly is always present and
// is never parseable as a number when there is no money — an absent key is
// worse, because a consumer that defaults a missing key to 0 gets exactly the
// zero this package refuses to state.
func (e Estimated) MarshalJSON() ([]byte, error) {
	monthly := NoMoney
	if e.Amount > 0 {
		monthly = EstimateMark + formatAmount(e.Amount)
	}
	return json.Marshal(struct {
		Currency string `json:"currency,omitempty"`
		Monthly  string `json:"monthly"`
		Display  string `json:"display"`
	}{e.Currency, monthly, e.String()})
}

// moneyBag segregates money by currency. There is no exchange rate anywhere in
// this tool, so a grand total across currencies is not a thing that can be
// computed — only a list of per-currency totals.
type moneyBag struct {
	byCurrency map[string]*Money
}

func newMoneyBag() *moneyBag { return &moneyBag{byCurrency: map[string]*Money{}} }

func (b *moneyBag) add(e Estimate) {
	cur := e.Currency
	if cur == "" {
		cur = pricing.DefaultCurrency
	}
	m := b.byCurrency[cur]
	if m == nil {
		m = &Money{Currency: cur}
		b.byCurrency[cur] = m
	}
	m.add(e)
}

func (b *moneyBag) sorted() []Money {
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

// Mesh seat pricing. Tailscale and NetBird are the one provider family where
// this tool has genuinely sufficient data: both bill per user seat, and users
// are assets it already collects.
//
// The rate ids are named here because a rollup is code, not a rule — there is
// no per-asset shape for "the tailnet's seat count". They must match
// books/mesh.yaml; a rename there without one here degrades the section to
// counts, which is why every lookup below is checked rather than assumed.
const (
	rateTailscaleStandard = "ts.seat.standard"
	rateTailscalePremium  = "ts.seat.premium"
	rateTailscaleTagged   = "ts.tagged_resource"
	rateNetbirdTeam       = "nb.seat.team"
	rateNetbirdBusiness   = "nb.seat.business"
)

// MeshRollup is one tailnet's or one NetBird account's seat bill. It is
// deliberately its own section and is NOT folded into Totals: neither vendor
// exposes which plan you are on, and picking one to make the grand total tidy
// would be inventing the single most consequential number in the section.
type MeshRollup struct {
	Provider string      `json:"provider"`
	Account  string      `json:"account"`
	Counts   []MeshCount `json:"counts"`
	Plans    []MeshPlan  `json:"plans"`
	Notes    []string    `json:"notes,omitempty"`
}

// MeshCount is one countable input to the rollup, named so a reader can check
// it against the asset table.
type MeshCount struct {
	Label  string `json:"label"`
	Assets int    `json:"assets"`
}

// MeshPlan is what the account would cost on one of the vendor's plans.
type MeshPlan struct {
	Plan     string `json:"plan"`
	Currency string `json:"currency"`
	Monthly  string `json:"monthly"`
	Detail   string `json:"detail"`
}

func (e *Estimator) meshRollup(assets []core.Asset) []MeshRollup {
	if e.book == nil {
		return nil
	}
	type counts struct {
		users, devices, tagged, peers int
	}
	byAccount := map[string]*counts{}
	order := []string{}
	touch := func(provider, account string) *counts {
		key := provider + "\x00" + account
		c := byAccount[key]
		if c == nil {
			c = &counts{}
			byAccount[key] = c
			order = append(order, key)
		}
		return c
	}
	for _, a := range assets {
		switch a.Type {
		case "tailscale.user":
			touch(a.Provider, a.AccountID).users++
		case "tailscale.device":
			c := touch(a.Provider, a.AccountID)
			c.devices++
			if a.Tags["acl_tags"] != "" {
				c.tagged++
			}
		case "netbird.user":
			touch(a.Provider, a.AccountID).users++
		case "netbird.peer":
			touch(a.Provider, a.AccountID).peers++
		}
	}
	sort.Strings(order)

	var out []MeshRollup
	for _, key := range order {
		provider, account, _ := strings.Cut(key, "\x00")
		c := byAccount[key]
		switch provider {
		case "tailscale":
			out = append(out, e.tailscaleRollup(account, c.users, c.devices, c.tagged))
		case "netbird":
			out = append(out, e.netbirdRollup(account, c.users, c.peers))
		}
	}
	return out
}

func (e *Estimator) tailscaleRollup(account string, users, devices, tagged int) MeshRollup {
	r := MeshRollup{
		Provider: "tailscale",
		Account:  account,
		Counts: []MeshCount{
			{Label: "user seats (tailscale.user)", Assets: users},
			{Label: "devices (tailscale.device)", Assets: devices},
			{Label: "tagged resources (non-empty acl_tags)", Assets: tagged},
		},
		Notes: []string{
			"Seats are counted from every tailscale.user asset. The API does not say which " +
				"users are billable, so suspended, shared and invited accounts are included here.",
			"The plan is not exposed by the API — both tiers are shown. Pick yours.",
		},
	}
	tagRate, hasTag := e.book.Rate(rateTailscaleTagged)
	for _, id := range []string{rateTailscaleStandard, rateTailscalePremium} {
		seat, ok := e.book.Rate(id)
		if !ok || users == 0 {
			continue
		}
		monthly := float64(users) * e.book.MonthlyAmount(seat)
		detail := fmt.Sprintf("%d seats x %s%s", users, currencySymbol(e.book.CurrencyOf(seat)), formatAmount(seat.Amount))
		if hasTag && tagged > 0 {
			monthly += float64(tagged) * e.book.MonthlyAmount(tagRate)
			detail += fmt.Sprintf(" + %d tagged x %s%s", tagged,
				currencySymbol(e.book.CurrencyOf(tagRate)), formatAmount(tagRate.Amount))
		}
		r.Plans = append(r.Plans, MeshPlan{
			Plan:     strings.TrimPrefix(id, "ts.seat."),
			Currency: e.book.CurrencyOf(seat),
			Monthly:  EstimateMark + formatAmount(monthly),
			Detail:   detail,
		})
	}
	if hasTag && tagRate.TierNote != "" && tagged > 0 {
		// Priced at the marginal rate like everything else, so a tailnet inside
		// the included allowance is over-estimated. Saying so is cheaper than
		// duplicating the allowance size out of the price book and letting the
		// two drift.
		r.Notes = append(r.Notes, "Tagged resources are priced at the marginal rate: "+tagRate.TierNote)
	}
	return r
}

func (e *Estimator) netbirdRollup(account string, users, peers int) MeshRollup {
	r := MeshRollup{
		Provider: "netbird",
		Account:  account,
		Counts: []MeshCount{
			{Label: "user seats (netbird.user)", Assets: users},
			{Label: "peers (netbird.peer)", Assets: peers},
		},
		Notes: []string{
			"Seats are counted from every netbird.user asset, including service users — the API " +
				"does not say which of them NetBird bills for.",
			"The plan is not exposed by the API — both tiers are shown. Pick yours.",
		},
	}
	for _, id := range []string{rateNetbirdTeam, rateNetbirdBusiness} {
		seat, ok := e.book.Rate(id)
		if !ok || users == 0 {
			continue
		}
		cur := e.book.CurrencyOf(seat)
		r.Plans = append(r.Plans, MeshPlan{
			Plan:     strings.TrimPrefix(id, "nb.seat."),
			Currency: cur,
			Monthly:  EstimateMark + formatAmount(float64(users)*e.book.MonthlyAmount(seat)),
			Detail: fmt.Sprintf("%d seats x %s%s (peer allowance not applied — see notes)",
				users, currencySymbol(cur), formatAmount(seat.Amount)),
		})
		if seat.Note != "" {
			r.Notes = append(r.Notes, strings.TrimPrefix(id, "nb.seat.")+": "+seat.Note)
		}
	}
	if peers > 0 {
		r.Notes = append(r.Notes, "Machine overage is not estimated: the included allowance depends on the "+
			"plan and the seat count, and charging for peers inside it would invent money.")
	}
	// Currencies are reported as published. See the disclaimer.
	r.Notes = append(r.Notes, "NetBird publishes in EUR. No exchange rate is applied anywhere in this tool, "+
		"and EUR totals are never combined with USD ones.")
	return r
}

// currencySymbol is a display convenience only; the currency code always
// travels alongside it in the data. An unrecognised code prints as the code
// itself, which is safer than guessing a glyph.
func currencySymbol(code string) string {
	switch strings.ToUpper(code) {
	case "USD":
		return "$"
	case "EUR":
		return "€"
	case "GBP":
		return "£"
	case "":
		return ""
	default:
		return code + " "
	}
}

// withCommas groups the integer part of a money figure in threes. Hand-rolled
// because this is the only place the project formats money, and a locale
// library would be a new dependency for one function.
func withCommas(v float64) string {
	intPart, frac, _ := strings.Cut(formatAmount(v), ".")
	out := groupDigits(intPart)
	if frac != "" {
		out += "." + frac
	}
	return out
}

func groupDigits(s string) string {
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

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
