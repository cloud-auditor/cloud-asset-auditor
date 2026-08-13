package cost

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/pricing"
)

func testEstimator(t *testing.T, opts ...Option) *Estimator {
	t.Helper()
	book, err := pricing.Default()
	if err != nil {
		t.Fatalf("pricing.Default: %v", err)
	}
	return New(book, opts...)
}

// numeric parses a cost.monthly value, reporting whether it was a figure at
// all. Every assertion about zeros goes through this rather than through string
// comparison, so a change of glyph cannot quietly turn "no number" into one.
func numeric(monthly string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimPrefix(monthly, EstimateMark), 64)
	return v, err == nil
}

// Tests for the zero rule.

// TestEstimate_NeverSilentlyZero is the ship-blocking test. It walks every rule
// in the embedded book and feeds each an asset carrying nothing at all — no
// tags, no Raw, no status.
//
// The assertion is not "no number": a rule whose every quantity is a literal
// states a fact about itself ("a load balancer is one load balancer") and
// rightly prices a bare asset. The assertion is that a rule which needed
// something from the resource and got nothing reports that, and that no rule
// anywhere produces a zero. "$0.00" is a real price in OCI's feed, so an
// unpriced resource rendering as zero is indistinguishable from a free one.
func TestEstimate_NeverSilentlyZero(t *testing.T) {
	e := testEstimator(t)
	for i := range e.Book().Rules {
		rule := &e.Book().Rules[i]
		got := e.Estimate(core.Asset{Provider: "x", Type: rule.Type, ID: "id", Name: "n"})
		monthly := got.MonthlyString()

		v, isNum := numeric(monthly)
		switch {
		case !isNum:
			if monthly != ValueUnknown && monthly != ValueMetered {
				t.Errorf("rule %q: monthly = %q, want %q or %q", rule.Type, monthly, ValueUnknown, ValueMetered)
			}
		case v <= 0:
			t.Errorf("rule %q: bare asset priced as %v (%q) — a zero must come only from zero_when_status",
				rule.Type, v, monthly)
		default:
			// A figure from an empty asset is only defensible if nothing in the
			// rule was supposed to be read off the resource.
			for j, term := range rule.Terms {
				if term.Quantity.Literal == nil {
					t.Errorf("rule %q: bare asset priced as %v, but term %d wanted a quantity from the resource (%+v)",
						rule.Type, v, j, term.Quantity)
				}
			}
		}
		if got.Detail == "" {
			t.Errorf("rule %q: no detail — every answer must say where it came from", rule.Type)
		}
	}
}

func TestEstimate_UnknownTypeIsUnknownNotZero(t *testing.T) {
	e := testEstimator(t)
	got := e.Estimate(core.Asset{Provider: "aws", Type: "aws.ec2.instance", ID: "i-1"})

	if got.Priced {
		t.Fatalf("a type with no rule was priced: %+v", got)
	}
	if got.MonthlyString() != ValueUnknown {
		t.Errorf("monthly = %q, want %q", got.MonthlyString(), ValueUnknown)
	}
	if got.Basis != BasisUnknown {
		t.Errorf("basis = %q, want %q", got.Basis, BasisUnknown)
	}
	// The message has to distinguish "we have no rate" from "it is free",
	// because the two have different fixes.
	if !strings.Contains(got.Detail, "not a free resource") {
		t.Errorf("detail = %q, want it to deny that unknown means free", got.Detail)
	}
	if _, ok := got.Tags()[TagCurrency]; ok {
		t.Error("cost.currency set on an asset with no cost")
	}
}

// TestEstimate_ZeroQuantityDoesNotProduceAZeroTotal covers the subtler failure:
// a rule that resolves fine but reads a size of 0 off the asset. A volume
// reporting 0 GB is a bad reading, not a free volume.
func TestEstimate_ZeroQuantityDoesNotProduceAZeroTotal(t *testing.T) {
	e := testEstimator(t)
	got := e.Estimate(core.Asset{
		Provider: "oci", Type: "oci.block_volume", ID: "ocid1.volume.oc1..z",
		Tags: map[string]string{"size_gb": "0"},
	})
	if v, isNum := numeric(got.MonthlyString()); isNum {
		t.Fatalf("a 0 GB volume priced as %v; want %q", v, ValueUnknown)
	}
	if got.MonthlyString() != ValueUnknown {
		t.Errorf("monthly = %q, want %q", got.MonthlyString(), ValueUnknown)
	}
}

// TestEstimate_StatusZeroIsTheOnlyZero pins the one legitimate path to a zero
// figure and asserts it always arrives with its explanation.
func TestEstimate_StatusZeroIsTheOnlyZero(t *testing.T) {
	e := testEstimator(t)
	got := e.Estimate(core.Asset{
		Provider: "oci", Type: "oci.compute.instance", ID: "ocid1.instance.oc1..s",
		Status: "STOPPED",
		Tags:   map[string]string{"shape": "VM.Standard.E4.Flex", "ocpus": "2", "memory_gb": "32"},
	})

	if !got.Priced {
		t.Fatalf("a stopped instance should still carry a figure: %+v", got)
	}
	v, isNum := numeric(got.MonthlyString())
	if !isNum || v != 0 {
		t.Fatalf("monthly = %q, want a zero figure", got.MonthlyString())
	}
	if !strings.Contains(got.Detail, "not billed") {
		t.Errorf("a zero must carry its zero_note; detail = %q", got.Detail)
	}
	// The glyph rule outranks the shape of the number: this zero was derived by
	// this tool, not reported by a billing API, so it keeps its tilde.
	if !strings.HasPrefix(got.MonthlyString(), EstimateMark) {
		t.Errorf("monthly = %q, want the ~ glyph on a derived zero", got.MonthlyString())
	}
}

