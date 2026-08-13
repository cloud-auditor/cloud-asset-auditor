// Package cost turns an inventory into an estimate of what it costs to run,
// and is far more careful about what it refuses to claim than about what it
// reports.
//
// This package exists to say "here are 47 resources that appear to be doing
// nothing, and here is the exact join key to price them properly."
// It must never say "your bill is $3,200."
//
// # The shape of the feature
//
// Cost splits cleanly into a part that streams and a part that cannot, and this
// package refuses to blur them.
//
// Stage A is per-asset annotation: Estimator.Estimate is a pure function of one
// asset plus the price book, and Estimator.Annotate is the channel middleware
// that stamps its answer onto the stream (see annotate.go). It has the same
// shape as filter.Chan — O(1) memory, no lookahead, context-aware — so
// `audit --cost` keeps the project's streaming invariant intact.
//
// Stage B is cross-asset attribution and lives in report.go and kube.go. Three
// things are irreducibly whole-set: Kubernetes pod attribution needs every Node
// before it can price any Pod, mesh seat pricing needs the user count before a
// per-seat figure means anything, and a rollup is a reduction by definition.
// Those belong to the buffered `auditor cost` report path, in the same family
// as `auditor diff` and `auditor check` — commands that were always buffered
// because they emit a summary rather than a stream. That is not a new exception
// to the streaming invariant; it is the category those commands already occupy.
//
// # The one rule that outranks the rest
//
// An unpriced resource must never render as 0. "$0.00" is a real price in
// OCI's feed — it is what the Always Free tier costs — so zero and unknown have
// to be impossible to confuse. Every path out of Estimate that cannot produce a
// defensible number returns the string "unknown" or "metered", never a figure,
// and the only path that produces a zero figure is a rule's zero_when_status,
// which the price book requires to carry an explanation. A number on a screen
// will be believed; a wrong one is worse than none.
package cost

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/pricing"
)

// Basis is an alias, not a redefinition: price-book rules name these values
// directly in YAML (`basis: unpriceable`, `else_basis: unknown`), so a parallel
// type here would be two vocabularies for one idea.
type Basis = pricing.Basis

// Re-exported so callers can talk about bases without importing pricing.
const (
	BasisMeasured    = pricing.BasisMeasured
	BasisInferred    = pricing.BasisInferred
	BasisAssumed     = pricing.BasisAssumed
	BasisUnpriceable = pricing.BasisUnpriceable
	BasisUnknown     = pricing.BasisUnknown
)

// The four tags cost rides on. core.Asset gains no fields; Tags is the one
// thing that already flows through every renderer, the SQLite cache, the SSE
// API, --filter and --sheet-by, so four keys buy a cost column in every output
// this project owns for free.
//
// There are exactly four. Resist a fifth: each one has to be explained on every
// surface, and the marginal key is always the one that turns a legible row into
// a wall of metadata.
const (
	TagMonthly  = "cost.monthly"
	TagCurrency = "cost.currency"
	TagBasis    = "cost.basis"
	TagDetail   = "cost.detail"
)

// TagPrefix is the reserved namespace. internal/diff must skip keys carrying it
// when comparing snapshots — a price book has a vintage independent of the
// asset snapshot's, so re-pricing an unchanged tenancy would otherwise report
// drift on every asset.
const TagPrefix = "cost."

// The non-numeric values of TagMonthly. Each states a different fact, and the
// difference between the first two is the most important distinction in the
// feature: "we cannot price this because it is metered" is permanent, "we have
// no rate for this" is a pull request to the price book. Neither is ever 0.
const (
	// ValueUnknown — no rule, no shape, or no quantity source matched. A gap in
	// this book, not a free resource.
	ValueUnknown = "unknown"
	// ValueMetered — the billing model is known and is consumption-based, so it
	// is structurally underivable from an inventory.
	ValueMetered = "metered"
	// ValueAttributed — a figure exists but is a share of another asset's cost
	// rather than spend of its own (a Pod against its Node). It is deliberately
	// not a number here: an attributed figure that any consumer could sum would
	// double-count the node it came from. The figure itself is in TagDetail and
	// in the report's Kubernetes section, where it can be labelled.
	ValueAttributed = "attributed"
)

