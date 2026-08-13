// Package pricing is the price book behind cost estimation: the rate data, its
// schema, and the lookup surface internal/cost drives. It does no network I/O
// beyond the explicitly opt-in refresh in refresh.go, and no file I/O beyond
// reading the embedded books and any --price-book overrides.
//
// The whole language is deliberately tiny. An estimate is a sum of terms; a
// term is a quantity times a rate; a rate is a scalar price for one unit of one
// thing, per hour or per month. Everything the estimator needs to know about a
// resource type is one Rule, matched on the exact core.Asset.Type. Resist
// growing this: a richer language is one nobody can audit, and the value of a
// price book is that a reviewer can read it and say "yes, that is what Oracle
// charges".
//
// The invariant that outranks everything else here: an unpriced resource must
// never be indistinguishable from a free one. $0.00 is a real price in OCI's
// feed — it is what the Always Free tier costs — so a zero or negative rate is
// rejected at load time (see Book.Validate). A type this tool cannot price gets
// no rule, or a rule that declares why; it never gets a zero rate. The
// corollary lives in refresh.go: every amount is the *marginal* tier.
//
// A *Book is immutable once Load returns and is safe for concurrent readers.
// Nothing in this package mutates one after construction.
package pricing

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the only price-book schema this build understands. It is
// checked per document rather than after merging so a wrong-schema file is
// named in the error.
const SchemaVersion = 1

// Defaults applied after merging when no document sets them. 730 is 365*24/12,
// the conventional "average month". OCI's own Always Free allowances are quoted
// against a 744-hour (31-day) month instead, which is one more reason those
// tiers are unusable per-asset — see refresh.go.
const (
	DefaultCurrency      = "USD"
	DefaultHoursPerMonth = 730
)

// Unit is the period a Rate.Amount covers.
type Unit string

const (
	UnitHour  Unit = "hour"
	UnitMonth Unit = "month"
)

// Basis records where a billable quantity came from, and therefore how much a
// figure can be trusted. It is declared here rather than in internal/cost
// because price-book rules name these values directly (`basis: unpriceable`,
// `else_basis: unknown`); internal/cost aliases the type rather than defining a
// parallel one.
//
// The ordering that matters to the estimator is measured > inferred > assumed:
// a rule's basis is the weakest of its terms' contributions. Unpriceable and
// unknown are not points on that scale — they mean no number at all, and the
// difference between them is the most important distinction in the feature.
// Unpriceable is a permanent statement about the resource's billing model;
// unknown is a temporary statement about this book's coverage.
type Basis string

const (
	// BasisMeasured — the provider's own billing API reported this exact
	// resource's cost, including the customer's negotiated discount.
	BasisMeasured Basis = "measured"
	// BasisInferred — quantity read from the asset itself; price is list.
	BasisInferred Basis = "inferred"
	// BasisAssumed — quantity is a price-book default the asset didn't carry.
	BasisAssumed Basis = "assumed"
	// BasisUnpriceable — billing is consumption-based and structurally
	// underivable from an inventory. Renders as "metered", never as 0.
	BasisUnpriceable Basis = "unpriceable"
	// BasisUnknown — no rule, no shape, or no quantity source matched. A gap
	// in this book, not a free resource.
	BasisUnknown Basis = "unknown"
)

// Valid reports whether b is one of the five defined bases.
func (b Basis) Valid() bool {
	switch b {
	case BasisMeasured, BasisInferred, BasisAssumed, BasisUnpriceable, BasisUnknown:
		return true
	}
	return false
}

// Source is provenance for one book: where its numbers came from and when they
// were last confirmed. For generated books Vintage is the upstream feed's own
// lastUpdated; for hand-transcribed books it is the date a human checked the
// published page, which is the only staleness signal those books have.
type Source struct {
	ID          string `yaml:"id"`
	Source      string `yaml:"source"`
	Vintage     string `yaml:"vintage"`
	GeneratedBy string `yaml:"generated_by,omitempty"`
	Note        string `yaml:"note,omitempty"`
}

