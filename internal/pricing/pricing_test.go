package pricing

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// minimalBook is a valid one-book document tests extend. Kept small so a test
// that adds one rule reads as being about that rule.
const minimalBook = `
version: 1
books:
  - {id: test, source: "https://example.invalid", vintage: "2026-08-13"}
rates:
  - {id: t.hourly, book: test, unit: hour,  amount: 0.01}
  - {id: t.monthly, book: test, unit: month, amount: 5.00}
rules:
  - type: t.thing
    terms:
      - {rate: t.monthly, quantity: {literal: 1}}
`

func doc(t *testing.T, body string) Document {
	t.Helper()
	return Document{Name: "test.yaml", Data: []byte(body)}
}

func mustLoad(t *testing.T, bodies ...string) *Book {
	t.Helper()
	docs := make([]Document, len(bodies))
	for i, b := range bodies {
		docs[i] = doc(t, b)
	}
	b, err := Load(docs...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// The tier trap
// ---------------------------------------------------------------------------

// A feed fixture in the exact shape Oracle publishes: an Always Free first
// tier at value 0, then the real rate. Reading prices[0] here is the bug the
// whole feature exists to prevent, so the expected values are written out
// literally rather than derived — a derived expectation would reproduce
// whatever mistake the implementation made.
const tieredFeed = `{
  "lastUpdated": "2026-08-06T15:45:07.573Z",
  "items": [
    {"partNumber":"B93030","displayName":"Load Balancer Base","metricName":"Load Balancer",
     "currencyCodeLocalizations":[{"currencyCode":"USD","prices":[
       {"model":"PAY_AS_YOU_GO","value":0,"rangeMin":0,"rangeMax":744},
       {"model":"PAY_AS_YOU_GO","value":0.0113,"rangeMin":744,"rangeMax":999999999}]}]},
    {"partNumber":"B93297","displayName":"Compute - Standard - A1 - OCPU","metricName":"OCPU Per Hour",
     "currencyCodeLocalizations":[{"currencyCode":"USD","prices":[
       {"model":"PAY_AS_YOU_GO","value":0,"rangeMin":0,"rangeMax":3000},
       {"model":"PAY_AS_YOU_GO","value":0.01,"rangeMin":3000,"rangeMax":999999999999999}]}]},
    {"partNumber":"B93113","displayName":"Compute - Standard - E4 - OCPU","metricName":"OCPU Per Hour",
     "currencyCodeLocalizations":[{"currencyCode":"USD","prices":[
       {"model":"PAY_AS_YOU_GO","value":0.025}]}]},
    {"partNumber":"B99999","displayName":"Shuffled Tiers","metricName":"Widget Per Hour",
     "currencyCodeLocalizations":[{"currencyCode":"USD","prices":[
       {"model":"PAY_AS_YOU_GO","value":0.05,"rangeMin":100,"rangeMax":999999999},
       {"model":"PAY_AS_YOU_GO","value":0,"rangeMin":0,"rangeMax":100}]}]}
  ]
}`

func TestMarginalPrice_PicksHighestRangeMinNotFirstTier(t *testing.T) {
	feed, err := ParseOCIFeed([]byte(tieredFeed))
	if err != nil {
		t.Fatalf("ParseOCIFeed: %v", err)
	}
	cases := []struct {
		sku       string
		want      float64
		wantTiers int
		why       string
	}{
		{"B93030", 0.0113, 2, "Always Free first tier must not win — a load balancer is not free forever"},
		{"B93297", 0.01, 2, "the 3000 free A1 OCPU-hours are tenancy-wide and unknowable per-asset"},
		{"B93113", 0.025, 1, "an untiered SKU is its own marginal tier"},
		{"B99999", 0.05, 2, "tier order in the document must not matter; selection is by rangeMin"},
	}
	for _, tc := range cases {
		p, ok := feed.Product(tc.sku)
		if !ok {
			t.Fatalf("%s: not in fixture", tc.sku)
		}
		price, tiers, ok := p.MarginalPrice("USD")
		if !ok {
			t.Fatalf("%s: no USD price", tc.sku)
		}
		if price.Value != tc.want {
			t.Errorf("%s: marginal = %v, want %v (%s)", tc.sku, price.Value, tc.want, tc.why)
		}
		if tiers != tc.wantTiers {
			t.Errorf("%s: tiers = %d, want %d", tc.sku, tiers, tc.wantTiers)
		}
	}
}

// The sentinels in Oracle's feed are inconsistent (999999999 and
// 999999999999999 both appear alongside real bounds like 744), so any code that
// recognises "infinity" by value is a bug waiting on a third sentinel. This
// asserts the implementation never looks at rangeMax at all.
func TestMarginalPrice_IgnoresRangeMaxSentinels(t *testing.T) {
	const feedJSON = `{"lastUpdated":"2026-01-01T00:00:00Z","items":[
	  {"partNumber":"X","displayName":"X","metricName":"Unit",
	   "currencyCodeLocalizations":[{"currencyCode":"USD","prices":[
	     {"model":"PAY_AS_YOU_GO","value":0,"rangeMin":0,"rangeMax":10},
	     {"model":"PAY_AS_YOU_GO","value":0.7,"rangeMin":10,"rangeMax":42}]}]}]}`
	feed, err := ParseOCIFeed([]byte(feedJSON))
	if err != nil {
		t.Fatalf("ParseOCIFeed: %v", err)
	}
	p, _ := feed.Product("X")
	price, _, ok := p.MarginalPrice("USD")
	if !ok || price.Value != 0.7 {
		// A rangeMax of 42 is not any known sentinel; if the top tier were
		// selected by "rangeMax looks infinite" this would return 0.
		t.Fatalf("marginal = %v (ok=%v), want 0.7 with no sentinel matching", price.Value, ok)
	}
}

// TestPrices_UseMarginalTier is the regression guard on the committed book: it
// re-derives every OCI amount from the recorded feed the generator used. If
// someone hand-edits an amount, or the generator regresses to prices[0], this
// fails with the SKU named.
func TestPrices_UseMarginalTier(t *testing.T) {
	feed := recordedFeed(t)
	book, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	checked := 0
	for _, r := range book.Rates {
		if r.Book != OCIBookID || r.SKU == "" {
			continue
		}
		p, ok := feed.Product(r.SKU)
		if !ok {
			t.Errorf("rate %s: SKU %s absent from the recorded feed", r.ID, r.SKU)
			continue
		}
		price, tiers, ok := p.MarginalPrice(book.CurrencyOf(&r))
		if !ok {
			t.Errorf("rate %s: SKU %s has no price in the recorded feed", r.ID, r.SKU)
			continue
		}
		if r.Amount != price.Value {
			t.Errorf("rate %s (%s): amount %v, feed marginal tier %v", r.ID, r.SKU, r.Amount, price.Value)
		}
		if (tiers > 1) != (r.TierNote != "") {
			t.Errorf("rate %s (%s): %d tiers but tier_note=%q — a tiered rate must say so",
				r.ID, r.SKU, tiers, r.TierNote)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no OCI rates checked — the fixture or the book is not wired up")
	}
	t.Logf("verified %d OCI rates against feed vintage %s", checked, feed.LastUpdated)
}

// The Always Free tier is worth its own named test, because "the first one is
// free" is exactly the shape of reasoning that produces a $0.00 estimate.
func TestPrices_AlwaysFreeTierIsNotThePrice(t *testing.T) {
	book, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	for _, id := range []string{"oci.lb.base", "oci.compute.a1.ocpu", "oci.object_storage.storage"} {
		r, ok := book.Rate(id)
		if !ok {
			t.Fatalf("rate %s missing from the embedded book", id)
		}
		if r.Amount <= 0 {
			t.Errorf("rate %s: amount %v — the Always Free first tier leaked into the book", id, r.Amount)
		}
		if r.TierNote == "" {
			t.Errorf("rate %s: tiered SKU with no tier_note; the over-estimate must be disclosed", id)
		}
	}
}

func recordedFeed(t *testing.T) *OCIFeed {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join("testdata", "oci-feed.json.gz"))
	if err != nil {
		t.Fatalf("read recorded feed (run `just prices`): %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("gunzip recorded feed: %v", err)
	}
	defer func() { _ = zr.Close() }()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read recorded feed: %v", err)
	}
	feed, err := ParseOCIFeed(raw)
	if err != nil {
		t.Fatalf("parse recorded feed: %v", err)
	}
	return feed
}

// ---------------------------------------------------------------------------
// Zero is never a silent answer
// ---------------------------------------------------------------------------

// A zero rate is indistinguishable from a rate we failed to read, and it
// renders as a confident $0.00. Load must refuse one outright, including from a
// user's --price-book.
func TestLoad_RejectsZeroAndNonsenseRates(t *testing.T) {
	cases := map[string]string{
		"zero":     "0",
		"negative": "-1.5",
		"nan":      ".nan",
		"infinity": ".inf",
	}
	for name, amount := range cases {
		t.Run(name, func(t *testing.T) {
			body := `
version: 1
books: [{id: test, source: "x", vintage: "2026-08-13"}]
rates: [{id: t.free, book: test, unit: month, amount: ` + amount + `}]
`
			_, err := Load(doc(t, body))
			if err == nil {
				t.Fatal("Load accepted a rate that cannot be a price")
			}
			if !strings.Contains(err.Error(), "amount must be > 0") {
				t.Errorf("error should name the invariant, got: %v", err)
			}
		})
	}
}

// Every rule in the shipped book must resolve to a stated basis or a computable
// estimate — never to an implicit zero. A rule declaring a basis has to say
// why, and a rule that can produce 0.00 has to carry the explanation with it.
func TestDefault_NoRuleCanProduceASilentZero(t *testing.T) {
	book, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	for i := range book.Rules {
		r := &book.Rules[i]
		switch {
		case r.Declared() && r.Note == "":
			t.Errorf("rule %s: declares basis %q with no note", r.Type, r.Basis)
		case len(r.ZeroWhenStatus) > 0 && r.ZeroNote == "":
			t.Errorf("rule %s: can zero on status with no zero_note", r.Type)
		case r.Condition != nil && r.ElseNote == "":
			t.Errorf("rule %s: conditional with no else_note", r.Type)
		}
		for _, term := range r.Terms {
			if term.Rate.ID == "" {
				continue
			}
			rate, ok := book.Rate(term.Rate.ID)
			if !ok {
				t.Errorf("rule %s: term rate %q does not exist", r.Type, term.Rate.ID)
				continue
			}
			if rate.Amount <= 0 {
				t.Errorf("rule %s: term rate %q is %v", r.Type, term.Rate.ID, rate.Amount)
			}
		}
	}
}

// An asset type with no rule must yield no rule — not a fallback, not a
// neighbouring rule, and above all not a zero. This is the guard against
// someone adding prefix matching later.
func TestBook_UnknownTypeYieldsNoRuleRatherThanAZeroRate(t *testing.T) {
	book, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	for _, typ := range []string{
		"oci.brand_new_service",
		"oci.compute",           // a prefix of oci.compute.instance
		"oci.compute.instance.", // a suffix extension of a real rule
		"cloudflare.",
		"",
	} {
		if r, ok := book.Rule(typ); ok {
			t.Errorf("Rule(%q) matched %q — matching must be exact", typ, r.Type)
		}
	}
	if _, ok := book.Rate("no.such.rate"); ok {
		t.Error("Rate matched an id that does not exist")
	}
	if _, ok := book.Shape("oci", "VM.Imaginary.Flex"); ok {
		t.Error("Shape matched a shape that does not exist — there must be no default shape")
	}
}

// ---------------------------------------------------------------------------
// Merge semantics
// ---------------------------------------------------------------------------

func TestLoad_MergeReplacesByIdentityAndKeepsTheRest(t *testing.T) {
	override := `
version: 1
books:
  - {id: test, source: "https://example.invalid/v2", vintage: "2026-09-01"}
rates:
  - {id: t.monthly, book: test, unit: month, amount: 9.99}
  - {id: t.extra,   book: test, unit: hour,  amount: 0.5}
rules:
  - type: t.thing
    basis: unpriceable
    note: "overridden"
  - type: t.other
    terms:
      - {rate: t.extra, quantity: {literal: 2}}
`
	book := mustLoad(t, minimalBook, override)

	// Replaced by id.
	if r, _ := book.Rate("t.monthly"); r.Amount != 9.99 {
		t.Errorf("t.monthly = %v, want 9.99", r.Amount)
	}
	// Untouched by the override, therefore kept.
	if r, ok := book.Rate("t.hourly"); !ok || r.Amount != 0.01 {
		t.Errorf("t.hourly = %+v, want the built-in 0.01 to survive", r)
	}
	// Added.
	if _, ok := book.Rate("t.extra"); !ok {
		t.Error("t.extra was not added")
	}
	// Rules replace by type, wholesale — not field by field. The original
	// t.thing had terms and no basis; the override has a basis and no terms.
	rule, ok := book.Rule("t.thing")
	if !ok {
		t.Fatal("t.thing missing")
	}
	if rule.Basis != BasisUnpriceable || len(rule.Terms) != 0 {
		t.Errorf("t.thing = %+v, want a wholesale replacement, not a deep merge", rule)
	}
	if _, ok := book.Rule("t.other"); !ok {
		t.Error("t.other was not added")
	}
	// Sources replace by id too.
	if s, _ := book.Source("test"); s.Vintage != "2026-09-01" {
		t.Errorf("source vintage = %q, want the override's", s.Vintage)
	}
	// Position is stable: a replaced entry keeps its slot, so output order
	// does not depend on how many overrides were passed.
	if book.Rates[1].ID != "t.monthly" {
		t.Errorf("rates order = %v, want t.monthly to keep slot 1", ratesIDs(book))
	}
}

func TestLoad_MergeReplacesShapesByNameWithinTable(t *testing.T) {
	base := `
version: 1
books: [{id: test, source: "x", vintage: "2026-08-13"}]
rates:
  - {id: r.cpu, book: test, unit: hour, amount: 0.02}
  - {id: r.mem, book: test, unit: hour, amount: 0.002}
shapes:
  demo:
    A: {ocpu_rate: r.cpu, memory_rate: r.mem, flexible: true, default_ocpu: 1, default_memory_gb: 8}
    B: {ocpu_rate: r.cpu, default_ocpu: 4}
`
	override := `
version: 1
books: [{id: test, source: "x", vintage: "2026-08-13"}]
shapes:
  demo:
    A: {ocpu_rate: r.cpu, memory_rate: r.mem, flexible: true, default_ocpu: 16, default_memory_gb: 256}
    C: {ocpu_rate: r.cpu, default_ocpu: 2}
`
	book := mustLoad(t, base, override)
	if s, _ := book.Shape("demo", "A"); s.DefaultOCPU != 16 {
		t.Errorf("shape A default_ocpu = %v, want the override's 16", s.DefaultOCPU)
	}
	if s, ok := book.Shape("demo", "B"); !ok || s.DefaultOCPU != 4 {
		t.Errorf("shape B = %+v, want the built-in to survive", s)
	}
	if _, ok := book.Shape("demo", "C"); !ok {
		t.Error("shape C was not added")
	}
}

func TestLoadFile_LayersOverTheEmbeddedBook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "override.yaml")
	body := `
version: 1
books:
  - {id: oci, source: "local", vintage: "2026-12-25"}
rates:
  - {id: oci.lb.base, book: oci, sku: B93030, unit: hour, amount: 0.9}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	book, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if r, _ := book.Rate("oci.lb.base"); r.Amount != 0.9 {
		t.Errorf("overridden rate = %v, want 0.9", r.Amount)
	}
	if _, ok := book.Rate("oci.block.storage"); !ok {
		t.Error("an unrelated built-in rate did not survive the override")
	}
	if _, ok := book.Rule("cloudflare.r2_bucket"); !ok {
		t.Error("rules from other embedded books did not survive the override")
	}

	// The shared Default() book must not have been mutated by the override.
	def, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if r, _ := def.Rate("oci.lb.base"); r.Amount == 0.9 {
		t.Error("LoadFile mutated the shared embedded book")
	}
}

func TestLoadFile_MissingFileIsAnError(t *testing.T) {
	if _, err := LoadFile(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("LoadFile accepted a missing path")
	}
}

func ratesIDs(b *Book) []string {
	ids := make([]string, len(b.Rates))
	for i, r := range b.Rates {
		ids[i] = r.ID
	}
	return ids
}

// ---------------------------------------------------------------------------
// Schema, round-tripping and validation
// ---------------------------------------------------------------------------

// The generator rewrites books/oci.yaml by marshalling a parsed Book, so a
// field that does not survive the round trip is a field silently deleted from
// the committed book on the next `just prices`.
func TestBook_YAMLRoundTrips(t *testing.T) {
	original, err := os.ReadFile(filepath.Join("books", "oci.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := Load(Document{Name: "books/oci.yaml", Data: original})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := yaml.Marshal(first)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	second, err := Load(Document{Name: "remarshalled", Data: out})
	if err != nil {
		t.Fatalf("reload after marshal: %v", err)
	}
	again, err := yaml.Marshal(second)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Equal(out, again) {
		t.Error("marshal/parse is not a fixed point — `just prices` would churn the file every run")
	}
	if len(first.Rates) != len(second.Rates) || len(first.Rules) != len(second.Rules) {
		t.Fatalf("round trip lost content: rates %d->%d, rules %d->%d",
			len(first.Rates), len(second.Rates), len(first.Rules), len(second.Rules))
	}
	for i := range first.Rules {
		a, b := first.Rules[i], second.Rules[i]
		if a.Type != b.Type || len(a.Terms) != len(b.Terms) || a.Note != b.Note || a.ZeroNote != b.ZeroNote {
			t.Errorf("rule %s did not survive the round trip:\n %+v\n %+v", a.Type, a, b)
		}
		for j := range a.Terms {
			if a.Terms[j].Rate != b.Terms[j].Rate || a.Terms[j].Optional != b.Terms[j].Optional {
				t.Errorf("rule %s term %d rate ref changed: %+v -> %+v", a.Type, j, a.Terms[j], b.Terms[j])
			}
		}
	}
}

// The scalar form is the one a human writes; losing it on regeneration would
// rewrite every hand-written rule into mapping syntax.
func TestRateRef_ScalarAndMappingFormsBothWork(t *testing.T) {
	var scalar RateRef
	if err := yaml.Unmarshal([]byte("oci.block.storage"), &scalar); err != nil {
		t.Fatal(err)
	}
	if scalar.ID != "oci.block.storage" || scalar.FromShape != "" {
		t.Errorf("scalar form = %+v", scalar)
	}
	out, err := yaml.Marshal(scalar)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "oci.block.storage" {
		t.Errorf("scalar re-marshalled as %q, want a bare scalar", strings.TrimSpace(string(out)))
	}

	var mapping RateRef
	if err := yaml.Unmarshal([]byte("{from_shape: ocpu_rate}"), &mapping); err != nil {
		t.Fatal(err)
	}
	if mapping.FromShape != FieldOCPURate || mapping.ID != "" {
		t.Errorf("mapping form = %+v", mapping)
	}
}

func TestLoad_RejectsMalformedBooks(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{
			"wrong schema version",
			"version: 2\nbooks: [{id: t, source: x, vintage: \"2026-08-13\"}]\n",
			"schema version",
		},
		{
			"unknown key is a typo, not an extension",
			"version: 1\nbooks: [{id: t, source: x, vintage: \"2026-08-13\"}]\nrattes: []\n",
			"field rattes not found",
		},
		{
			"rate with no book source",
			"version: 1\nrates: [{id: a, book: ghost, unit: hour, amount: 1}]\n",
			"has no source entry",
		},
		{
			"rate with an unrecognised unit",
			"version: 1\nbooks: [{id: t, source: x, vintage: \"2026-08-13\"}]\n" +
				"rates: [{id: a, book: t, unit: week, amount: 1}]\n",
			"must be \"hour\" or \"month\"",
		},
		{
			"duplicate rate id inside one document",
			"version: 1\nbooks: [{id: t, source: x, vintage: \"2026-08-13\"}]\n" +
				"rates: [{id: a, book: t, unit: hour, amount: 1}, {id: a, book: t, unit: hour, amount: 2}]\n",
			"declared twice",
		},
		{
			"rule term pointing at a rate that does not exist",
			minimalBook + "\n  - type: t.ghost\n    terms:\n      - {rate: t.nope, quantity: {literal: 1}}\n",
			"is not a known rate",
		},
		{
			"rule with neither terms nor a basis",
			minimalBook + "\n  - type: t.empty\n",
			"needs either terms or a declared basis",
		},
		{
			"declared basis with no explanation",
			minimalBook + "\n  - {type: t.mystery, basis: unpriceable}\n",
			"requires a note",
		},
		{
			"a basis that is not one of the five",
			minimalBook + "\n  - {type: t.odd, basis: cheap, note: \"why\"}\n",
			"must be \"unpriceable\" or \"unknown\"",
		},
		{
			"quantity naming no source at all",
			minimalBook + "\n  - type: t.void\n    terms:\n      - {rate: t.monthly, quantity: {}}\n",
			"quantity names no source",
		},
		{
			"zeroing status with no explanation",
			minimalBook + "\n    zero_when_status: [STOPPED]\n",
			"requires a zero_note",
		},
		{
			"condition with no else_basis",
			minimalBook + "\n    condition: {tag: k, equals: v}\n",
			"requires else_basis",
		},
		{
			"optional on a literal rate id",
			minimalBook + "\n  - type: t.opt\n    terms:\n      - {rate: t.monthly, quantity: {literal: 1}, optional: true}\n",
			"optional applies only to from_shape rates",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(doc(t, tc.body))
			if err == nil {
				t.Fatal("Load accepted a malformed book")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v\nwant it to contain %q", err, tc.want)
			}
		})
	}
}

func TestValidate_ShapeInvariants(t *testing.T) {
	const prelude = `
version: 1
books: [{id: t, source: x, vintage: "2026-08-13"}]
rates:
  - {id: r.cpu, book: t, unit: hour, amount: 0.02}
  - {id: r.mem, book: t, unit: hour, amount: 0.002}
shapes:
  demo:
`
	cases := []struct{ name, shapes, want string }{
		{
			"flexible shape missing its memory rate",
			"    F: {ocpu_rate: r.cpu, flexible: true, default_ocpu: 1}\n",
			"flexible shapes must set both",
		},
		{
			"shape with no rate and no explanation",
			"    G: {default_ocpu: 1}\n",
			"must carry a note explaining the gap",
		},
		{
			"shape pointing at a rate that does not exist",
			"    H: {ocpu_rate: r.ghost, default_ocpu: 1}\n",
			"is not a known rate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(doc(t, prelude+tc.shapes))
			if err == nil {
				t.Fatal("Load accepted an invalid shape table")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v\nwant it to contain %q", err, tc.want)
			}
		})
	}

	// A documented gap is legal: it resolves to unknown, but carrying its own
	// explanation instead of "unknown shape — add it to the price book".
	if _, err := Load(doc(t, prelude+"    Free: {note: \"Always Free only; no marginal rate exists\"}\n")); err != nil {
		t.Errorf("a note-only shape should be legal: %v", err)
	}
}

// A required from_shape term must be satisfiable by every shape in the table,
// or some asset resolves to nothing at estimate time — a runtime failure for a
// defect the book could have caught at load.
func TestValidate_RequiredShapeTermMustCoverEveryShape(t *testing.T) {
	body := `
version: 1
books: [{id: t, source: x, vintage: "2026-08-13"}]
rates:
  - {id: r.cpu, book: t, unit: hour, amount: 0.02}
  - {id: r.mem, book: t, unit: hour, amount: 0.002}
shapes:
  demo:
    Flex:  {ocpu_rate: r.cpu, memory_rate: r.mem, flexible: true, default_ocpu: 1, default_memory_gb: 8}
    Fixed: {ocpu_rate: r.cpu, default_ocpu: 4}
rules:
  - type: t.vm
    shape_from: {tag: shape}
    shape_table: demo
    terms:
      - {rate: {from_shape: ocpu_rate},   quantity: {tag: ocpus,  shape: default_ocpu}}
      - {rate: {from_shape: memory_rate}, quantity: {tag: mem_gb, shape: default_memory_gb}}
`
	_, err := Load(doc(t, body))
	if err == nil {
		t.Fatal("Load accepted a required memory term the Fixed shape cannot satisfy")
	}
	if !strings.Contains(err.Error(), "mark the term optional") {
		t.Errorf("error should point at the fix, got: %v", err)
	}

	// Marking it optional is the documented escape hatch for OCI's pre-Flex
	// shapes, where memory is bundled into the OCPU SKU.
	if _, err := Load(doc(t, strings.Replace(body,
		"quantity: {tag: mem_gb, shape: default_memory_gb}}",
		"quantity: {tag: mem_gb, shape: default_memory_gb}, optional: true}", 1))); err != nil {
		t.Errorf("optional should make the term legal: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Lookups and unit conversion
// ---------------------------------------------------------------------------

func TestBook_MonthlyAmountAndCurrency(t *testing.T) {
	book := mustLoad(t, minimalBook)
	hourly, _ := book.Rate("t.hourly")
	if got, want := book.MonthlyAmount(hourly), 0.01*DefaultHoursPerMonth; got != want {
		t.Errorf("MonthlyAmount(hourly) = %v, want %v", got, want)
	}
	monthly, _ := book.Rate("t.monthly")
	if got := book.MonthlyAmount(monthly); got != 5.0 {
		t.Errorf("MonthlyAmount(monthly) = %v, want 5", got)
	}
	if got := book.CurrencyOf(monthly); got != DefaultCurrency {
		t.Errorf("CurrencyOf = %q, want the book default %q", got, DefaultCurrency)
	}

	// EUR rates must keep their own currency — nothing in this tool converts,
	// so a rate that forgets its currency silently becomes dollars.
	def, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	nb, ok := def.Rate("nb.seat.team")
	if !ok {
		t.Fatal("nb.seat.team missing")
	}
	if got := def.CurrencyOf(nb); got != "EUR" {
		t.Errorf("CurrencyOf(nb.seat.team) = %q, want EUR", got)
	}
}

func TestBook_HoursPerMonthOverride(t *testing.T) {
	book := mustLoad(t, minimalBook, "version: 1\nhours_per_month: 744\n")
	if book.HoursPerMonth != 744 {
		t.Fatalf("HoursPerMonth = %v, want 744", book.HoursPerMonth)
	}
	hourly, _ := book.Rate("t.hourly")
	if got := book.MonthlyAmount(hourly); math.Abs(got-7.44) > 1e-9 {
		t.Errorf("MonthlyAmount = %v, want 7.44", got)
	}
}

func TestRule_ZerosAtIsCaseInsensitive(t *testing.T) {
	book, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	rule, ok := book.Rule("oci.compute.instance")
	if !ok {
		t.Fatal("oci.compute.instance rule missing")
	}
	for _, status := range []string{"STOPPED", "stopped", "Stopped", "TERMINATED"} {
		if !rule.ZerosAt(status) {
			t.Errorf("ZerosAt(%q) = false", status)
		}
	}
	for _, status := range []string{"RUNNING", "", "STOPPING"} {
		if rule.ZerosAt(status) {
			t.Errorf("ZerosAt(%q) = true — a running instance is billed", status)
		}
	}
}

func TestCondition_AbsentTagNeverMatches(t *testing.T) {
	// Not being able to observe the discriminator is not evidence that it
	// holds. An OKE cluster with no cluster_type tag must not be priced as an
	// enhanced one.
	c := Condition{Tag: "cluster_type", Equals: "ENHANCED_CLUSTER"}
	if c.Matches("", false) {
		t.Error("absent tag matched an equals condition")
	}
	if !c.Matches("ENHANCED_CLUSTER", true) {
		t.Error("present matching tag did not match")
	}
	if c.Matches("BASIC_CLUSTER", true) {
		t.Error("present non-matching tag matched")
	}

	ne := Condition{Tag: "acl_tags", NonEmpty: true}
	if ne.Matches("", true) {
		t.Error("empty value satisfied non_empty")
	}
	if ne.Matches("", false) {
		t.Error("absent tag satisfied non_empty")
	}
	if !ne.Matches("tag:prod", true) {
		t.Error("non-empty value did not satisfy non_empty")
	}
}

func TestShape_FieldResolutionIsExactAndTyped(t *testing.T) {
	s := Shape{OCPURate: "r.cpu", MemoryRate: "r.mem", DefaultOCPU: 2, DefaultMemoryGB: 32}
	if id, ok := s.RateID(FieldOCPURate); !ok || id != "r.cpu" {
		t.Errorf("RateID(ocpu_rate) = %q, %v", id, ok)
	}
	if _, ok := s.RateID("ocpuRate"); ok {
		t.Error("a misspelled field resolved — the switch must fail closed")
	}
	if q, ok := s.DefaultQuantity(FieldDefaultMemoryGB); !ok || q != 32 {
		t.Errorf("DefaultQuantity(default_memory_gb) = %v, %v", q, ok)
	}
	// A zero default is not a default: it would price the term at 0.
	empty := Shape{OCPURate: "r.cpu"}
	if _, ok := empty.DefaultQuantity(FieldDefaultOCPU); ok {
		t.Error("a zero default_ocpu resolved as a usable quantity")
	}
}

func TestBook_StaleFlagsOldAndUnparseableVintages(t *testing.T) {
	book := mustLoad(t, minimalBook)
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if stale := book.Stale(now, 90*24*time.Hour); len(stale) != 1 {
		t.Errorf("Stale = %v, want the 2026-08-13 book flagged", stale)
	}
	if stale := book.Stale(time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), 90*24*time.Hour); len(stale) != 0 {
		t.Errorf("Stale = %v, want nothing flagged a week later", stale)
	}
	// An unreadable vintage is not evidence of freshness.
	bad := mustLoad(t, "version: 1\nbooks: [{id: t, source: x, vintage: \"whenever\"}]\n")
	if stale := bad.Stale(now, 90*24*time.Hour); len(stale) != 1 {
		t.Error("an unparseable vintage was treated as fresh")
	}
}

// ---------------------------------------------------------------------------
// The embedded books themselves
// ---------------------------------------------------------------------------

func TestDefault_LoadsAndCoversTheProvidersWeCollect(t *testing.T) {
	book, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	for _, id := range []string{"oci", "cloudflare", "mesh", "kubernetes"} {
		s, ok := book.Source(id)
		if !ok {
			t.Errorf("book %q has no source entry", id)
			continue
		}
		if s.Source == "" || s.Vintage == "" {
			t.Errorf("book %q: source=%q vintage=%q — both are required provenance", id, s.Source, s.Vintage)
		}
		if _, ok := s.VintageTime(); !ok {
			t.Errorf("book %q: vintage %q does not parse as RFC3339 or a date", id, s.Vintage)
		}
	}
	// Types the estimator is expected to actually price.
	for _, typ := range []string{
		"oci.compute.instance", "oci.block_volume", "oci.boot_volume",
		"oci.load_balancer", "oci.oke.cluster", "tailscale.device",
	} {
		rule, ok := book.Rule(typ)
		if !ok {
			t.Errorf("no rule for %s", typ)
			continue
		}
		if len(rule.Terms) == 0 {
			t.Errorf("rule %s has no terms — it cannot produce a number", typ)
		}
	}
	// Types whose billing model is known to be consumption-based.
	for _, typ := range []string{
		"cloudflare.r2_bucket", "cloudflare.worker_script",
		"cloudflare.kv_namespace", "cloudflare.d1_database",
		"oci.object_storage.bucket",
	} {
		rule, ok := book.Rule(typ)
		if !ok {
			t.Errorf("no rule for %s", typ)
			continue
		}
		if rule.Basis != BasisUnpriceable {
			t.Errorf("rule %s basis = %q, want unpriceable — metered is not unknown", typ, rule.Basis)
		}
	}
}

// The whole point of the shape table is that a real OCI shape name resolves.
func TestDefault_CommonOCIShapesResolve(t *testing.T) {
	book, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	for _, name := range []string{
		"VM.Standard.E4.Flex", "VM.Standard.E5.Flex", "VM.Standard.A1.Flex",
		"VM.Standard2.4", "VM.Standard.E2.1.Micro",
	} {
		if _, ok := book.Shape("oci", name); !ok {
			t.Errorf("shape %s does not resolve", name)
		}
	}
	// The canonical conformance figure: 200 GB Balanced (10 VPU/GB) is
	// Oracle's published $0.0425/GB-month, so $8.50/month.
	storage, ok := book.Rate("oci.block.storage")
	if !ok {
		t.Fatal("oci.block.storage missing")
	}
	vpu, ok := book.Rate("oci.block.vpu")
	if !ok {
		t.Fatal("oci.block.vpu missing")
	}
	got := 200 * (book.MonthlyAmount(storage) + 10*book.MonthlyAmount(vpu))
	if math.Abs(got-8.50) > 1e-9 {
		t.Errorf("200 GB Balanced = %v, want 8.50", got)
	}
}

// ---------------------------------------------------------------------------
// Refresh — every failure mode has to degrade, because offline is normal
// ---------------------------------------------------------------------------

type fakeCache struct {
	entry     CachedFeed
	have      bool
	loadErr   error
	saveErr   error
	saved     *CachedFeed
	loadCalls int
}

func (c *fakeCache) LoadPriceFeed(context.Context, string) (CachedFeed, bool, error) {
	c.loadCalls++
	return c.entry, c.have, c.loadErr
}

func (c *fakeCache) SavePriceFeed(_ context.Context, f CachedFeed) error {
	if c.saveErr != nil {
		return c.saveErr
	}
	c.saved = &f
	return nil
}

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	out, err := gzipBytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A book whose single OCI rate is covered by tieredFeed, so Reprice can run
// without dragging in the whole embedded catalogue.
func refreshBase(t *testing.T) *Book {
	t.Helper()
	return mustLoad(t, `
version: 1
books:
  - {id: oci, source: "https://example.invalid", vintage: "2020-01-01"}
rates:
  - {id: oci.lb.base, book: oci, sku: B93030, unit: hour, amount: 0.0113}
rules:
  - type: oci.load_balancer
    terms: [{rate: oci.lb.base, quantity: {literal: 1}}]
`)
}

func TestRefresh_FetchesAndRepricesAtTheMarginalTier(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"abc"`)
		_, _ = io.WriteString(w, tieredFeed)
	}))
	defer srv.Close()

	cache := &fakeCache{}
	book, err := Refresh(context.Background(), refreshBase(t), cache, redirectClient(srv.URL))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	r, _ := book.Rate("oci.lb.base")
	if r.Amount != 0.0113 {
		t.Errorf("refreshed amount = %v, want the marginal 0.0113 (not the free tier's 0)", r.Amount)
	}
	if r.TierNote == "" {
		t.Error("a tiered SKU was refreshed without a tier_note")
	}
	if s, _ := book.Source(OCIBookID); s.Vintage != "2026-08-06T15:45:07.573Z" {
		t.Errorf("vintage = %q, want the feed's own lastUpdated", s.Vintage)
	}
	if cache.saved == nil {
		t.Fatal("a fresh fetch was not cached")
	}
	if cache.saved.ETag != `"abc"` {
		t.Errorf("cached etag = %q", cache.saved.ETag)
	}
	if raw, err := gunzipBytes(cache.saved.Payload); err != nil || !bytes.Contains(raw, []byte("B93030")) {
		t.Errorf("cached payload is not the gzipped feed: %v", err)
	}
}