// Tests for marginal-tier pricing.

// TestEstimate_TieredRatePricesAtTheMargin guards the trap that made two
// research reports report "$0" for a load balancer: OCI encodes the Always Free
// allowance as a first tier at zero, and reading prices[0] makes the first of
// everything free forever.
func TestEstimate_TieredRatePricesAtTheMargin(t *testing.T) {
	e := testEstimator(t)
	lb, ok := e.Book().Rate("oci.lb.base")
	if !ok {
		t.Fatal("oci.lb.base missing from the embedded book")
	}
	if lb.TierNote == "" {
		t.Fatal("oci.lb.base is expected to be a tiered SKU; the test guards the wrong rate otherwise")
	}

	got := e.Estimate(core.Asset{Provider: "oci", Type: "oci.load_balancer", ID: "ocid1.loadbalancer.oc1..a"})
	v, isNum := numeric(got.MonthlyString())
	if !isNum {
		t.Fatalf("load balancer not priced: %+v", got)
	}
	if v <= 0 {
		t.Fatalf("load balancer priced at %v — the first (free) tier was used instead of the marginal one", v)
	}
	want := lb.Amount * e.Book().HoursPerMonth
	if diff := v - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("monthly = %v, want %v (marginal rate x hours)", v, want)
	}
	// The reader has to be told the figure ignores an allowance they may be
	// sitting inside, or a small tenancy reads this as its bill.
	if !strings.Contains(got.Detail, "marginal tier") {
		t.Errorf("detail = %q, want it to name the marginal tier", got.Detail)
	}
}

// TestEstimate_BlockVolumeConformance checks the arithmetic end to end against
// a figure Oracle publishes independently: Balanced block storage is
// ~$0.0425/GB-month, so 200 GB is $8.50.
func TestEstimate_BlockVolumeConformance(t *testing.T) {
	e := testEstimator(t)
	got := e.Estimate(core.Asset{
		Provider: "oci", Type: "oci.block_volume", ID: "ocid1.volume.oc1..a", Name: "prod-data-01",
		Tags: map[string]string{"size_gb": "200", "vpus_per_gb": "10"},
	})
	v, isNum := numeric(got.MonthlyString())
	if !isNum {
		t.Fatalf("not priced: %+v", got)
	}
	if diff := v - 8.50; diff > 0.01 || diff < -0.01 {
		t.Errorf("monthly = %v, want 8.50 (200 GB Balanced at Oracle's published $0.0425/GB-mo)", v)
	}
	if got.Basis != BasisInferred {
		t.Errorf("basis = %q, want %q — both quantities came off the asset", got.Basis, BasisInferred)
	}
}

// Tests that the bases are genuinely distinguishable to a reader.

func TestEstimate_BasisReflectsWhereTheQuantityCameFrom(t *testing.T) {
	e := testEstimator(t)
	shape := map[string]string{"shape": "VM.Standard.E4.Flex"}

	cases := []struct {
		name      string
		asset     core.Asset
		wantBasis Basis
		wantIn    string
	}{
		{
			name: "tag",
			asset: core.Asset{Provider: "oci", Type: "oci.compute.instance", ID: "a",
				Tags: map[string]string{"shape": "VM.Standard.E4.Flex", "ocpus": "2", "memory_gb": "32"}},
			wantBasis: BasisInferred,
			wantIn:    "tag ocpus",
		},
		{
			name: "raw",
			asset: core.Asset{Provider: "oci", Type: "oci.compute.instance", ID: "b", Tags: shape,
				Raw: json.RawMessage(`{"shapeConfig":{"ocpus":2,"memoryInGBs":32}}`)},
			wantBasis: BasisInferred,
			wantIn:    "raw shapeConfig.ocpus",
		},
		{
			name:      "shape default",
			asset:     core.Asset{Provider: "oci", Type: "oci.compute.instance", ID: "c", Tags: shape},
			wantBasis: BasisAssumed,
			wantIn:    "shape default_ocpu",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := e.Estimate(tc.asset)
			if !got.Priced {
				t.Fatalf("not priced: %+v", got)
			}
			if got.Basis != tc.wantBasis {
				t.Errorf("basis = %q, want %q (detail: %s)", got.Basis, tc.wantBasis, got.Detail)
			}
			if !strings.Contains(got.Detail, tc.wantIn) {
				t.Errorf("detail = %q, want it to name the quantity source %q", got.Detail, tc.wantIn)
			}
			if !strings.HasPrefix(got.MonthlyString(), EstimateMark) {
				t.Errorf("monthly = %q, want the ~ glyph on a list-price estimate", got.MonthlyString())
			}
		})
	}

	// A size read off the resource and a size the book supplied must not price
	// the same, or the basis distinction is decorative.
	real := e.Estimate(cases[0].asset)
	guessed := e.Estimate(cases[2].asset)
	if real.Monthly == guessed.Monthly {
		t.Error("a 2-OCPU/32 GB instance priced the same as the book's default size")
	}
}