// VintageTime parses Vintage as either an RFC3339 timestamp (generated books,
// verbatim from the feed) or a plain YYYY-MM-DD date (hand-checked books).
func (s Source) VintageTime() (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if t, err := time.Parse(layout, s.Vintage); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Rate is a scalar price for one unit of one thing.
//
// Amount is always the MARGINAL rate — the price of the next unit, not the
// first. Where the upstream feed encodes an allowance as a cheaper first tier,
// TierNote says so. See refresh.go for why that choice is forced.
type Rate struct {
	ID       string  `yaml:"id"`
	Book     string  `yaml:"book"`
	SKU      string  `yaml:"sku,omitempty"`
	Unit     Unit    `yaml:"unit"`
	Amount   float64 `yaml:"amount"`
	Currency string  `yaml:"currency,omitempty"`
	TierNote string  `yaml:"tier_note,omitempty"`
	Note     string  `yaml:"note,omitempty"`
}

// Shape describes a provider instance shape: which rates it bills against, and
// what size to assume when the asset itself doesn't carry one.
//
// There is deliberately no wildcard or "average" entry. An unlisted shape
// yields BasisUnknown, because a guessed instance size is indistinguishable
// from a measured one once it is a number on a screen.
//
// A shape with a Note and no rates is a *documented* gap rather than a missing
// one: VM.Standard.E2.1.Micro exists only as an Always Free shape and has no
// marginal list rate to quote. It still resolves to BasisUnknown, but carrying
// the note, which reads very differently from "unknown shape — add it".
//
// MemoryRate is empty for pre-Flex shapes, where OCI bundles memory into the
// OCPU SKU. The memory term in the compute rule is marked optional for exactly
// that case; a Flexible shape must define both rates (enforced by Validate),
// so the tolerance can't quietly swallow a real omission.
type Shape struct {
	OCPURate        string  `yaml:"ocpu_rate,omitempty"`
	MemoryRate      string  `yaml:"memory_rate,omitempty"`
	Flexible        bool    `yaml:"flexible,omitempty"`
	DefaultOCPU     float64 `yaml:"default_ocpu,omitempty"`
	DefaultMemoryGB float64 `yaml:"default_memory_gb,omitempty"`
	Note            string  `yaml:"note,omitempty"`
}

// Shape field names addressable from a rule, as they appear in the YAML.
const (
	FieldOCPURate        = "ocpu_rate"
	FieldMemoryRate      = "memory_rate"
	FieldDefaultOCPU     = "default_ocpu"
	FieldDefaultMemoryGB = "default_memory_gb"
)

// RateID resolves a term's from_shape reference against this shape.
//
// The switch is exhaustive on purpose. Reflecting over yaml tags would accept a
// typo'd field name and silently drop the term, which under-prices the asset —
// a wrong number, which is worse than no number.
func (s Shape) RateID(field string) (string, bool) {
	switch field {
	case FieldOCPURate:
		return s.OCPURate, s.OCPURate != ""
	case FieldMemoryRate:
		return s.MemoryRate, s.MemoryRate != ""
	}
	return "", false
}

// DefaultQuantity resolves a quantity's shape reference — the fallback size
// used when the asset carries neither the tag nor the Raw field. Resolving here
// is what drops an estimate to BasisAssumed.
func (s Shape) DefaultQuantity(field string) (float64, bool) {
	switch field {
	case FieldDefaultOCPU:
		return s.DefaultOCPU, s.DefaultOCPU > 0
	case FieldDefaultMemoryGB:
		return s.DefaultMemoryGB, s.DefaultMemoryGB > 0
	}
	return 0, false
}

// RateRef points a term at a rate, either directly by id or indirectly through
// whichever shape the asset resolved to.
type RateRef struct {
	ID        string `yaml:"id,omitempty"`
	FromShape string `yaml:"from_shape,omitempty"`
}

// UnmarshalYAML accepts both the scalar form (`rate: oci.block.storage`) and
// the mapping form (`rate: {from_shape: ocpu_rate}`). The scalar form is by far
// the common case and spelling it out as a mapping would bury the rate id.
func (r *RateRef) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return node.Decode(&r.ID)
	}
	var aux struct {
		ID        string `yaml:"id"`
		FromShape string `yaml:"from_shape"`
	}
	if err := node.Decode(&aux); err != nil {
		return err
	}
	r.ID, r.FromShape = aux.ID, aux.FromShape
	return nil
}