// EstimateMark is the glyph, and it does more work than any other character in
// this feature: a leading ~ means estimated from public list price, no ~ means
// the provider's billing API said so. It survives copy-paste, screenshots,
// Slack, CSV and raw JSON, which is more than can be said for any banner.
const EstimateMark = "~"

// Estimate is one asset's answer. It carries a number only when a number is
// defensible; Priced is the discriminator, and callers must consult it rather
// than testing Monthly against zero.
type Estimate struct {
	// Monthly is meaningful only when Priced or Attributed.
	Monthly float64
	// Priced reports that Monthly is this asset's own monthly cost.
	Priced bool
	// Attributed reports that Monthly is a share of some other asset's cost.
	// Such a figure never enters a total — see ValueAttributed.
	Attributed bool
	// Currency is set whenever Monthly is meaningful. It is never converted:
	// NetBird publishes in EUR and everything else in USD, and a stale exchange
	// rate would be a second invisible source of wrongness on top of an estimate.
	Currency string
	Basis    Basis
	// Detail is one greppable human string carrying the SKUs, rates and
	// quantities used, or the reason there is no number.
	Detail string
}

// MonthlyString is the value of the cost.monthly tag and the string every
// renderer prints. It is a string in every format including JSON, so that a
// machine consumer cannot accidentally do arithmetic on an estimate: parsing
// "~8.50" forces a decision about the tilde, and that friction is the feature.
func (e Estimate) MonthlyString() string {
	switch {
	case e.Attributed:
		return ValueAttributed
	case !e.Priced && e.Basis == BasisUnpriceable:
		return ValueMetered
	case !e.Priced:
		return ValueUnknown
	case e.Basis == BasisMeasured:
		return formatAmount(e.Monthly)
	default:
		return EstimateMark + formatAmount(e.Monthly)
	}
}

// Estimated reports whether the figure carries the ~ glyph.
func (e Estimate) Estimated() bool {
	return (e.Priced || e.Attributed) && e.Basis != BasisMeasured
}

// Tags renders the estimate as the four cost.* tags. Currency and detail are
// omitted when empty rather than written blank, so a CSV column stays readable.
func (e Estimate) Tags() map[string]string {
	t := map[string]string{
		TagMonthly: e.MonthlyString(),
		TagBasis:   string(e.Basis),
	}
	if e.Currency != "" && (e.Priced || e.Attributed) {
		t[TagCurrency] = e.Currency
	}
	if e.Detail != "" {
		t[TagDetail] = normalizeSpace(e.Detail)
	}
	return t
}

// ApplyTo returns a copy of the asset with the cost tags stamped on. The tag
// map is copied rather than written in place: assets loaded from a snapshot or
// the SQLite cache can share a map, and annotating one must not silently
// annotate another.
func (e Estimate) ApplyTo(a core.Asset) core.Asset {
	tags := make(map[string]string, len(a.Tags)+4)
	for k, v := range a.Tags {
		tags[k] = v
	}
	for k, v := range e.Tags() {
		tags[k] = v
	}
	a.Tags = tags
	return a
}

// MeasuredCost is a provider's own billed figure for one resource, including
// the customer's negotiated discount. It is not an estimate, which is why it
// renders without the ~ glyph.
type MeasuredCost struct {
	Amount   float64
	Currency string
	// Detail should name the window the figure covers — a measured number is
	// historical, and next month's may differ.
	Detail string
}

// MeasuredIndex resolves an asset to a provider-reported cost. It is an
// interface so the OCI Usage API join can land as its own file without this one
// growing an SDK dependency, and so the degradation path (no usage-report
// permission) is a nil index rather than an error.
type MeasuredIndex interface {
	MeasuredCost(provider, id string) (MeasuredCost, bool)
}