func TestEstimate_UnknownShapeSaysWhichOne(t *testing.T) {
	e := testEstimator(t)
	got := e.Estimate(core.Asset{Provider: "oci", Type: "oci.compute.instance", ID: "a",
		Tags: map[string]string{"shape": "VM.Imaginary9.Flex"}})

	if got.Priced {
		t.Fatalf("an unlisted shape was priced: %+v", got)
	}
	if !strings.Contains(got.Detail, `VM.Imaginary9.Flex`) || !strings.Contains(got.Detail, "add it to the price book") {
		t.Errorf("detail = %q, want the shape name and what to do about it", got.Detail)
	}
}

// A shape that exists only as an Always Free offering is a documented gap, and
// reads very differently from a missing entry.
func TestEstimate_NoteOnlyShapeCarriesItsNote(t *testing.T) {
	e := testEstimator(t)
	if _, ok := e.Book().Shape("oci", "VM.Standard.E2.1.Micro"); !ok {
		t.Skip("the note-only micro shape is not in this book")
	}
	got := e.Estimate(core.Asset{Provider: "oci", Type: "oci.compute.instance", ID: "a",
		Tags: map[string]string{"shape": "VM.Standard.E2.1.Micro"}})

	if got.Priced {
		t.Fatalf("a shape with no marginal rate was priced: %+v", got)
	}
	if strings.Contains(got.Detail, "add it to the price book") {
		t.Errorf("detail = %q, want the shape's own note rather than the missing-entry message", got.Detail)
	}
	if !strings.Contains(got.Detail, "Always Free") {
		t.Errorf("detail = %q, want the documented reason", got.Detail)
	}
}

func TestEstimate_MeteredIsNotUnknown(t *testing.T) {
	e := testEstimator(t)
	got := e.Estimate(core.Asset{Provider: "cloudflare", Type: "cloudflare.r2_bucket", ID: "b"})

	if got.MonthlyString() != ValueMetered {
		t.Errorf("monthly = %q, want %q", got.MonthlyString(), ValueMetered)
	}
	if got.Basis != BasisUnpriceable {
		t.Errorf("basis = %q, want %q — a known consumption model is not a gap in the book", got.Basis, BasisUnpriceable)
	}
}

func TestEstimate_MeasuredOverridesAndDropsTheGlyph(t *testing.T) {
	idx := MeasuredMap{}
	idx.Put("oci", "ocid1.instance.oc1..m", MeasuredCost{
		Amount: 412.90, Currency: "USD",
		Detail: "actual cost, 2026-07-01..2026-07-31, incl. discount",
	})
	e := testEstimator(t, WithMeasured(idx))

	got := e.Estimate(core.Asset{Provider: "oci", Type: "oci.compute.instance", ID: "ocid1.instance.oc1..m",
		Tags: map[string]string{"shape": "VM.Standard.E4.Flex"}})

	if got.Basis != BasisMeasured {
		t.Fatalf("basis = %q, want %q", got.Basis, BasisMeasured)
	}
	if got.MonthlyString() != "412.90" {
		t.Errorf("monthly = %q, want %q — a billed figure carries no ~", got.MonthlyString(), "412.90")
	}
	if got.Estimated() {
		t.Error("a measured figure reported itself as an estimate")
	}
}

// Tests for the tag contract.

func TestTags_ExactlyFourKeysAndNoMutation(t *testing.T) {
	e := testEstimator(t)
	original := map[string]string{"shape": "VM.Standard.E4.Flex", "env": "prod"}
	a := core.Asset{Provider: "oci", Type: "oci.compute.instance", ID: "a", Tags: original}

	out := e.Estimate(a).ApplyTo(a)

	if len(original) != 2 {
		t.Fatalf("the asset's own tag map was mutated: %v", original)
	}
	want := map[string]bool{TagMonthly: true, TagCurrency: true, TagBasis: true, TagDetail: true}
	for k := range out.Tags {
		if strings.HasPrefix(k, TagPrefix) && !want[k] {
			t.Errorf("unexpected cost tag %q — the contract is exactly four keys", k)
		}
	}
	for k := range want {
		if _, ok := out.Tags[k]; !ok {
			t.Errorf("missing cost tag %q", k)
		}
	}
	if out.Tags["env"] != "prod" {
		t.Error("annotation dropped an existing tag")
	}
	// A tag value carrying a newline breaks CSV, the SSE frame format and every
	// table renderer, and price-book notes are YAML-folded across lines.
	for k, v := range out.Tags {
		if strings.ContainsAny(v, "\n\r") {
			t.Errorf("tag %q contains a newline: %q", k, v)
		}
	}
}

func TestTags_ZeroIsNeverUsedForUnknown(t *testing.T) {
	e := testEstimator(t)
	for _, tp := range []string{"cloudflare.zone", "cloudflare.r2_bucket", "v1.ConfigMap", "oci.subnet"} {
		got := e.Estimate(core.Asset{Provider: "p", Type: tp, ID: "x"}).Tags()[TagMonthly]
		if v, isNum := numeric(got); isNum {
			t.Errorf("%s: cost.monthly = %q (%v) — an unpriced asset must never carry a figure", tp, got, v)
		}
	}
}