// MarshalYAML re-emits the scalar form so a round trip through `just prices`
// doesn't rewrite every hand-written rule into mapping syntax.
func (r RateRef) MarshalYAML() (any, error) {
	if r.FromShape == "" {
		return r.ID, nil
	}
	return map[string]string{"from_shape": r.FromShape}, nil
}

// Quantity resolves how many units a term bills for. Exactly one source
// applies, tried in a fixed order — Tag, Raw, Shape, Default, Literal — and the
// first that resolves wins.
//
// That order is also a confidence order, and it is what makes the difference
// between inferred and assumed observable: Tag and Raw read the resource's own
// attributes, while Shape and Default substitute a book value. Literal counts
// as inferred because it states a fact about the rule rather than about the
// resource ("a load balancer is one load balancer").
//
// MultiplyBy scales the resolved quantity by a second quantity, which is how a
// per-GB rate becomes a per-VPU-per-GB rate without a second term.
type Quantity struct {
	Literal    *float64  `yaml:"literal,omitempty"`
	Tag        string    `yaml:"tag,omitempty"`
	Raw        string    `yaml:"raw,omitempty"`
	Shape      string    `yaml:"shape,omitempty"`
	Default    *float64  `yaml:"default,omitempty"`
	MultiplyBy *Quantity `yaml:"multiply_by,omitempty"`
}

// Empty reports whether this quantity names no source at all, which makes the
// whole rule unresolvable.
func (q Quantity) Empty() bool {
	return q.Literal == nil && q.Tag == "" && q.Raw == "" && q.Shape == "" && q.Default == nil
}

// Term is one addend of an estimate: a quantity times a rate.
//
// Optional tolerates a from_shape rate the resolved shape doesn't define,
// skipping the term instead of failing the rule. It exists for OCI's pre-Flex
// shapes, where memory is bundled into the OCPU SKU and a memory term is
// intentionally absent. Validate rejects Optional on a literal rate id, and
// requires Flexible shapes to define both rates, so the tolerance cannot
// quietly absorb a genuine gap.
type Term struct {
	Rate     RateRef  `yaml:"rate"`
	Quantity Quantity `yaml:"quantity"`
	Optional bool     `yaml:"optional,omitempty"`
}

// Selector names where on an asset to read a string.
type Selector struct {
	Tag string `yaml:"tag,omitempty"`
	Raw string `yaml:"raw,omitempty"`
}

// Condition gates a rule on one tag. A failing condition sends the rule to
// ElseBasis rather than to a number.
type Condition struct {
	Tag      string `yaml:"tag"`
	Equals   string `yaml:"equals,omitempty"`
	NonEmpty bool   `yaml:"non_empty,omitempty"`
}

// Matches evaluates the condition against a tag value and whether it was
// present. An absent tag never satisfies a condition — being unable to observe
// the discriminator is not evidence that it holds.
func (c Condition) Matches(value string, present bool) bool {
	if !present {
		return false
	}
	if c.NonEmpty {
		return value != ""
	}
	return value == c.Equals
}

// Rule is everything the estimator knows about one core.Asset.Type. Matching is
// by exact type, with no prefix or glob fallback — a wildcard rule is how you
// accidentally price a VCN like a VM.
type Rule struct {
	Type string `yaml:"type"`

	// Basis short-circuits the rule: the billing model is known and no
	// per-asset arithmetic applies. Only unpriceable and unknown are
	// meaningful here.
	Basis Basis  `yaml:"basis,omitempty"`
	Note  string `yaml:"note,omitempty"`

	ShapeFrom  *Selector `yaml:"shape_from,omitempty"`
	ShapeTable string    `yaml:"shape_table,omitempty"`

	Terms []Term `yaml:"terms,omitempty"`

	Condition *Condition `yaml:"condition,omitempty"`
	ElseBasis Basis      `yaml:"else_basis,omitempty"`
	ElseNote  string     `yaml:"else_note,omitempty"`

	// ZeroWhenStatus lists the statuses where the resource genuinely stops
	// billing. This is the only path in the whole feature that legitimately
	// produces 0.00, which is why ZeroNote is required alongside it: a zero
	// always arrives with its explanation attached.
	ZeroWhenStatus []string `yaml:"zero_when_status,omitempty"`
	ZeroNote       string   `yaml:"zero_note,omitempty"`

	// UnpricedComponents names consumption-based charges this rule knowingly
	// omits, so a partial estimate says what it left out.
	UnpricedComponents []string `yaml:"unpriced_components,omitempty"`
}