func TestRefresh_NoNetworkFallsBackToCache(t *testing.T) {
	cache := &fakeCache{
		have:  true,
		entry: CachedFeed{BookID: OCIBookID, ETag: `"abc"`, Payload: gzipped(t, tieredFeed)},
	}
	// A client that always fails, standing in for DNS failure, a proxy denial,
	// or an air-gapped runner — the environments this binary actually runs in.
	book, err := Refresh(context.Background(), refreshBase(t), cache, failingClient())
	if err != nil {
		t.Fatalf("Refresh returned an error instead of degrading: %v", err)
	}
	if r, _ := book.Rate("oci.lb.base"); r.Amount != 0.0113 {
		t.Errorf("amount = %v, want the cached feed's marginal rate", r.Amount)
	}
	if cache.saved != nil {
		t.Error("a cache-sourced feed was written back to the cache")
	}
}

func TestRefresh_NoNetworkNoCacheKeepsTheEmbeddedBook(t *testing.T) {
	base := refreshBase(t)
	book, err := Refresh(context.Background(), base, &fakeCache{}, failingClient())
	if err != nil {
		t.Fatalf("Refresh returned an error instead of degrading: %v", err)
	}
	if r, _ := book.Rate("oci.lb.base"); r.Amount != 0.0113 {
		t.Errorf("amount = %v, want the embedded value untouched", r.Amount)
	}
	if s, _ := book.Source(OCIBookID); s.Vintage != "2020-01-01" {
		t.Errorf("vintage = %q — an unrefreshed book must not claim a fresh vintage", s.Vintage)
	}
}