// A positive figure must not round to a zero at any magnitude. Four decimals
// cover a fraction of a cent, and below that the rendering becomes an
// inequality — because a quantity tag of "0.000001" is still a claim that the
// resource costs something, and "0.0000" would deny it.
func TestFormatAmount_NoPositiveFigureRoundsToZero(t *testing.T) {
	for _, v := range []float64{0.004, 0.0001, 1e-5, 1e-9, 1e-300} {
		got := formatAmount(v)
		if f, err := strconv.ParseFloat(got, 64); err == nil && f == 0 {
			t.Errorf("formatAmount(%v) = %q, which reads as zero", v, got)
		}
	}
	// Zero itself still renders as a figure: it reaches here only from a rule's
	// zero_when_status, which carries the note that explains it.
	if got := formatAmount(0); got != "0.00" {
		t.Errorf("formatAmount(0) = %q, want %q", got, "0.00")
	}
}

// Estimated is the aggregate figure type — a pod attribution, a cluster's
// unrequested capacity. Its zero value is what a cluster where nothing could be
// attributed produces, and it must not present that as money: "~$0.00" against
// 29 unattributed pods reads as "these pods are free" rather than "this tool
// attributed nothing". Money already gets this right; this pins the pair.
func TestEstimated_NoMoneyRendersAsNoMoneyNotZero(t *testing.T) {
	for _, e := range []Estimated{{}, {Currency: "USD"}, {Currency: "EUR", Amount: 0}} {
		if got := e.String(); got != NoMoney {
			t.Errorf("Estimated%+v.String() = %q, want %q", e, got, NoMoney)
		}
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out struct{ Monthly, Display string }
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// monthly is present and unparseable rather than absent: a consumer that
		// defaults a missing key to 0 would land on exactly the zero this package
		// refuses to state.
		if out.Monthly != NoMoney || out.Display != NoMoney {
			t.Errorf("Estimated%+v JSON = %s, want both fields %q", e, b, NoMoney)
		}
	}
	if got := (Estimated{Currency: "USD", Amount: 8.5}).String(); got != "~$8.50" {
		t.Errorf("a real figure regressed: %q", got)
	}
}

// The whole-report form of the rule: over an estate where nothing prices, no
// renderer may emit a zero money figure anywhere — not in a rollup, not in the
// Kubernetes section, not in the JSON a dashboard reads.
func TestRender_NothingPricedMeansNoZeroFigureAnywhere(t *testing.T) {
	e := testEstimator(t)
	rep := e.Report([]core.Asset{
		{Provider: "kubernetes", Type: "v1.Pod", ID: "p1", AccountID: "c1", Name: "p1"},
		{Provider: "kubernetes", Type: "v1.Node", ID: "n1", AccountID: "c1", Name: "n1"},
		{Provider: "cloudflare", Type: "cloudflare.r2_bucket", ID: "b1"},
	}, Options{Now: time.Unix(0, 0).UTC()})

	if !rep.Totals.Balances() {
		t.Errorf("totals do not balance: %+v", rep.Totals)
	}
	for _, format := range []string{"table", "json", "csv", "markdown"} {
		var buf strings.Builder
		if err := Render(rep, format, &buf); err != nil {
			t.Fatalf("render %s: %v", format, err)
		}
		for _, bad := range []string{"~0.00", "$0.00", "~$0.00", "0.0000"} {
			if strings.Contains(buf.String(), bad) {
				t.Errorf("%s output contains %q where nothing could be priced", format, bad)
			}
		}
	}
}

// Tests for Annotate, the streaming stage.

func TestAnnotate_ForwardsEveryAssetAndClosesExactlyOnce(t *testing.T) {
	e := testEstimator(t)
	in := make(chan core.Asset, 3)
	in <- core.Asset{Provider: "oci", Type: "oci.load_balancer", ID: "lb"}
	in <- core.Asset{Provider: "cloudflare", Type: "cloudflare.r2_bucket", ID: "r2"}
	in <- core.Asset{Provider: "x", Type: "nope", ID: "n"}
	close(in)

	out := e.Annotate(t.Context(), in)
	var got []core.Asset
	for a := range out {
		got = append(got, a)
	}
	if len(got) != 3 {
		t.Fatalf("forwarded %d assets, want 3", len(got))
	}
	// Receiving from a closed channel is fine; receiving from one closed twice
	// panics in the producer, which -race and the second receive below surface.
	if a, ok := <-out; ok {
		t.Fatalf("channel not closed after drain, got %+v", a)
	}
	if a, ok := <-out; ok {
		t.Fatalf("second receive on a closed channel yielded %+v", a)
	}
	for _, a := range got {
		if _, ok := a.Tags[TagMonthly]; !ok {
			t.Errorf("%s: no %s tag", a.ID, TagMonthly)
		}
	}
}

func TestAnnotate_RespectsCancellation(t *testing.T) {
	e := testEstimator(t)
	ctx, cancel := context.WithCancel(t.Context())

	// Deliberately never closed: the stage must stop because the context is
	// done, not because the input ended.
	in := make(chan core.Asset)
	out := e.Annotate(ctx, in)
	cancel()

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("received an asset after cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("output channel did not close after the context was cancelled")
	}
}