// Declared reports whether the rule states a basis outright instead of
// computing one from terms.
func (r *Rule) Declared() bool { return r.Basis != "" }

// ZerosAt reports whether status is one where the resource stops billing.
// Comparison is case-insensitive: provider lifecycle states are conventionally
// upper-case but that is a convention, not a guarantee.
func (r *Rule) ZerosAt(status string) bool {
	for _, s := range r.ZeroWhenStatus {
		if strings.EqualFold(s, status) {
			return true
		}
	}
	return false
}

// Book is a merged, validated, indexed price book.
type Book struct {
	Version       int                         `yaml:"version"`
	Currency      string                      `yaml:"currency,omitempty"`
	HoursPerMonth float64                     `yaml:"hours_per_month,omitempty"`
	Books         []Source                    `yaml:"books,omitempty"`
	Rates         []Rate                      `yaml:"rates,omitempty"`
	Shapes        map[string]map[string]Shape `yaml:"shapes,omitempty"`
	Rules         []Rule                      `yaml:"rules,omitempty"`

	// Indexes, built by index() after every merge. Read-only thereafter, which
	// is what makes a *Book safe to share with the streaming annotator.
	rateByID   map[string]*Rate
	ruleByType map[string]*Rule
	sourceByID map[string]*Source
}

// Document is one YAML price-book file: its bytes plus a name used only to make
// errors point at the file that caused them.
type Document struct {
	Name string
	Data []byte
}

// Load parses and merges price-book documents in order, then validates the
// result.
//
// Later documents win by identity, not by field: a rate replaces an earlier
// rate with the same id, a rule replaces an earlier rule with the same type, a
// shape replaces an earlier shape with the same table and name, and a source
// replaces one with the same id. Anything a later document doesn't mention
// survives untouched. It is deliberately not a deep merge — a rule whose terms
// came from one file and whose condition came from another is not something a
// reviewer can reason about, and reasoning about it is the whole point.
//
// Position is stable: replacing an entry keeps its original slot, so the merged
// book renders in a predictable order no matter how many overrides were passed.
func Load(docs ...Document) (*Book, error) {
	b := &Book{Version: SchemaVersion}
	for _, doc := range docs {
		var in Book
		dec := yaml.NewDecoder(bytes.NewReader(doc.Data))
		// Strict: an unrecognised key is far more likely to be a typo that
		// silently drops a rate than a forward-compatible extension.
		dec.KnownFields(true)
		if err := dec.Decode(&in); err != nil {
			return nil, fmt.Errorf("pricing: %s: %w", doc.Name, err)
		}
		if in.Version != SchemaVersion {
			return nil, fmt.Errorf("pricing: %s: schema version %d, want %d", doc.Name, in.Version, SchemaVersion)
		}
		// Duplicates have to be caught before the merge, which collapses them
		// by design: replacing an entry is the whole point of an override
		// document, but two entries with the same id *within one file* is a
		// typo, and silently keeping the last one is how a hand-edit to the
		// wrong copy goes unnoticed.
		if err := checkDuplicates(doc.Name, &in); err != nil {
			return nil, err
		}
		b.merge(&in)
	}
	if b.Currency == "" {
		b.Currency = DefaultCurrency
	}
	if b.HoursPerMonth == 0 {
		b.HoursPerMonth = DefaultHoursPerMonth
	}
	b.index()
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}

// LoadFile returns the embedded default book with each path merged over it in
// order. That composition is the --price-book contract: an override file names
// only what it changes.
func LoadFile(paths ...string) (*Book, error) {
	docs, err := defaultDocuments()
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("pricing: read price book: %w", err)
		}
		docs = append(docs, Document{Name: p, Data: data})
	}
	return Load(docs...)
}

func checkDuplicates(name string, in *Book) error {
	seen := map[string]bool{}
	for _, r := range in.Rates {
		if seen["rate:"+r.ID] {
			return fmt.Errorf("pricing: %s: rate %q declared twice", name, r.ID)
		}
		seen["rate:"+r.ID] = true
	}
	for _, r := range in.Rules {
		if seen["rule:"+r.Type] {
			return fmt.Errorf("pricing: %s: rule %q declared twice", name, r.Type)
		}
		seen["rule:"+r.Type] = true
	}
	for _, s := range in.Books {
		if seen["book:"+s.ID] {
			return fmt.Errorf("pricing: %s: book %q declared twice", name, s.ID)
		}
		seen["book:"+s.ID] = true
	}
	return nil
}