func TestRefresh_WithNoCacheAtAllStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, tieredFeed)
	}))
	defer srv.Close()
	book, err := Refresh(context.Background(), refreshBase(t), nil, redirectClient(srv.URL))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if r, _ := book.Rate("oci.lb.base"); r.Amount != 0.0113 {
		t.Errorf("amount = %v", r.Amount)
	}
}

func TestRefresh_MalformedResponseDoesNotPoisonTheCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html>we are having some trouble</html>")
	}))
	defer srv.Close()

	cache := &fakeCache{
		have:  true,
		entry: CachedFeed{BookID: OCIBookID, ETag: `"abc"`, Payload: gzipped(t, tieredFeed)},
	}
	book, err := Refresh(context.Background(), refreshBase(t), cache, redirectClient(srv.URL))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if cache.saved != nil {
		t.Error("a malformed response overwrote the cached feed")
	}
	// It should still have fallen through to the good cached copy.
	if s, _ := book.Source(OCIBookID); s.Vintage != "2026-08-06T15:45:07.573Z" {
		t.Errorf("vintage = %q, want the cached feed to have been used", s.Vintage)
	}
}

func TestRefresh_NotModifiedUsesTheCachedCopy(t *testing.T) {
	var sentETag string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentETag = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	cache := &fakeCache{
		have:  true,
		entry: CachedFeed{BookID: OCIBookID, ETag: `"abc"`, Payload: gzipped(t, tieredFeed)},
	}
	book, err := Refresh(context.Background(), refreshBase(t), cache, redirectClient(srv.URL))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if sentETag != `"abc"` {
		t.Errorf("If-None-Match = %q, want the stored etag", sentETag)
	}
	if cache.saved != nil {
		t.Error("a 304 wrote to the cache")
	}
	if r, _ := book.Rate("oci.lb.base"); r.Amount != 0.0113 {
		t.Errorf("amount = %v", r.Amount)
	}
}