// A blocked consumer must not pin the stage open past a cancellation, or Ctrl+C
// takes as long as the slowest renderer.
func TestAnnotate_CancellationUnblocksAPendingSend(t *testing.T) {
	e := testEstimator(t)
	ctx, cancel := context.WithCancel(t.Context())

	in := make(chan core.Asset, 1)
	in <- core.Asset{Provider: "oci", Type: "oci.load_balancer", ID: "lb"}
	out := e.Annotate(ctx, in) // nobody receives
	cancel()

	select {
	case _, ok := <-out:
		if ok {
			// The value was already in flight; the next receive must close.
			if _, ok := <-out; ok {
				t.Fatal("stage kept producing after cancellation")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("output channel did not close after the context was cancelled")
	}
}

func TestAnnotateSlice_LeavesTheInputAlone(t *testing.T) {
	e := testEstimator(t)
	in := []core.Asset{{Provider: "oci", Type: "oci.load_balancer", ID: "lb"}}
	out := e.AnnotateSlice(in)

	if in[0].Tags != nil {
		t.Errorf("input asset was annotated in place: %v", in[0].Tags)
	}
	if out[0].Tags[TagMonthly] == "" {
		t.Error("output asset was not annotated")
	}
}

// Tests for the report.

func sampleEstate() []core.Asset {
	return []core.Asset{
		{Provider: "oci", AccountID: "tenancy", Region: "phx", Type: "oci.compute.instance",
			ID: "ocid1.instance.oc1.phx.node1", Name: "oke-node-1",
			Tags: map[string]string{"shape": "VM.Standard.E4.Flex", "ocpus": "4", "memory_gb": "64"}},
		{Provider: "oci", AccountID: "tenancy", Region: "phx", Type: "oci.block_volume",
			ID: "ocid1.volume.oc1..v", Name: "prod-data-01",
			Tags: map[string]string{"size_gb": "200", "vpus_per_gb": "10"}},
		{Provider: "oci", AccountID: "tenancy", Region: "phx", Type: "oci.load_balancer",
			ID: "ocid1.loadbalancer.oc1..lb", Name: "edge-lb"},
		{Provider: "oci", AccountID: "tenancy", Region: "phx", Type: "oci.subnet",
			ID: "ocid1.subnet.oc1..s", Name: "private"},
		{Provider: "cloudflare", AccountID: "acct", Type: "cloudflare.r2_bucket", ID: "r2-1", Name: "assets"},
		{Provider: "cloudflare", AccountID: "acct", Type: "cloudflare.zone", ID: "z1", Name: "example.com"},
		{Provider: "tailscale", AccountID: "-", Type: "tailscale.user", ID: "u1", Name: "ada"},
		{Provider: "tailscale", AccountID: "-", Type: "tailscale.user", ID: "u2", Name: "grace"},
		{Provider: "tailscale", AccountID: "-", Type: "tailscale.device", ID: "d1", Name: "ci-runner",
			Tags: map[string]string{"acl_tags": "tag:ci"}},
		{Provider: "tailscale", AccountID: "-", Type: "tailscale.device", ID: "d2", Name: "laptop"},
	}
}

// TestReport_UnpricedAccountingAddsUp is the structural guarantee behind the
// whole report: every asset lands in exactly one bucket, so a total can never
// silently omit part of the estate.
func TestReport_UnpricedAccountingAddsUp(t *testing.T) {
	e := testEstimator(t)
	assets := append(sampleEstate(), kubeEstate()...)
	rep := e.Report(assets, Options{Now: time.Unix(0, 0).UTC()})

	if rep.Totals.Assets != len(assets) {
		t.Fatalf("totals.assets = %d, want %d", rep.Totals.Assets, len(assets))
	}
	if !rep.Totals.Balances() {
		t.Errorf("buckets do not add up: priced=%d metered=%d unknown=%d attributed=%d, assets=%d",
			rep.Totals.Priced, rep.Totals.Metered, rep.Totals.Unknown,
			rep.Totals.Attributed, rep.Totals.Assets)
	}
	if got := rep.Unpriced.Assets(); got != rep.Totals.UnpricedCount() {
		t.Errorf("unpriced accounting covers %d assets, but %d were not priced", got, rep.Totals.UnpricedCount())
	}

	// The same identity, per group.
	var sum int
	for _, g := range rep.Groups {
		if g.Priced+g.Metered+g.Unknown+g.Attributed != g.Assets {
			t.Errorf("group %q does not balance: %+v", g.Key, g)
		}
		sum += g.Assets
	}
	if sum != rep.Totals.Assets {
		t.Errorf("groups cover %d assets, want %d", sum, rep.Totals.Assets)
	}

	// Every bucket's per-type counts have to add back up to the bucket, or the
	// "NOT PRICED" section is a sample rather than an accounting.
	for name, b := range map[string]UnpricedBucket{
		"metered": rep.Unpriced.Metered, "unknown": rep.Unpriced.Unknown, "attributed": rep.Unpriced.Attributed,
	} {
		var n int
		for _, ut := range b.Types {
			n += ut.Assets
			if ut.Reason == "" {
				t.Errorf("%s bucket: type %q has no reason", name, ut.Type)
			}
		}
		if n != b.Assets {
			t.Errorf("%s bucket: types cover %d assets, bucket says %d", name, n, b.Assets)
		}
	}
}

func TestReport_UnknownAndMeteredRollUpSeparately(t *testing.T) {
	e := testEstimator(t)
	rep := e.Report(sampleEstate(), Options{})

	if rep.Unpriced.Metered.Assets == 0 {
		t.Error("no metered assets counted; the R2 bucket should be one")
	}
	if rep.Unpriced.Unknown.Assets == 0 {
		t.Error("no unknown assets counted; the subnet should be one")
	}
	for _, ut := range rep.Unpriced.Metered.Types {
		if ut.Type == "oci.subnet" {
			t.Error("a no-charge object was filed as metered")
		}
	}
}

func TestReport_TotalIsSegregatedByCurrency(t *testing.T) {
	e := testEstimator(t)
	// A rate in a second currency has to stay in its own bucket: no exchange
	// rate is applied anywhere in this tool.
	rep := e.Report(sampleEstate(), Options{})
	for _, m := range rep.Totals.Monthly {
		if m.Currency == "" {
			t.Error("a money bucket with no currency")
		}
	}
	blob, err := json.Marshal(rep.Totals.Monthly)
	if err != nil {
		t.Fatal(err)
	}
	// Money must never serialise as a JSON number.
	var raw []map[string]any
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatal(err)
	}
	for _, m := range raw {
		for k, v := range m {
			if _, isNum := v.(float64); isNum {
				t.Errorf("money field %q serialised as a number (%v); estimates must be strings", k, v)
			}
		}
	}
}

func TestReport_TopNRanksAndTruncates(t *testing.T) {
	e := testEstimator(t)
	rep := e.Report(sampleEstate(), Options{TopN: 2})

	if len(rep.Top) != 2 {
		t.Fatalf("top has %d rows, want 2", len(rep.Top))
	}
	if rep.Top[0].amount < rep.Top[1].amount {
		t.Error("top-N is not sorted by cost descending")
	}
	all := e.Report(sampleEstate(), Options{TopN: 0})
	if len(all.Top) != rep.Totals.Priced {
		t.Errorf("--top 0 returned %d rows, want every priced asset (%d)", len(all.Top), rep.Totals.Priced)
	}
}

func TestReport_MeshShowsEveryPlanAndStaysOutOfTheTotal(t *testing.T) {
	e := testEstimator(t)
	rep := e.Report(sampleEstate(), Options{})

	ts, found := MeshRollup{}, false
	for _, m := range rep.Mesh {
		if m.Provider == "tailscale" {
			ts, found = m, true
		}
	}
	if !found {
		t.Fatal("no tailscale mesh rollup")
	}
	if len(ts.Plans) < 2 {
		t.Errorf("got %d plans, want every tier — the API does not say which one the tailnet is on", len(ts.Plans))
	}
	for _, p := range ts.Plans {
		if !strings.HasPrefix(p.Monthly, EstimateMark) {
			t.Errorf("plan %q monthly = %q, want the ~ glyph", p.Plan, p.Monthly)
		}
	}
	// Seat money is account-wide and unattributable, so it must not appear in
	// the per-asset totals — where it would be summed against infrastructure.
	for _, g := range rep.Groups {
		if g.Key != "tailscale" {
			continue
		}
		for _, m := range g.Monthly {
			seats := 2 * 8.0 // two users at the Standard rate
			if m.Estimated >= seats {
				t.Errorf("tailscale group total %v looks like it absorbed the seat rollup", m.Estimated)
			}
		}
	}
}

func TestParseGroupBy(t *testing.T) {
	for in, want := range map[string]string{
		"": "provider", "Provider": "provider", "type": "type",
		"account": "account", "tag:Env": "tag:Env",
	} {
		got, err := ParseGroupBy(in)
		if err != nil {
			t.Errorf("ParseGroupBy(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseGroupBy(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"cost", "tag:", "name"} {
		if _, err := ParseGroupBy(bad); err == nil {
			t.Errorf("ParseGroupBy(%q) accepted an invalid dimension", bad)
		}
	}
}

// Tests for Kubernetes attribution.

func kubeEstate() []core.Asset {
	nodeRaw := `{"spec":{"providerID":"ocid1.instance.oc1.phx.orphan"},
	  "status":{"capacity":{"cpu":"8","memory":"32Gi"},
	            "allocatable":{"cpu":"7800m","memory":"30Gi"}}}`
	podRaw := func(node, cpu, mem string) json.RawMessage {
		return json.RawMessage(`{"spec":{"nodeName":"` + node + `","containers":[
		  {"resources":{"requests":{"cpu":"` + cpu + `","memory":"` + mem + `"}}}]}}`)
	}
	return []core.Asset{
		{Provider: "kubernetes", AccountID: "cluster-a", Type: kubeNodeType, ID: "n1", Name: "node-1",
			Raw: json.RawMessage(nodeRaw)},
		{Provider: "kubernetes", AccountID: "cluster-a", Type: kubePodType, ID: "p1", Name: "api-0",
			Status: "Running", Raw: podRaw("node-1", "500m", "1Gi")},
		{Provider: "kubernetes", AccountID: "cluster-a", Type: kubePodType, ID: "p2", Name: "worker-0",
			Status: "Running", Raw: podRaw("node-1", "1", "2Gi")},
		// No Raw: attribution is impossible and must say so rather than guess.
		{Provider: "kubernetes", AccountID: "cluster-a", Type: kubePodType, ID: "p3", Name: "no-raw",
			Status: "Running"},
	}
}

func TestReport_KubeNodesArePricedAndPodsOnlyAttribute(t *testing.T) {
	e := testEstimator(t)
	rep := e.Report(kubeEstate(), Options{TopN: 5})

	if rep.Kubernetes == nil || len(rep.Kubernetes.Clusters) != 1 {
		t.Fatalf("expected one Kubernetes cluster section, got %+v", rep.Kubernetes)
	}
	c := rep.Kubernetes.Clusters[0]
	if c.NodesPriced != 1 {
		t.Fatalf("nodes priced = %d, want 1", c.NodesPriced)
	}
	if c.NodeMonthly.Estimated <= 0 {
		t.Fatalf("node monthly = %v, want a positive estimate", c.NodeMonthly.Estimated)
	}
	if c.PodsAttributed != 2 {
		t.Errorf("pods attributed = %d, want 2 (the third has no Raw)", c.PodsAttributed)
	}

	// Attribution can never exceed the money it is attributing.
	if c.Attributed.Amount > c.NodeMonthly.Estimated+0.01 {
		t.Errorf("attributed %v exceeds node cost %v", c.Attributed, c.NodeMonthly.Estimated)
	}
	if diff := c.Attributed.Amount + c.Unrequested.Amount - c.NodeMonthly.Estimated; diff > 0.01 || diff < -0.01 {
		t.Errorf("attributed (%v) + unrequested (%v) != node cost (%v)",
			c.Attributed.Amount, c.Unrequested.Amount, c.NodeMonthly.Estimated)
	}

	// The node's money is in the total exactly once; the pods' is not in it at all.
	var total float64
	for _, m := range rep.Totals.Monthly {
		total += m.Total()
	}
	if diff := total - c.NodeMonthly.Estimated; diff > 0.01 || diff < -0.01 {
		t.Errorf("grand total %v, want the node cost %v alone — pods must not be summed with nodes",
			total, c.NodeMonthly.Estimated)
	}
	if rep.Totals.Attributed != 2 {
		t.Errorf("totals.attributed = %d, want 2", rep.Totals.Attributed)
	}
}

func TestReport_PodWithoutRawSaysWhatIsMissing(t *testing.T) {
	e := testEstimator(t)
	rep := e.Report(kubeEstate(), Options{ShowUnpriced: true})

	var found bool
	for _, a := range rep.Assets {
		if a.ID != "p3" {
			continue
		}
		found = true
		if a.Monthly != ValueUnknown {
			t.Errorf("monthly = %q, want %q", a.Monthly, ValueUnknown)
		}
		if !strings.Contains(a.Detail, "--include-raw") {
			t.Errorf("detail = %q, want it to name the missing input", a.Detail)
		}
	}
	if !found {
		t.Fatal("pod p3 missing from the report")
	}
}

func TestReport_KubeNodeOnAnAuditedInstanceIsNotDoubleCounted(t *testing.T) {
	e := testEstimator(t)
	const ocid = "ocid1.instance.oc1.phx.node1"
	assets := []core.Asset{
		{Provider: "oci", AccountID: "tenancy", Type: "oci.compute.instance", ID: ocid, Name: "oke-node-1",
			Tags: map[string]string{"shape": "VM.Standard.E4.Flex", "ocpus": "4", "memory_gb": "64"}},
		{Provider: "kubernetes", AccountID: "cluster-a", Type: kubeNodeType, ID: "n1", Name: "node-1",
			Raw: json.RawMessage(`{"spec":{"providerID":"oci://` + ocid + `"},
			  "status":{"allocatable":{"cpu":"3800m","memory":"60Gi"}}}`)},
	}
	rep := e.Report(assets, Options{})

	instance := e.Estimate(assets[0])
	var total float64
	for _, m := range rep.Totals.Monthly {
		total += m.Total()
	}
	if diff := total - instance.Monthly; diff > 0.01 || diff < -0.01 {
		t.Errorf("grand total %v, want the instance's cost %v once — the node must not add it again",
			total, instance.Monthly)
	}
	if rep.Totals.Attributed != 1 {
		t.Errorf("totals.attributed = %d, want 1 (the node)", rep.Totals.Attributed)
	}
	c := rep.Kubernetes.Clusters[0]
	if c.CountedElsewhere.Amount <= 0 {
		t.Error("the section does not report that the node's cost is counted elsewhere")
	}
	if len(c.RateSources) == 0 || c.RateSources[0].Source != rateViaInstance {
		t.Errorf("rate sources = %+v, want the instance join first", c.RateSources)
	}
}

func TestQuantityParsing(t *testing.T) {
	cores, ok := quantityCores("500m")
	if !ok || cores != 0.5 {
		t.Errorf("quantityCores(500m) = %v, %v; want 0.5", cores, ok)
	}
	gib, ok := quantityGiB("2Gi")
	if !ok || gib != 2 {
		t.Errorf("quantityGiB(2Gi) = %v, %v; want 2", gib, ok)
	}
	if _, ok := quantityCores("not-a-quantity"); ok {
		t.Error("a malformed quantity parsed")
	}
}

// Tests for renderers.

func TestRender_EveryFormatCarriesTheCaveatWithTheTotal(t *testing.T) {
	e := testEstimator(t)
	rep := e.Report(append(sampleEstate(), kubeEstate()...), Options{TopN: 5, Now: time.Unix(0, 0).UTC()})

	for _, format := range []string{"table", "json", "csv", "markdown"} {
		t.Run(format, func(t *testing.T) {
			var sb strings.Builder
			if err := Render(rep, format, &sb); err != nil {
				t.Fatalf("Render(%s): %v", format, err)
			}
			out := sb.String()
			if out == "" {
				t.Fatal("empty output")
			}
			// The caveat has to be in the same document as the number, in every
			// format — a shareable artifact travels without its author.
			marker := "not an invoice"
			if !strings.Contains(strings.ToLower(out), marker) {
				t.Errorf("%s output does not contain %q", format, marker)
			}
			if !strings.Contains(out, EstimateMark) {
				t.Errorf("%s output carries no ~ glyph on its estimates", format)
			}
			// The denominator travels with the total everywhere but CSV, where
			// it rides in the TOTAL row's detail column.
			if !strings.Contains(out, "metered") || !strings.Contains(out, ValueUnknown) {
				t.Errorf("%s output does not report its unpriced coverage", format)
			}
		})
	}
}

func TestRender_JSONMonthlyIsAlwaysAString(t *testing.T) {
	e := testEstimator(t)
	rep := e.Report(sampleEstate(), Options{})

	var sb strings.Builder
	if err := Render(rep, "json", &sb); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Disclaimer string `json:"disclaimer"`
		Assets     []struct {
			Monthly json.RawMessage `json:"monthly"`
		} `json:"assets"`
	}
	if err := json.Unmarshal([]byte(sb.String()), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if doc.Disclaimer == "" {
		t.Error("disclaimer is a required top-level field and it is empty")
	}
	for i, a := range doc.Assets {
		if len(a.Monthly) == 0 || a.Monthly[0] != '"' {
			t.Errorf("assets[%d].monthly = %s, want a JSON string", i, a.Monthly)
		}
	}
}

// Every money-bearing field in the JSON report has to be a string, including
// the Kubernetes ones. A dashboard that read "unrequested" as a number would be
// adding an estimate to whatever else was on the page.
func TestRender_JSONHasNoBareMoneyNumbers(t *testing.T) {
	e := testEstimator(t)
	rep := e.Report(append(sampleEstate(), kubeEstate()...), Options{})

	var sb strings.Builder
	if err := Render(rep, "json", &sb); err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal([]byte(sb.String()), &doc); err != nil {
		t.Fatal(err)
	}
	// Money and Estimated both serialise a "display" key and nothing else that
	// is a number, so "any object with a display key is entirely strings" is
	// exactly the invariant, and it holds wherever such an object is nested.
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch node := v.(type) {
		case map[string]any:
			if _, isMoney := node["display"]; isMoney {
				for k, sub := range node {
					if _, isNum := sub.(float64); isNum {
						t.Errorf("%s.%s serialised as a JSON number (%v); money must be a string", path, k, sub)
					}
				}
			}
			for k, sub := range node {
				walk(path+"."+k, sub)
			}
		case []any:
			for i, sub := range node {
				walk(fmt.Sprintf("%s[%d]", path, i), sub)
			}
		}
	}
	walk("$", doc)

	// And the per-asset figures, which are bare strings rather than objects.
	top := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(sb.String()), &top); err != nil {
		t.Fatal(err)
	}
	for _, list := range []string{"assets", "top"} {
		var rows []map[string]json.RawMessage
		if err := json.Unmarshal(top[list], &rows); err != nil {
			t.Fatalf("%s: %v", list, err)
		}
		for i, row := range rows {
			if m := row["monthly"]; len(m) == 0 || m[0] != '"' {
				t.Errorf("%s[%d].monthly = %s, want a JSON string", list, i, m)
			}
		}
	}
}

func TestRender_UnknownFormatIsAnError(t *testing.T) {
	if err := Render(&Report{}, "yaml", &strings.Builder{}); err == nil {
		t.Error("an unknown format was accepted")
	}
}

func TestRender_EmptyEstate(t *testing.T) {
	e := testEstimator(t)
	rep := e.Report(nil, Options{})
	for _, format := range []string{"table", "json", "csv", "markdown"} {
		var sb strings.Builder
		if err := Render(rep, format, &sb); err != nil {
			t.Errorf("Render(%s) on an empty estate: %v", format, err)
		}
	}
	if !rep.Totals.Balances() {
		t.Error("an empty report does not balance")
	}
}

func TestNilBookPricesNothingRatherThanZero(t *testing.T) {
	e := New(nil)
	got := e.Estimate(core.Asset{Provider: "oci", Type: "oci.load_balancer", ID: "lb"})
	if got.MonthlyString() != ValueUnknown {
		t.Errorf("monthly = %q, want %q", got.MonthlyString(), ValueUnknown)
	}
	if got.Detail == "" {
		t.Error("no detail explaining the missing book")
	}

	// The whole report path has to survive it too — a missing book must cost a
	// column, never the command.
	rep := e.Report(append(sampleEstate(), kubeEstate()...), Options{})
	if rep.Totals.Priced != 0 || !rep.Totals.Balances() {
		t.Errorf("nil-book report priced %d assets and balances=%v", rep.Totals.Priced, rep.Totals.Balances())
	}
	if err := Render(rep, "table", &strings.Builder{}); err != nil {
		t.Errorf("Render with no price book: %v", err)
	}
}