func (b *Book) merge(in *Book) {
	if in.Currency != "" {
		b.Currency = in.Currency
	}
	if in.HoursPerMonth != 0 {
		b.HoursPerMonth = in.HoursPerMonth
	}
	for _, s := range in.Books {
		if i := indexOf(b.Books, func(x Source) bool { return x.ID == s.ID }); i >= 0 {
			b.Books[i] = s
		} else {
			b.Books = append(b.Books, s)
		}
	}
	for _, r := range in.Rates {
		if i := indexOf(b.Rates, func(x Rate) bool { return x.ID == r.ID }); i >= 0 {
			b.Rates[i] = r
		} else {
			b.Rates = append(b.Rates, r)
		}
	}
	for _, r := range in.Rules {
		if i := indexOf(b.Rules, func(x Rule) bool { return x.Type == r.Type }); i >= 0 {
			b.Rules[i] = r
		} else {
			b.Rules = append(b.Rules, r)
		}
	}
	for table, shapes := range in.Shapes {
		if b.Shapes == nil {
			b.Shapes = make(map[string]map[string]Shape, len(in.Shapes))
		}
		if b.Shapes[table] == nil {
			b.Shapes[table] = make(map[string]Shape, len(shapes))
		}
		for name, s := range shapes {
			b.Shapes[table][name] = s
		}
	}
}

func (b *Book) index() {
	b.rateByID = make(map[string]*Rate, len(b.Rates))
	for i := range b.Rates {
		b.rateByID[b.Rates[i].ID] = &b.Rates[i]
	}
	b.ruleByType = make(map[string]*Rule, len(b.Rules))
	for i := range b.Rules {
		b.ruleByType[b.Rules[i].Type] = &b.Rules[i]
	}
	b.sourceByID = make(map[string]*Source, len(b.Books))
	for i := range b.Books {
		b.sourceByID[b.Books[i].ID] = &b.Books[i]
	}
}

// Rule returns the rule for an exact asset type. A miss is the estimator's
// signal to report unknown — never to fall back to something adjacent.
func (b *Book) Rule(assetType string) (*Rule, bool) {
	r, ok := b.ruleByType[assetType]
	return r, ok
}

// Rate returns a rate by id.
func (b *Book) Rate(id string) (*Rate, bool) {
	r, ok := b.rateByID[id]
	return r, ok
}

// Shape returns an entry from a named shape table.
func (b *Book) Shape(table, name string) (Shape, bool) {
	s, ok := b.Shapes[table][name]
	return s, ok
}

// Source returns provenance for a book id.
func (b *Book) Source(id string) (*Source, bool) {
	s, ok := b.sourceByID[id]
	return s, ok
}

// MonthlyAmount converts a rate to a per-month figure. Every caller must go
// through this rather than multiplying by hours itself — an hour/month mix-up
// is a 730x error that looks entirely plausible on a screen.
func (b *Book) MonthlyAmount(r *Rate) float64 {
	if r.Unit == UnitHour {
		return r.Amount * b.HoursPerMonth
	}
	return r.Amount
}

// CurrencyOf returns a rate's currency, falling back to the book default.
// NetBird publishes in EUR while everything else is USD; no conversion happens
// anywhere in this tool, so the currency has to travel with the number.
func (b *Book) CurrencyOf(r *Rate) string {
	if r.Currency != "" {
		return r.Currency
	}
	return b.Currency
}

// Stale returns the sources whose vintage is older than maxAge, plus any whose
// vintage doesn't parse — an unreadable vintage is not evidence of freshness.
func (b *Book) Stale(now time.Time, maxAge time.Duration) []Source {
	var out []Source
	for _, s := range b.Books {
		t, ok := s.VintageTime()
		if !ok || now.Sub(t) > maxAge {
			out = append(out, s)
		}
	}
	return out
}