func TestRefresh_MissingSKUKeepsTheWholeEmbeddedBook(t *testing.T) {
	// A feed that parses but no longer carries B93030. Repricing the rest and
	// stamping the fresh vintage would claim a freshness the book does not
	// have, so the whole refresh is abandoned.
	const without = `{"lastUpdated":"2026-08-06T15:45:07.573Z","items":[
	  {"partNumber":"B00000","displayName":"Something Else","metricName":"Unit",
	   "currencyCodeLocalizations":[{"currencyCode":"USD","prices":[{"model":"PAY_AS_YOU_GO","value":1}]}]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, without)
	}))
	defer srv.Close()

	book, err := Refresh(context.Background(), refreshBase(t), &fakeCache{}, redirectClient(srv.URL))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if s, _ := book.Source(OCIBookID); s.Vintage != "2020-01-01" {
		t.Errorf("vintage = %q, want the stale-but-honest embedded one", s.Vintage)
	}
}

func TestRefresh_UnreadableCacheIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, tieredFeed)
	}))
	defer srv.Close()
	cache := &fakeCache{loadErr: errors.New("database is locked")}
	book, err := Refresh(context.Background(), refreshBase(t), cache, redirectClient(srv.URL))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if r, _ := book.Rate("oci.lb.base"); r.Amount != 0.0113 {
		t.Errorf("amount = %v", r.Amount)
	}
}

func TestRefresh_SaveFailureDoesNotLoseTheFreshPrices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, tieredFeed)
	}))
	defer srv.Close()
	cache := &fakeCache{saveErr: errors.New("disk full")}
	book, err := Refresh(context.Background(), refreshBase(t), cache, redirectClient(srv.URL))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if s, _ := book.Source(OCIBookID); s.Vintage != "2026-08-06T15:45:07.573Z" {
		t.Errorf("vintage = %q — caching is an optimisation, not a precondition", s.Vintage)
	}
}

func TestReprice_RejectsAFeedThatTurnsARateFree(t *testing.T) {
	// If a SKU's tier structure changes such that even the marginal tier is 0,
	// that is a signal for a human, not a $0.00 estimate.
	const freeNow = `{"lastUpdated":"2026-09-01T00:00:00Z","items":[
	  {"partNumber":"B93030","displayName":"Load Balancer Base","metricName":"Load Balancer",
	   "currencyCodeLocalizations":[{"currencyCode":"USD","prices":[{"model":"PAY_AS_YOU_GO","value":0}]}]}]}`
	feed, err := ParseOCIFeed([]byte(freeNow))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refreshBase(t).Reprice(OCIBookID, feed); err == nil {
		t.Fatal("Reprice accepted a zero amount")
	}
}

func TestParseOCIFeed_RejectsEmptyAndHeaderlessDocuments(t *testing.T) {
	for _, body := range []string{
		"",
		"not json",
		`{"lastUpdated":"2026-01-01T00:00:00Z","items":[]}`,
		`{"items":[{"partNumber":"X"}]}`,
	} {
		if _, err := ParseOCIFeed([]byte(body)); err == nil {
			t.Errorf("ParseOCIFeed accepted %q", body)
		}
	}
}

// redirectClient points every request at the test server, which is how the
// package-level OCIFeedURL constant gets exercised without making it a variable
// that production code could accidentally repoint.
func redirectClient(base string) *http.Client {
	return &http.Client{Transport: rewriteTransport{base: base}}
}

type rewriteTransport struct{ base string }

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := http.NewRequestWithContext(req.Context(), req.Method, t.base, nil)
	if err != nil {
		return nil, err
	}
	target.Header = req.Header
	return http.DefaultTransport.RoundTrip(target)
}

func failingClient() *http.Client {
	return &http.Client{Transport: failingTransport{}}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no such host")
}