// MeasuredMap is the trivial MeasuredIndex: a map keyed by (provider, id).
// core.Asset.ID is an OCID for every OCI asset and UsageSummary.ResourceId is
// an OCID too, so for OCI this is a primary-key join.
type MeasuredMap map[string]MeasuredCost

func measuredKey(provider, id string) string { return provider + "\x00" + id }

// Put records a measured cost for one resource.
func (m MeasuredMap) Put(provider, id string, c MeasuredCost) {
	m[measuredKey(provider, id)] = c
}

// MeasuredCost implements MeasuredIndex.
func (m MeasuredMap) MeasuredCost(provider, id string) (MeasuredCost, bool) {
	c, ok := m[measuredKey(provider, id)]
	return c, ok
}

// Estimator prices assets against a price book. It holds no mutable state, and
// a *pricing.Book is immutable once loaded, so one Estimator is safe to share
// across the annotator goroutine and the report path.
type Estimator struct {
	book     *pricing.Book
	measured MeasuredIndex
}

// Option configures an Estimator.
type Option func(*Estimator)

// WithMeasured supplies provider-reported costs, which override the estimator
// for any asset they cover.
func WithMeasured(idx MeasuredIndex) Option {
	return func(e *Estimator) { e.measured = idx }
}

// New returns an Estimator over book. Callers get a book from pricing.Default
// or pricing.LoadFile; a nil book is tolerated and prices everything as
// unknown, because failing open here would trade a missing column for a failed
// audit.
func New(book *pricing.Book, opts ...Option) *Estimator {
	e := &Estimator{book: book}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Book returns the price book in use, so a report can print its vintages.
func (e *Estimator) Book() *pricing.Book { return e.book }

// Estimate prices one asset. It is the whole of Stage A, and it is a pure
// function: same asset plus same book, same answer, no I/O.
//
// The steps mirror the specification exactly, and every early return is a
// refusal to guess rather than a failure.
func (e *Estimator) Estimate(a core.Asset) Estimate {
	if e == nil || e.book == nil {
		return unknownf("no price book is loaded")
	}

	// 1. A measured figure outranks everything. It is the customer's real,
	//    post-discount cost, so there is nothing to estimate.
	if e.measured != nil {
		if m, ok := e.measured.MeasuredCost(a.Provider, a.ID); ok {
			cur := m.Currency
			if cur == "" {
				cur = e.book.Currency
			}
			return Estimate{
				Monthly:  m.Amount,
				Priced:   true,
				Currency: cur,
				Basis:    BasisMeasured,
				Detail:   m.Detail,
			}
		}
	}

	// 2. Exact type match only. A prefix or glob fallback is how you
	//    accidentally price a VCN like a VM.
	rule, ok := e.book.Rule(a.Type)
	if !ok {
		return unknownf("no rule for type %q — a gap in the price book, not a free resource", a.Type)
	}

	// 3. A rule may state its answer outright.
	if rule.Declared() {
		return Estimate{Basis: rule.Basis, Detail: rule.Note}
	}

	res := &resolver{asset: a}

	// 4. A failing condition sends the rule to its declared else, never to a
	//    number. An absent discriminator is not evidence that it holds.
	if c := rule.Condition; c != nil {
		v, present := res.tag(c.Tag)
		if !c.Matches(v, present) {
			return Estimate{Basis: rule.ElseBasis, Detail: rule.ElseNote}
		}
	}

	// 5. Shape resolution, for compute-shaped rules.
	var (
		shape     pricing.Shape
		shapeName string
	)
	if sel := rule.ShapeFrom; sel != nil {
		name, ok := res.selector(*sel)
		if !ok || name == "" {
			return unknownf("rule %q needs a shape from %s, which this asset does not carry",
				rule.Type, selectorLabel(*sel))
		}
		shape, ok = e.book.Shape(rule.ShapeTable, name)
		if !ok {
			return unknownf("unknown shape %q — add it to the price book", name)
		}
		// A shape carrying a note and no rates is a documented gap, not a
		// missing entry. It still yields no number, but the note explains which
		// kind of nothing this is.
		if shape.OCPURate == "" {
			return unknownf("shape %q: %s", name, shape.Note)
		}
		shapeName = name
	}

	// 6-7. Sum the terms. Quantity source order is also a confidence order, and
	//      the rule's basis is the weakest contribution any term made.
	var (
		monthly  float64
		currency string
		basis    = BasisMeasured // strongest; weakened by every term
		parts    []string
		hourly   bool
		tiered   bool
	)
	for i, t := range rule.Terms {
		rate, ok := e.resolveRate(t.Rate, shape)
		if !ok {
			if t.Optional {
				// Expected for OCI's pre-Flex shapes, which bundle memory into
				// the OCPU SKU. pricing.Validate guarantees a flexible shape
				// defines both rates, so this cannot swallow a real omission.
				continue
			}
			return unknownf("rule %q term %d: shape %q defines no %s",
				rule.Type, i, shapeName, t.Rate.FromShape)
		}
		qty, src, label, ok := res.quantity(t.Quantity, shape)
		if !ok {
			return unknownf("rule %q term %d (%s): no quantity source resolved — %s",
				rule.Type, i, rateLabel(rate), quantitySources(t.Quantity))
		}
		if qty < 0 || math.IsNaN(qty) || math.IsInf(qty, 0) {
			return unknownf("rule %q term %d (%s): quantity %v from %s is not a usable number",
				rule.Type, i, rateLabel(rate), qty, label)
		}

		// Currencies are never combined, so a rule that mixes them is a book
		// defect rather than a conversion problem. Refuse rather than pick one.
		cur := e.book.CurrencyOf(rate)
		if currency == "" {
			currency = cur
		} else if cur != currency {
			return unknownf("rule %q mixes %s and %s rates; no exchange rate is applied anywhere in this tool",
				rule.Type, currency, cur)
		}

		monthly += qty * e.book.MonthlyAmount(rate)
		basis = weaker(basis, src.basis())
		parts = append(parts, fmt.Sprintf("%s x%s @%s/%s (%s)",
			rateLabel(rate), formatQuantity(qty), formatQuantity(rate.Amount), rate.Unit, label))
		hourly = hourly || rate.Unit == pricing.UnitHour
		tiered = tiered || rate.TierNote != ""
	}
	if len(parts) == 0 {
		return unknownf("rule %q resolved no terms", rule.Type)
	}
	if math.IsNaN(monthly) || math.IsInf(monthly, 0) {
		return unknownf("rule %q summed to %v", rule.Type, monthly)
	}

	detail := strings.Join(parts, " + ")
	if hourly {
		detail += fmt.Sprintf(" (%gh/mo", e.book.HoursPerMonth)
		if tiered {
			detail += ", marginal tier"
		}
		detail += ")"
	} else if tiered {
		detail += " (marginal tier)"
	}
	if shape.Note != "" {
		detail += "; shape note: " + shape.Note
	}
	if len(rule.UnpricedComponents) > 0 {
		// A partial estimate has to say what it left out, or the omission reads
		// as a claim that there is nothing else to pay.
		detail += "; excludes " + strings.Join(rule.UnpricedComponents, "; ")
	}

	// 8. Status zeroing is the only path in the whole feature that legitimately
	//    produces a zero figure, and the price book requires it to carry a note.
	if rule.ZerosAt(a.Status) {
		return Estimate{
			Monthly:  0,
			Priced:   true,
			Currency: currency,
			Basis:    basis,
			Detail:   rule.ZeroNote + " " + detail,
		}
	}

	// Anything else that sums to zero is a reading we do not trust — a size tag
	// of 0, a shape default of 0 — and reporting it as free would be exactly the
	// confusion this package exists to prevent.
	if monthly <= 0 {
		return unknownf("rule %q summed to %s, which no zero_when_status justifies: %s",
			rule.Type, formatAmount(monthly), detail)
	}

	return Estimate{
		Monthly:  monthly,
		Priced:   true,
		Currency: currency,
		Basis:    basis,
		Detail:   detail,
	}
}

// resolveRate turns a term's rate reference into a rate, following the resolved
// shape for from_shape references.
func (e *Estimator) resolveRate(ref pricing.RateRef, shape pricing.Shape) (*pricing.Rate, bool) {
	id := ref.ID
	if ref.FromShape != "" {
		var ok bool
		if id, ok = shape.RateID(ref.FromShape); !ok {
			return nil, false
		}
	}
	return e.book.Rate(id)
}

// resolver reads quantities off one asset, parsing Raw at most once. Several
// terms addressing the same Raw document is the common case (size_gb twice for
// a volume), and re-unmarshalling a Pod spec per term would be the difference
// between a fast annotation stage and a slow one.
type resolver struct {
	asset     core.Asset
	raw       map[string]any
	rawParsed bool
}

func (r *resolver) tag(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	v, ok := r.asset.Tags[key]
	return v, ok
}

// selector reads a string from a tag or a Raw path — the shape name, today.
func (r *resolver) selector(s pricing.Selector) (string, bool) {
	if s.Tag != "" {
		if v, ok := r.tag(s.Tag); ok && v != "" {
			return v, true
		}
	}
	if s.Raw != "" {
		if v, ok := r.rawValue(s.Raw); ok {
			if str, ok := v.(string); ok && str != "" {
				return str, true
			}
		}
	}
	return "", false
}

// source records where a quantity came from, which is what makes the
// difference between an inferred and an assumed estimate observable. A size
// read off the resource is a different claim from a size the book supplied.
type source int

const (
	fromTag source = iota
	fromRaw
	fromShapeDefault
	fromQuantityDefault
	fromLiteral
)

// basis maps a quantity's origin to the confidence it contributes. Tag and Raw
// read the resource's own attributes; Literal states a fact about the rule ("a
// load balancer is one load balancer"), which is no weaker. Shape and quantity
// defaults substitute a book value for something the resource did not say.
func (s source) basis() Basis {
	switch s {
	case fromShapeDefault, fromQuantityDefault:
		return BasisAssumed
	default:
		return BasisInferred
	}
}

// quantity resolves one term's quantity. The order — tag, raw, shape default,
// literal default, literal — is fixed by the price-book schema and is also the
// confidence order, strongest first.
func (r *resolver) quantity(q pricing.Quantity, shape pricing.Shape) (float64, source, string, bool) {
	val, src, label, ok := r.quantityOnce(q, shape)
	if !ok {
		return 0, 0, "", false
	}
	if q.MultiplyBy != nil {
		mul, msrc, mlabel, mok := r.quantityOnce(*q.MultiplyBy, shape)
		if !mok {
			return 0, 0, "", false
		}
		// The product is only as trustworthy as its weaker factor: a real size
		// times an assumed performance level is an assumption.
		return val * mul, weakerSource(src, msrc), label + " x " + mlabel, true
	}
	return val, src, label, true
}

func (r *resolver) quantityOnce(q pricing.Quantity, shape pricing.Shape) (float64, source, string, bool) {
	if q.Tag != "" {
		if s, ok := r.tag(q.Tag); ok {
			if v, ok := parseNumber(s); ok {
				return v, fromTag, "tag " + q.Tag, true
			}
		}
	}
	if q.Raw != "" {
		if raw, ok := r.rawValue(q.Raw); ok {
			if v, ok := numberOf(raw); ok {
				return v, fromRaw, "raw " + q.Raw, true
			}
		}
	}
	if q.Shape != "" {
		if v, ok := shape.DefaultQuantity(q.Shape); ok {
			return v, fromShapeDefault, "shape " + q.Shape, true
		}
	}
	if q.Default != nil {
		return *q.Default, fromQuantityDefault, "book default", true
	}
	if q.Literal != nil {
		return *q.Literal, fromLiteral, "literal", true
	}
	return 0, 0, "", false
}

// rawValue walks a dotted path into Asset.Raw. Raw is only present under
// --include-raw, so a miss here is routine rather than exceptional — it is what
// keeps a plain audit's compute estimates at `assumed` instead of failing.
func (r *resolver) rawValue(path string) (any, bool) {
	if !r.rawParsed {
		r.rawParsed = true
		if len(r.asset.Raw) > 0 {
			// A Raw document that does not unmarshal is not worth an error: the
			// estimate degrades to the next quantity source, which is the same
			// behaviour as Raw being absent.
			_ = json.Unmarshal(r.asset.Raw, &r.raw)
		}
	}
	cur := any(r.raw)
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	if cur == nil {
		return nil, false
	}
	return cur, true
}

// numberOf accepts a JSON number or a numeric string. SDKs disagree about which
// they emit for the same field, and rejecting the string form would make an
// estimate depend on a marshalling detail.
func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		return parseNumber(n)
	}
	return 0, false
}