// Validate enforces the invariants the rest of the feature relies on. It runs
// on every Load, including of user --price-book files, because a malformed
// override is exactly as dangerous as a malformed built-in.
func (b *Book) Validate() error {
	if b.HoursPerMonth <= 0 {
		return fmt.Errorf("pricing: hours_per_month must be > 0, got %v", b.HoursPerMonth)
	}
	seenRate := make(map[string]bool, len(b.Rates))
	for _, r := range b.Rates {
		if err := b.validateRate(r, seenRate); err != nil {
			return err
		}
	}
	for table, shapes := range b.Shapes {
		for name, s := range shapes {
			if err := b.validateShape(table, name, s); err != nil {
				return err
			}
		}
	}
	seenRule := make(map[string]bool, len(b.Rules))
	for i := range b.Rules {
		if err := b.validateRule(&b.Rules[i], seenRule); err != nil {
			return err
		}
	}
	return nil
}

func (b *Book) validateRate(r Rate, seen map[string]bool) error {
	switch {
	case r.ID == "":
		return fmt.Errorf("pricing: rate with empty id")
	case seen[r.ID]:
		return fmt.Errorf("pricing: rate %q declared twice", r.ID)
	case r.Book == "":
		return fmt.Errorf("pricing: rate %q: missing book", r.ID)
	case r.Unit != UnitHour && r.Unit != UnitMonth:
		return fmt.Errorf("pricing: rate %q: unit %q must be %q or %q", r.ID, r.Unit, UnitHour, UnitMonth)
	}
	seen[r.ID] = true
	if _, ok := b.sourceByID[r.Book]; !ok {
		return fmt.Errorf("pricing: rate %q: book %q has no source entry", r.ID, r.Book)
	}
	// The central invariant. A zero rate is indistinguishable from a rate we
	// failed to read, and it renders as a confident $0.00 — the one outcome
	// this feature exists to prevent. A genuinely free SKU gets no rate at all;
	// it gets a rule stating why, or a shape note.
	if math.IsNaN(r.Amount) || math.IsInf(r.Amount, 0) || r.Amount <= 0 {
		return fmt.Errorf("pricing: rate %q: amount must be > 0, got %v — "+
			"a free tier is not a rate; drop the rate and state the reason in a rule note", r.ID, r.Amount)
	}
	return nil
}

func (b *Book) validateShape(table, name string, s Shape) error {
	where := fmt.Sprintf("shape %s/%s", table, name)
	for field, id := range map[string]string{FieldOCPURate: s.OCPURate, FieldMemoryRate: s.MemoryRate} {
		if id == "" {
			continue
		}
		if _, ok := b.rateByID[id]; !ok {
			return fmt.Errorf("pricing: %s: %s %q is not a known rate", where, field, id)
		}
	}
	// A flexible shape bills memory separately by definition, so a missing
	// memory_rate there is an omission rather than OCI's pre-Flex bundling.
	// Catching it here is what lets Term.Optional stay safe.
	if s.Flexible && (s.OCPURate == "" || s.MemoryRate == "") {
		return fmt.Errorf("pricing: %s: flexible shapes must set both %s and %s", where, FieldOCPURate, FieldMemoryRate)
	}
	if s.OCPURate == "" && s.Note == "" {
		return fmt.Errorf("pricing: %s: a shape with no %s must carry a note explaining the gap", where, FieldOCPURate)
	}
	return nil
}

func (b *Book) validateRule(r *Rule, seen map[string]bool) error {
	if r.Type == "" {
		return fmt.Errorf("pricing: rule with empty type")
	}
	if seen[r.Type] {
		return fmt.Errorf("pricing: rule %q declared twice", r.Type)
	}
	seen[r.Type] = true

	if r.Declared() {
		switch {
		case r.Basis != BasisUnpriceable && r.Basis != BasisUnknown:
			return fmt.Errorf("pricing: rule %q: basis %q must be %q or %q",
				r.Type, r.Basis, BasisUnpriceable, BasisUnknown)
		case r.Note == "":
			// A declared non-price is a claim about the resource; without the
			// reason it is indistinguishable from an oversight.
			return fmt.Errorf("pricing: rule %q: basis %q requires a note", r.Type, r.Basis)
		case len(r.Terms) > 0:
			return fmt.Errorf("pricing: rule %q: basis and terms are mutually exclusive", r.Type)
		}
		return nil
	}
	if len(r.Terms) == 0 {
		return fmt.Errorf("pricing: rule %q: needs either terms or a declared basis", r.Type)
	}
	if r.Condition != nil {
		if r.Condition.Tag == "" {
			return fmt.Errorf("pricing: rule %q: condition needs a tag", r.Type)
		}
		if !r.ElseBasis.Valid() || (r.ElseBasis != BasisUnknown && r.ElseBasis != BasisUnpriceable) {
			return fmt.Errorf("pricing: rule %q: condition requires else_basis of %q or %q",
				r.Type, BasisUnknown, BasisUnpriceable)
		}
		if r.ElseNote == "" {
			return fmt.Errorf("pricing: rule %q: condition requires an else_note", r.Type)
		}
	}
	// A zero without an explanation is the failure mode; refuse to encode one.
	if len(r.ZeroWhenStatus) > 0 && r.ZeroNote == "" {
		return fmt.Errorf("pricing: rule %q: zero_when_status requires a zero_note", r.Type)
	}
	if r.ShapeFrom != nil {
		if r.ShapeFrom.Tag == "" && r.ShapeFrom.Raw == "" {
			return fmt.Errorf("pricing: rule %q: shape_from needs a tag or raw path", r.Type)
		}
		if _, ok := b.Shapes[r.ShapeTable]; !ok {
			return fmt.Errorf("pricing: rule %q: shape_table %q does not exist", r.Type, r.ShapeTable)
		}
	}
	for i, t := range r.Terms {
		if err := b.validateTerm(r, i, t); err != nil {
			return err
		}
	}
	for _, id := range r.UnpricedComponents {
		if id == "" {
			return fmt.Errorf("pricing: rule %q: empty unpriced_components entry", r.Type)
		}
	}
	return nil
}

func (b *Book) validateTerm(r *Rule, i int, t Term) error {
	where := fmt.Sprintf("rule %q term %d", r.Type, i)
	switch {
	case t.Rate.ID == "" && t.Rate.FromShape == "":
		return fmt.Errorf("pricing: %s: rate needs an id or from_shape", where)
	case t.Rate.ID != "" && t.Rate.FromShape != "":
		return fmt.Errorf("pricing: %s: rate cannot set both id and from_shape", where)
	case t.Quantity.Empty():
		return fmt.Errorf("pricing: %s: quantity names no source", where)
	}
	if t.Rate.ID != "" {
		if _, ok := b.rateByID[t.Rate.ID]; !ok {
			return fmt.Errorf("pricing: %s: %q is not a known rate", where, t.Rate.ID)
		}
		// Optional only means "the shape may not define this rate". A literal
		// id either exists or the book is wrong; tolerating it would let a
		// typo'd rate id silently under-price every matching asset.
		if t.Optional {
			return fmt.Errorf("pricing: %s: optional applies only to from_shape rates", where)
		}
		return nil
	}
	if r.ShapeFrom == nil {
		return fmt.Errorf("pricing: %s: from_shape needs a shape_from selector on the rule", where)
	}
	if t.Rate.FromShape != FieldOCPURate && t.Rate.FromShape != FieldMemoryRate {
		return fmt.Errorf("pricing: %s: from_shape %q must be %q or %q",
			where, t.Rate.FromShape, FieldOCPURate, FieldMemoryRate)
	}
	if !t.Optional {
		// Every shape in the table must satisfy a required term, or some asset
		// with that shape resolves to nothing at estimate time — a runtime
		// failure for a defect the book could have caught here.
		for name, s := range b.Shapes[r.ShapeTable] {
			if s.Note != "" && s.OCPURate == "" {
				continue // a documented gap; resolves to unknown by design
			}
			if _, ok := s.RateID(t.Rate.FromShape); !ok {
				return fmt.Errorf("pricing: %s: shape %s/%s defines no %s (mark the term optional if that is intended)",
					where, r.ShapeTable, name, t.Rate.FromShape)
			}
		}
	}
	if q := t.Quantity.Shape; q != "" && q != FieldDefaultOCPU && q != FieldDefaultMemoryGB {
		return fmt.Errorf("pricing: %s: quantity shape %q must be %q or %q",
			where, q, FieldDefaultOCPU, FieldDefaultMemoryGB)
	}
	return nil
}

func indexOf[T any](s []T, match func(T) bool) int {
	for i := range s {
		if match(s[i]) {
			return i
		}
	}
	return -1
}