func parseNumber(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// weaker returns the lower-confidence of two bases. measured > inferred >
// assumed; a rule is only as good as its weakest term.
func weaker(a, b Basis) Basis {
	if basisRank(b) < basisRank(a) {
		return b
	}
	return a
}

func weakerSource(a, b source) source {
	if basisRank(b.basis()) < basisRank(a.basis()) {
		return b
	}
	return a
}

func basisRank(b Basis) int {
	switch b {
	case BasisMeasured:
		return 3
	case BasisInferred:
		return 2
	case BasisAssumed:
		return 1
	default:
		return 0
	}
}

func unknownf(format string, args ...any) Estimate {
	return Estimate{Basis: BasisUnknown, Detail: fmt.Sprintf(format, args...)}
}

// rateLabel prefers the SKU, because a SKU is what a reader can look up in the
// provider's price list to check this tool's arithmetic.
func rateLabel(r *pricing.Rate) string {
	if r.SKU != "" {
		return r.SKU
	}
	return r.ID
}

func selectorLabel(s pricing.Selector) string {
	switch {
	case s.Tag != "" && s.Raw != "":
		return fmt.Sprintf("tag %s or raw %s", s.Tag, s.Raw)
	case s.Raw != "":
		return "raw " + s.Raw
	default:
		return "tag " + s.Tag
	}
}

// quantitySources names every place a quantity could have come from, so the
// detail on an unpriced asset says what to add rather than only that something
// is missing.
func quantitySources(q pricing.Quantity) string {
	var want []string
	if q.Tag != "" {
		want = append(want, "tag "+q.Tag)
	}
	if q.Raw != "" {
		want = append(want, "raw "+q.Raw+" (needs --include-raw)")
	}
	if q.Shape != "" {
		want = append(want, "shape "+q.Shape)
	}
	if len(want) == 0 {
		return "no source declared"
	}
	return "wanted " + strings.Join(want, ", ")
}

// smallestShown is the smallest positive figure that survives four decimals.
// Below it, four decimals round to "0.0000" — which is the rendering this
// package must never produce for something that costs money.
const smallestShown = 0.0001

// formatAmount renders money. Two decimals everywhere except for figures under
// a cent, which take four; below what four decimals can express, the figure
// becomes an inequality rather than a rounded zero. Adding decimals until
// something shows would be the other option, but a monthly cost quoted to eight
// places invites more precision than an estimate has.
func formatAmount(v float64) string {
	switch {
	case v > 0 && v < smallestShown:
		return "<" + strconv.FormatFloat(smallestShown, 'f', 4, 64)
	case v > 0 && v < 0.01:
		return strconv.FormatFloat(v, 'f', 4, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// formatQuantity renders a count or a rate without trailing zeros, so a detail
// string reads "x2 @0.025/hour" rather than "x2.000000 @0.025000/hour".
func formatQuantity(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// normalizeSpace collapses runs of whitespace into single spaces. Price-book
// notes are YAML-folded across several lines, and a tag value carrying a
// newline breaks CSV, the SSE frame format, and every table renderer.
func normalizeSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}
