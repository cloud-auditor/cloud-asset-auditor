package insight

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/cost"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

// Tests for the cost family. Two of them matter more than the rest:
// TestCostFindings_MeetTheContract, which is this file's local copy of the
// house rule (every finding names what it cannot know), and
// TestCostStoppedBilled_ZeroedByTheBookIsNotFlagged, which is the one place a
// cost finding could accuse a resource of costing money the price book has
// already said it does not.

// ----------------------------------------------------------------------
// fixtures
// ----------------------------------------------------------------------

// costInsights is every insight this file owns, passed to Run explicitly so
// these tests are unaffected by whatever else the registry gains.
func costInsights() []Insight {
	return []Insight{
		costRunRate{}, costConcentration{}, costByDimension{}, costShowback{},
		costOrphanedSpend{}, costStoppedBilled{}, costUnpriced{},
	}
}

// spendStub prices by asset id, which is what these tests need: two instances
// of one type where one is stopped-and-zeroed and the other is stopped-and-
// still-priced is the whole point of half of them.
type spendStub map[string]cost.Estimate

func (s spendStub) Estimate(a core.Asset) cost.Estimate {
	if e, ok := s[a.ID]; ok {
		return e
	}
	return cost.Estimate{Basis: cost.BasisUnknown, Detail: "no rule for type " + a.Type}
}

func usd(amount float64) cost.Estimate {
	return cost.Estimate{Monthly: amount, Priced: true, Currency: "USD", Basis: cost.BasisInferred}
}

func eur(amount float64) cost.Estimate {
	return cost.Estimate{Monthly: amount, Priced: true, Currency: "EUR", Basis: cost.BasisInferred}
}

// zeroedByStatus is what the estimator returns when a rule's zero_when_status
// fired: priced, zero, and carrying the note that explains the zero.
func zeroedByStatus() cost.Estimate {
	return cost.Estimate{
		Monthly: 0, Priced: true, Currency: "USD", Basis: cost.BasisInferred,
		Detail: "instance is stopped — OCPU and memory are not billed while it is; its boot volume still is.",
	}
}

func metered(detail string) cost.Estimate {
	return cost.Estimate{Basis: cost.BasisUnpriceable, Detail: detail}
}

func attributed() cost.Estimate {
	return cost.Estimate{Monthly: 3, Attributed: true, Currency: "USD", Basis: cost.BasisAssumed,
		Detail: "a share of node-1's cost"}
}

// spendAssets is a small estate shaped for the cost questions: two priced
// compartments, a stopped instance the book zeroes and a stopped one it does
// not, one asset in every unpriced bucket, and a compartment asset whose id is
// the value of everything else's compartment_id tag.
func spendAssets() []core.Asset {
	return []core.Asset{
		{
			Provider: "oci", AccountID: "ocid1.tenancy..t", Region: "us-ashburn-1",
			Type: "oci.compute.instance", ID: "ocid1.instance..web1", Name: "web-1", Status: "RUNNING",
			Tags: map[string]string{"compartment_id": "ocid1.compartment..prod", "environment": "prod"},
		},
		{
			Provider: "oci", AccountID: "ocid1.tenancy..t", Region: "us-ashburn-1",
			Type: "oci.compute.instance", ID: "ocid1.instance..web2", Name: "web-2", Status: "STOPPED",
			Tags: map[string]string{"compartment_id": "ocid1.compartment..prod", "environment": "prod"},
		},
		{
			Provider: "oci", AccountID: "ocid1.tenancy..t", Region: "us-ashburn-1",
			Type: "oci.block_volume", ID: "ocid1.volume..batch", Name: "batch-data", Status: "STOPPED",
			Tags: map[string]string{"compartment_id": "ocid1.compartment..prod", "environment": "prod"},
		},
		{
			Provider: "oci", AccountID: "ocid1.tenancy..t", Region: "eu-frankfurt-1",
			Type: "oci.load_balancer", ID: "ocid1.lb..edge", Name: "edge-lb", Status: "ACTIVE",
			Tags: map[string]string{"compartment_id": "ocid1.compartment..dev"},
		},
		{
			Provider: "oci", AccountID: "ocid1.tenancy..t",
			Type: "oci.compartment", ID: "ocid1.compartment..prod", Name: "production",
		},
		{
			Provider: "cloudflare", AccountID: "acct-cf",
			Type: "cloudflare.r2_bucket", ID: "r2-assets", Name: "assets",
		},
		{
			Provider: "kubernetes", AccountID: "prod-cluster",
			Type: "v1.Pod", ID: "uid-pod-9", Name: "api-7c9f",
			Tags: map[string]string{"namespace": "prod"},
		},
	}
}

func spendPrices() spendStub {
	return spendStub{
		"ocid1.instance..web1": usd(300),
		"ocid1.instance..web2": zeroedByStatus(),
		"ocid1.volume..batch":  usd(45),
		"ocid1.lb..edge":       usd(18),
		"r2-assets":            metered("billed per GB stored plus per-request"),
		"uid-pod-9":            attributed(),
	}
}

func spendInput(tb testing.TB, assets []core.Asset, est Estimator) *Input {
	tb.Helper()
	return NewInput(assets, WithNow(fixedNow), WithEstimator(est))
}

// findingByID pulls one finding out of a slice, failing when it is absent —
// every test below is about a specific finding's wording or arithmetic.
func findingByID(tb testing.TB, fs []Finding, id string) Finding {
	tb.Helper()
	for _, f := range fs {
		if f.ID == id {
			return f
		}
	}
	tb.Fatalf("no finding %q in %v", id, findingIDs(fs))
	return Finding{}
}

func findingIDs(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

func hasFinding(fs []Finding, id string) bool {
	for _, f := range fs {
		if f.ID == id {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------
// the house rule
// ----------------------------------------------------------------------

// TestCostFindings_MeetTheContract runs every cost insight over an inventory
// that makes all of them fire, and checks the contract the framework enforces —
// so a bare caveat fails here with the insight's name rather than in a shared
// test with a stack trace.
func TestCostFindings_MeetTheContract(t *testing.T) {
	assets := append(spendAssets(), orphanLB())
	prices := spendPrices()
	prices["ocid1.lb..orphan"] = usd(22)

	in := spendInput(t, append(assets, fixtureAssets()...), prices)
	rep := Run(context.Background(), in, Options{Insights: costInsights()})

	if len(rep.Suppressed) > 0 {
		t.Fatalf("framework refused %d finding(s): %+v", len(rep.Suppressed), rep.Suppressed)
	}
	if len(rep.Skipped) > 0 {
		t.Fatalf("insights skipped with cost and topology available: %+v", rep.Skipped)
	}

	want := []string{
		"cost.run-rate", "cost.concentration", "cost.by-provider", "cost.by-region",
		"cost.by-account", "cost.unattributed", "cost.by-tag", "cost.orphaned-spend",
		"cost.stopped-but-billed", "cost.unpriced",
	}
	for _, id := range want {
		if !hasFinding(rep.Findings, id) {
			t.Errorf("finding %q missing; got %v", id, findingIDs(rep.Findings))
		}
	}

	for _, f := range rep.Findings {
		if err := ValidateFinding(f); err != nil {
			t.Errorf("%s: %v", f.ID, err)
		}
		if f.Family != FamilyCost {
			t.Errorf("%s: family %q, want cost", f.ID, f.Family)
		}
		// A caveat that is one clause long is usually a caveat that names the
		// tool's general ignorance rather than this finding's. Not enforceable
		// in the framework, but checkable here.
		if len(strings.Fields(f.Caveat)) < 12 {
			t.Errorf("%s: caveat is too short to name anything specific: %q", f.ID, f.Caveat)
		}
		if f.Total != nil && f.Total.Currency == "" {
			t.Errorf("%s: total carries no currency", f.ID)
		}
	}
}

// TestCostFindings_NeverRenderZeroAsMoney is the rule internal/cost is built
// around, restated for anything this file prints: an aggregate with nothing in
// it must render as cost.NoMoney, never as a figure.
func TestCostFindings_NeverRenderZeroAsMoney(t *testing.T) {
	in := spendInput(t, spendAssets(), spendPrices())
	for _, ins := range costInsights() {
		for _, f := range ins.Run(context.Background(), in) {
			for _, r := range f.Rows {
				if r.Money != nil && r.Money.Total() == 0 && r.Money.String() != cost.NoMoney {
					t.Errorf("%s row %q renders an empty aggregate as %q", f.ID, r.Label, r.Money.String())
				}
			}
			if f.Total != nil && f.Total.Total() <= 0 {
				t.Errorf("%s carries a total of %s", f.ID, f.Total.String())
			}
		}
	}
}

// ----------------------------------------------------------------------
// run rate
// ----------------------------------------------------------------------

func TestCostRunRate_YearlyIsMonthlyTimesTwelve(t *testing.T) {
	in := spendInput(t, spendAssets(), spendPrices())
	f := findingByID(t, costRunRate{}.Run(context.Background(), in), "cost.run-rate")

	if len(f.Rows) != 2 {
		t.Fatalf("want a monthly and a yearly row, got %d: %+v", len(f.Rows), f.Rows)
	}
	monthly, year := f.Rows[0], f.Rows[1]
	if monthly.Money == nil || year.Money == nil {
		t.Fatal("both run-rate rows must carry money")
	}
	// 300 + 45 + 18, and the zeroed instance contributing exactly nothing.
	if got, want := monthly.Money.Estimated, 363.0; got != want {
		t.Errorf("monthly = %v, want %v", got, want)
	}
	if got, want := year.Money.Estimated, 363.0*12; got != want {
		t.Errorf("yearly = %v, want monthly x 12 = %v", got, want)
	}
	if !strings.Contains(year.Fact, "× 12") {
		t.Errorf("the yearly row must say it is a multiplication: %q", year.Fact)
	}

	// The deliverable: the projection is named as a projection, in the caveat,
	// not only in a row's detail column.
	for _, want := range []string{"multiplied by 12", "not a forecast", "not what you will be invoiced"} {
		if !strings.Contains(f.Caveat, want) {
			t.Errorf("caveat is missing %q: %s", want, f.Caveat)
		}
	}
	if !strings.Contains(f.Caveat, cost.DisclaimerShort) {
		t.Error("the run-rate caveat must carry internal/cost's short disclaimer verbatim")
	}
	if f.Count != 4 {
		t.Errorf("count = %d, want the 4 priced assets", f.Count)
	}

	// The Summary is the one line built to be quoted on its own, detached from
	// the Caveat that qualifies it. A bare "$X a year" reads as next year's
	// budget; the projection has to be self-limiting wherever it travels.
	if !strings.Contains(f.Summary, "a year at this rate") {
		t.Errorf("the yearly figure in the summary must name itself a rate, not a total: %q", f.Summary)
	}
}

func TestCostRunRate_CurrenciesAreNeverCombined(t *testing.T) {
	assets := append(spendAssets(), core.Asset{
		Provider: "netbird", AccountID: "nb-1", Type: "netbird.peer", ID: "peer-1", Name: "laptop",
	})
	prices := spendPrices()
	prices["peer-1"] = eur(10)

	in := spendInput(t, assets, prices)
	f := findingByID(t, costRunRate{}.Run(context.Background(), in), "cost.run-rate")

	if len(f.Rows) != 4 {
		t.Fatalf("want a monthly and a yearly row per currency, got %d", len(f.Rows))
	}
	seen := map[string]bool{}
	for _, r := range f.Rows {
		if r.Money == nil {
			t.Fatalf("row %q carries no money", r.Label)
		}
		seen[r.Money.Currency] = true
	}
	if !seen["USD"] || !seen["EUR"] {
		t.Errorf("both currencies must appear separately, got %v", seen)
	}
	if f.Total != nil {
		t.Errorf("a cross-currency finding must carry no combined total, got %s", f.Total.String())
	}
}

func TestCostRunRate_NothingPricedProducesNothing(t *testing.T) {
	in := spendInput(t, spendAssets(), spendStub{})
	if got := (costRunRate{}).Run(context.Background(), in); len(got) != 0 {
		t.Errorf("a run rate of nothing is not a run rate; got %v", findingIDs(got))
	}
}

// ----------------------------------------------------------------------
// concentration
// ----------------------------------------------------------------------

func TestCostConcentration_HeadlineIsHalfTheSpend(t *testing.T) {
	in := spendInput(t, spendAssets(), spendPrices())
	f := findingByID(t, costConcentration{}.Run(context.Background(), in), "cost.concentration")

	// 300 of 363 is past half on its own, so one resource is the answer.
	if f.Count != 1 {
		t.Errorf("count = %d, want 1 (the smallest prefix reaching half the total)", f.Count)
	}
	if f.Rows[0].Label != "web-1" {
		t.Errorf("rows must be ranked by figure, got %q first", f.Rows[0].Label)
	}
	if f.Rows[0].Value != "83%" {
		t.Errorf("share = %q, want 83%%", f.Rows[0].Value)
	}
	// Three priced-with-money assets is a small estate; a small prefix there is
	// arithmetic, not a shape worth flagging.
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %q, want info below the lopsided floor", f.Severity)
	}
	if f.Total == nil || f.Total.Estimated != 363 {
		t.Errorf("total must cover the listed rows, got %v", f.Total)
	}
}

func TestCostConcentration_LopsidedEstateIsNotable(t *testing.T) {
	var assets []core.Asset
	prices := spendStub{}
	// Twelve resources, one of which is most of the money.
	for i, amount := range []float64{500, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4} {
		id := "ocid1.volume..v" + string(rune('a'+i))
		assets = append(assets, core.Asset{
			Provider: "oci", AccountID: "t", Type: "oci.block_volume", ID: id, Name: id,
		})
		prices[id] = usd(amount)
	}
	in := spendInput(t, assets, prices)
	f := findingByID(t, costConcentration{}.Run(context.Background(), in), "cost.concentration")

	if f.Severity != SeverityNotable {
		t.Errorf("severity = %q, want notable when half the spend is in one of twelve", f.Severity)
	}
	if f.Count != 1 {
		t.Errorf("count = %d, want 1", f.Count)
	}
	if n := len(f.Rows); n != 10 {
		t.Errorf("rows = %d, want the top 10", n)
	}
	for _, want := range []string{"the largest single resource is", "the top 5 are", "the top 10 are"} {
		if !strings.Contains(f.Summary, want) {
			t.Errorf("summary is missing %q: %s", want, f.Summary)
		}
	}
}

func TestCostConcentration_RanksWithinOneCurrency(t *testing.T) {
	assets := append(spendAssets(), core.Asset{
		Provider: "netbird", AccountID: "nb-1", Type: "netbird.peer", ID: "peer-1", Name: "laptop",
	})
	prices := spendPrices()
	prices["peer-1"] = eur(900) // larger than anything in USD, and not comparable to it

	in := spendInput(t, assets, prices)
	f := findingByID(t, costConcentration{}.Run(context.Background(), in), "cost.concentration")

	for _, r := range f.Rows {
		if r.Money != nil && r.Money.Currency != "EUR" {
			t.Errorf("ranking mixed currencies: row %q is %s", r.Label, r.Money.Currency)
		}
	}
	if !strings.Contains(f.Caveat, "no exchange rate") {
		t.Errorf("the caveat must say the other currency was excluded: %s", f.Caveat)
	}
}

// ----------------------------------------------------------------------
// by dimension
// ----------------------------------------------------------------------

func TestCostByDimension_ProviderRegionAccount(t *testing.T) {
	got := costByDimension{}.Run(context.Background(), spendInput(t, spendAssets(), spendPrices()))

	provider := findingByID(t, got, "cost.by-provider")
	if provider.Rows[0].Label != "oci" {
		t.Errorf("groups must be biggest-first, got %q", provider.Rows[0].Label)
	}
	if provider.Rows[0].Value != "100%" {
		t.Errorf("oci share = %q, want 100%%", provider.Rows[0].Value)
	}
	// Cloudflare and Kubernetes carry assets but no money: their rows exist so
	// the asset counts reconcile, and they must not read as free.
	for _, r := range provider.Rows[1:] {
		if r.Money != nil && r.Money.String() != cost.NoMoney {
			t.Errorf("row %q renders unpriced assets as %s", r.Label, r.Money.String())
		}
		if !strings.Contains(r.Fact, "0 of") {
			t.Errorf("row %q must show its priced/total counts, got %q", r.Label, r.Fact)
		}
	}

	region := findingByID(t, got, "cost.by-region")
	var none bool
	for _, r := range region.Rows {
		if r.Label == "(none)" {
			none = true
		}
	}
	if !none {
		t.Errorf("region-less assets must group under (none), got %v", region.Rows)
	}

	findingByID(t, got, "cost.by-account")
}

// The summary counted groups and then asserted all of them "carry estimated
// spend", while the table directly beneath showed the unpriced ones at 0% with
// an empty money column. The summary is the half that gets quoted alone, so a
// group that priced to nothing must not be counted as spending.
func TestCostByDimension_SummaryCountsGroupsThatActuallyCarryMoney(t *testing.T) {
	got := costByDimension{}.Run(context.Background(), spendInput(t, spendAssets(), spendPrices()))
	f := findingByID(t, got, "cost.by-provider")

	// The fixture has exactly one provider with money (oci) among several.
	var withMoney int
	for _, r := range f.Rows {
		if r.Money != nil && r.Money.String() != cost.NoMoney {
			withMoney++
		}
	}
	if withMoney >= len(f.Rows) {
		t.Fatalf("fixture no longer has a zero-spend group, so this guards nothing (%d rows)", len(f.Rows))
	}

	// Pins both halves: the spenders and the population they came out of. The
	// old wording opened with the group count alone ("3 providers carry
	// estimated spend"), so asserting the "N of M" form is what rules it out —
	// a bare substring check for the old text would match inside the new one.
	want := fmt.Sprintf("%d of %d providers carry estimated spend", withMoney, len(f.Rows))
	if !strings.Contains(f.Summary, want) {
		t.Errorf("summary should read %q, got %q", want, f.Summary)
	}
}

func TestCostByDimension_MixedCurrenciesDropTheTotalAndTheShares(t *testing.T) {
	assets := append(spendAssets(), core.Asset{
		Provider: "netbird", AccountID: "nb-1", Type: "netbird.peer", ID: "peer-1", Name: "laptop",
	})
	prices := spendPrices()
	prices["peer-1"] = eur(10)

	got := costByDimension{}.Run(context.Background(), spendInput(t, assets, prices))
	f := findingByID(t, got, "cost.by-provider")

	if f.Total != nil {
		t.Errorf("total = %s; USD and EUR are never combined", f.Total.String())
	}
	for _, r := range f.Rows {
		if r.Value != "" {
			t.Errorf("row %q shows a share of %q; a percentage of a cross-currency sum is a percentage "+
				"of a number this tool refuses to compute", r.Label, r.Value)
		}
	}
	for _, want := range []string{"EUR and USD", "no exchange rate is applied"} {
		if !strings.Contains(f.Caveat, want) {
			t.Errorf("caveat is missing %q: %s", want, f.Caveat)
		}
	}
	// Each provider is single-currency, so its own row still carries a figure.
	if f.Rows[0].Money == nil {
		t.Error("a single-currency group must still show its money")
	}
}

func TestCostByDimension_OneGroupIsNoBreakdown(t *testing.T) {
	assets := []core.Asset{{
		Provider: "oci", AccountID: "t", Region: "us-ashburn-1",
		Type: "oci.load_balancer", ID: "lb", Name: "lb",
	}}
	got := costByDimension{}.Run(context.Background(), spendInput(t, assets, spendStub{"lb": usd(18)}))
	if len(got) != 0 {
		t.Errorf("a single-group rollup says nothing; got %v", findingIDs(got))
	}
}

// ----------------------------------------------------------------------
// showback
// ----------------------------------------------------------------------

func TestCostShowback_UnattributedRemainderIsExplicit(t *testing.T) {
	got := costShowback{}.Run(context.Background(), spendInput(t, spendAssets(), spendPrices()))

	un := findingByID(t, got, "cost.unattributed")
	// The load balancer carries compartment_id but no environment; nothing is
	// entirely untagged, so the headline count is zero and the per-key rows
	// carry the gaps.
	if un.Severity != SeverityInfo {
		t.Errorf("severity = %q, want info when every priced asset carries some allocation key", un.Severity)
	}
	var (
		envRow Row
		found  bool
	)
	for _, r := range un.Rows {
		if r.Label == "environment" {
			envRow, found = r, true
		}
	}
	if !found {
		t.Fatalf("no row for the environment key: %+v", un.Rows)
	}
	if envRow.Money == nil || envRow.Money.Estimated != 18 {
		t.Errorf("the environment gap must be the load balancer's 18, got %v", envRow.Money)
	}

	// compartment_id covers every priced asset, so it is the breakdown key, and
	// its OCID values are shown by the compartment's own name.
	by := findingByID(t, got, "cost.by-tag")
	if !strings.Contains(by.Title, "compartment_id") {
		t.Errorf("title = %q, want the best-covered key", by.Title)
	}
	if by.Rows[0].Label != "production" {
		t.Errorf("a tag value that is an asset id must render as that asset's name, got %q", by.Rows[0].Label)
	}
}

func TestCostShowback_UntaggedSpendIsAWarning(t *testing.T) {
	assets := spendAssets()
	// Strip every allocation tag off the load balancer: now some spend belongs
	// to nobody, which is the finding.
	for i := range assets {
		if assets[i].ID == "ocid1.lb..edge" {
			assets[i].Tags = map[string]string{}
		}
	}
	got := costShowback{}.Run(context.Background(), spendInput(t, assets, spendPrices()))
	un := findingByID(t, got, "cost.unattributed")

	if un.Severity != SeverityWarn {
		t.Errorf("severity = %q, want warn when priced spend carries no allocation tag", un.Severity)
	}
	if un.Count != 1 {
		t.Errorf("count = %d, want the 1 untagged priced asset", un.Count)
	}
	// The per-key rows overlap, so the money column cannot be totalled — the
	// unattributable figure is a row of its own instead.
	if un.Total != nil {
		t.Errorf("total = %v, want none: the per-key gaps overlap", un.Total)
	}
	gap := un.Rows[len(un.Rows)-1]
	if gap.Label != "(none of these keys)" || gap.Money == nil || gap.Money.Estimated != 18 {
		t.Errorf("last row = %+v, want the unattributable 18", gap)
	}
	if !strings.Contains(un.Summary, "attributable to nobody") {
		t.Errorf("summary must name the gap: %s", un.Summary)
	}

	by := findingByID(t, got, "cost.by-tag")
	last := by.Rows[len(by.Rows)-1]
	if !strings.HasPrefix(last.Label, "(no ") {
		t.Errorf("the untagged remainder must be an explicit last row, got %q", last.Label)
	}
}

func TestCostShowback_NoAllocationTagAtAll(t *testing.T) {
	assets := []core.Asset{{
		Provider: "oci", AccountID: "t", Type: "oci.load_balancer", ID: "lb", Name: "edge",
	}}
	got := costShowback{}.Run(context.Background(), spendInput(t, assets, spendStub{"lb": usd(18)}))
	if len(got) != 1 {
		t.Fatalf("want exactly the no-convention finding, got %v", findingIDs(got))
	}
	f := got[0]
	if f.ID != "cost.unattributed" || f.Severity != SeverityNotable {
		t.Errorf("got %s/%s, want cost.unattributed/notable", f.ID, f.Severity)
	}
	if len(f.Rows) != 0 {
		t.Errorf("there are no keys to list: %+v", f.Rows)
	}
	if !strings.Contains(f.Caveat, "fixed list") {
		t.Errorf("the caveat must admit the key list is closed: %s", f.Caveat)
	}
}

// ----------------------------------------------------------------------
// spend with no inferred relationship
// ----------------------------------------------------------------------

// orphanLB is a load balancer whose address matches nothing else in the
// inventory, so no resolver produces an edge for it.
func orphanLB() core.Asset {
	return core.Asset{
		Provider: "oci", AccountID: "ocid1.tenancy..t", Region: "us-ashburn-1",
		Type: "oci.load_balancer", ID: "ocid1.lb..orphan", Name: "forgotten-lb",
		Tags: map[string]string{"ip_addresses": "198.51.100.77"},
	}
}

func TestCostOrphanedSpend_DegreeZeroAndPriced(t *testing.T) {
	assets := append(fixtureAssets(), orphanLB())
	prices := spendStub{
		"ocid1.lb..orphan": usd(22),
		"ocid1.lb..lb1":    usd(18), // connected: priced, but not a finding
	}
	in := spendInput(t, assets, prices)
	if in.Scope.Edges == 0 {
		t.Fatal("fixture must produce edges for this insight to be meaningful")
	}

	f := findingByID(t, costOrphanedSpend{}.Run(context.Background(), in), "cost.orphaned-spend")
	if len(f.Rows) != 1 || f.Rows[0].Label != "forgotten-lb" {
		t.Fatalf("want only the unconnected load balancer, got %+v", f.Rows)
	}
	if f.Severity != SeverityNotable {
		t.Errorf("severity = %q; a degree-0 node is a question, not a defect", f.Severity)
	}
	// The caveat is the orphan report's own text, so a cost finding cannot end
	// up sounding more certain about a degree-0 node than the report that
	// computes them.
	orphans := in.Graph.Orphans("provider")
	for _, para := range orphans.Caveat {
		if !strings.Contains(f.Caveat, para) {
			t.Errorf("caveat drops internal/topology's paragraph: %q", para)
		}
	}
	if strings.Contains(strings.ToLower(f.Title), "unused") ||
		strings.Contains(strings.ToLower(f.Summary), "unused") {
		t.Error("neither the title nor the summary may call a degree-0 resource unused")
	}
}

func TestCostOrphanedSpend_UnmodelledTypesAreNotFindings(t *testing.T) {
	// A Cloudflare KV namespace has no resolver, so its degree-0 status is
	// structural. internal/topology puts it in Unmodelled and so must this.
	assets := append(fixtureAssets(), core.Asset{
		Provider: "cloudflare", AccountID: "acct-cf",
		Type: "cloudflare.kv_namespace", ID: "kv-1", Name: "sessions",
	})
	in := spendInput(t, assets, spendStub{"kv-1": usd(5)})
	if got := (costOrphanedSpend{}).Run(context.Background(), in); len(got) != 0 {
		t.Errorf("a type no resolver models is noise, not a finding: %+v", got)
	}
}

func TestCostOrphanedSpend_CollapsedGraphFindsNothing(t *testing.T) {
	// A collapsed graph replaces assets with per-group summary nodes, so every
	// real asset is absent from it. Reporting them all as degree 0 would turn
	// the whole estate into a finding.
	group := core.Asset{Provider: "oci", Type: "topology.group", ID: "oci"}
	other := core.Asset{Provider: "cloudflare", Type: "topology.group", ID: "cloudflare"}
	collapsed := &topology.Topology{
		Nodes: []core.Asset{group, other},
		Edges: []core.Edge{{From: other.AsRef(), To: group.AsRef(), Kind: core.EdgeKindDNS,
			Confidence: core.ConfidenceHeuristic}},
	}
	in := NewInput(append(fixtureAssets(), orphanLB()), WithNow(fixedNow),
		WithEstimator(spendStub{"ocid1.lb..orphan": usd(22)}), WithTopology(collapsed))

	if got := (costOrphanedSpend{}).Run(context.Background(), in); len(got) != 0 {
		t.Errorf("a collapsed graph cannot answer this question; got %+v", got)
	}
}

// ----------------------------------------------------------------------
// stopped, and still priced
// ----------------------------------------------------------------------

func TestCostStoppedBilled_ZeroedByTheBookIsNotFlagged(t *testing.T) {
	in := spendInput(t, spendAssets(), spendPrices())
	f := findingByID(t, costStoppedBilled{}.Run(context.Background(), in), "cost.stopped-but-billed")

	if len(f.Rows) != 1 || f.Rows[0].Label != "batch-data" {
		t.Fatalf("want only the stopped asset whose rule still prices it, got %+v", f.Rows)
	}
	if f.Rows[0].Value != "STOPPED" {
		t.Errorf("the row must show the provider's own status word, got %q", f.Rows[0].Value)
	}
	if !strings.Contains(f.Summary, "zeroes 1 other") {
		t.Errorf("the summary must account for the asset the book zeroed: %s", f.Summary)
	}
	if f.Severity != SeverityWarn {
		t.Errorf("severity = %q, want warn", f.Severity)
	}
	// The two readings this finding genuinely cannot separate.
	for _, want := range []string{"never told this status matters", "single point in time", "boot volume"} {
		if !strings.Contains(f.Caveat, want) {
			t.Errorf("caveat is missing %q: %s", want, f.Caveat)
		}
	}
}

func TestCostStoppedBilled_StatusVocabulary(t *testing.T) {
	for _, s := range []string{"STOPPED", "stopped", "Shut-Off", "shut off", "TERMINATED", "suspended", "disabled"} {
		if !stoppedStatus(s) {
			t.Errorf("%q must read as stopped", s)
		}
	}
	// The near-misses a substring rule would get wrong, plus the running states.
	for _, s := range []string{"", "RUNNING", "ACTIVE", "not_stopping", "reactivating", "provisioning"} {
		if stoppedStatus(s) {
			t.Errorf("%q must not read as stopped", s)
		}
	}
}

func TestCostStoppedBilled_NothingStoppedProducesNothing(t *testing.T) {
	assets := spendAssets()
	for i := range assets {
		assets[i].Status = "RUNNING"
	}
	if got := (costStoppedBilled{}).Run(context.Background(), spendInput(t, assets, spendPrices())); len(got) != 0 {
		t.Errorf("want no finding, got %v", findingIDs(got))
	}
}

// ----------------------------------------------------------------------
// unpriced share
// ----------------------------------------------------------------------

func TestCostUnpriced_BucketsAndDenominator(t *testing.T) {
	in := spendInput(t, spendAssets(), spendPrices())
	f := findingByID(t, costUnpriced{}.Run(context.Background(), in), "cost.unpriced")

	// 7 assets: 4 priced, 1 metered, 1 attributed, 1 unknown (the compartment).
	if f.Count != 3 {
		t.Errorf("count = %d, want 3 unpriced", f.Count)
	}
	for _, want := range []string{"1 unknown", "1 metered", "1 attributed", "not $0"} {
		if !strings.Contains(f.Summary, want) {
			t.Errorf("summary is missing %q: %s", want, f.Summary)
		}
	}
	if f.Severity != SeverityNotable {
		t.Errorf("severity = %q, want notable while a fixable book gap exists", f.Severity)
	}
	for _, r := range f.Rows {
		if r.Money != nil {
			t.Errorf("row %q carries money; nothing unpriced has any", r.Label)
		}
	}
}

func TestCostUnpriced_OnlyStructuralGapsAreInfo(t *testing.T) {
	assets := []core.Asset{
		{Provider: "oci", AccountID: "t", Type: "oci.load_balancer", ID: "lb", Name: "lb"},
		{Provider: "cloudflare", AccountID: "cf", Type: "cloudflare.r2_bucket", ID: "r2", Name: "r2"},
	}
	prices := spendStub{"lb": usd(18), "r2": metered("billed per request")}
	f := findingByID(t, costUnpriced{}.Run(context.Background(), spendInput(t, assets, prices)), "cost.unpriced")
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %q, want info when everything unpriced is permanently so", f.Severity)
	}
}

func TestCostUnpriced_EverythingPricedProducesNothing(t *testing.T) {
	assets := []core.Asset{{Provider: "oci", AccountID: "t", Type: "oci.load_balancer", ID: "lb", Name: "lb"}}
	if got := (costUnpriced{}).Run(context.Background(), spendInput(t, assets, spendStub{"lb": usd(18)})); len(got) != 0 {
		t.Errorf("want no finding, got %v", findingIDs(got))
	}
}

// ----------------------------------------------------------------------
// the money helpers
// ----------------------------------------------------------------------

// TestSpendBag_EmptyIsNeverZero is internal/cost's rule restated for the
// aggregates this file builds: nothing and free must not look alike.
func TestSpendBag_EmptyIsNeverZero(t *testing.T) {
	b := newSpendBag()
	if got := b.money(); got != cost.NoMoney {
		t.Errorf("empty bag renders %q, want %q", got, cost.NoMoney)
	}
	if _, ok := b.single(); ok {
		t.Error("an empty bag has no single currency")
	}
	if totalOf(b) != nil {
		t.Error("an empty bag is not a total")
	}
	// A bag holding only a status-zeroed asset is priced and still worth no
	// figure — the zero is a claim about one resource, not about an aggregate.
	b.add(Money{Currency: "USD"})
	if got := b.money(); got != cost.NoMoney {
		t.Errorf("zero-valued bag renders %q, want %q", got, cost.NoMoney)
	}
	if totalOf(b) != nil {
		t.Error("a bag summing to nothing must not become a total")
	}
	if got := currencyClause(b); got != "" {
		t.Errorf("one currency needs no clause, got %q", got)
	}
}

func TestFormatShare(t *testing.T) {
	for _, tc := range []struct {
		part, whole float64
		want        string
	}{
		{0, 100, "0%"},
		{0.4, 1000, "<1%"},
		{4.5, 100, "4.5%"},
		{83, 100, "83%"},
		{5, 0, ""},   // no denominator, no percentage
		{-1, 10, ""}, // nonsense in, nothing out
	} {
		if got := formatShare(tc.part, tc.whole); got != tc.want {
			t.Errorf("formatShare(%v, %v) = %q, want %q", tc.part, tc.whole, got, tc.want)
		}
	}
}

func TestNewSpendIndex_WithoutAnEstimator(t *testing.T) {
	// Requirements normally keep these insights from running with cost off;
	// a hand-assembled Input can still reach the index, and it must not invent
	// an estate of unpriced assets to report on.
	x := newSpendIndex(NewInput(spendAssets(), WithNow(fixedNow)))
	if x.assets() != 0 || x.priced() != 0 || x.unpriced() != 0 {
		t.Errorf("want an empty index with cost off, got %d assets", x.assets())
	}
}

func TestCommonestDetail(t *testing.T) {
	got := commonestDetail(map[string]int{"no rule for type x": 4, "unknown shape": 1})
	if got != "no rule for type x (+1 other reason)" {
		t.Errorf("got %q; the commonest reason must not hide that there was another", got)
	}
	if got := commonestDetail(nil); got != "no reason recorded" {
		t.Errorf("got %q, want a placeholder rather than a blank cell", got)
	}
}

// ----------------------------------------------------------------------
// framework contract
// ----------------------------------------------------------------------

func TestCostInsights_RequireCost(t *testing.T) {
	// The fixture chain is included so the graph has edges: with the topology
	// requirement met, cost is the only thing left to be missing, and every
	// skip reason names the flag that fixes it.
	in := NewInput(append(spendAssets(), fixtureAssets()...), WithNow(fixedNow)) // no estimator
	rep := Run(context.Background(), in, Options{Insights: costInsights()})

	if len(rep.Findings) != 0 {
		t.Errorf("cost insights must not run without an estimator, got %v", findingIDs(rep.Findings))
	}
	if len(rep.Skipped) != len(costInsights()) {
		t.Fatalf("want every cost insight skipped and said so, got %+v", rep.Skipped)
	}
	for _, s := range rep.Skipped {
		if !strings.Contains(s.Reason, "--cost") {
			t.Errorf("%s: reason must name the flag that fixes it, got %q", s.Insight, s.Reason)
		}
	}
}

func TestCostInsights_AreDeterministic(t *testing.T) {
	render := func() []byte {
		in := spendInput(t, append(spendAssets(), orphanLB()), spendPrices())
		rep := Run(context.Background(), in, Options{Insights: costInsights()})
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(rep); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	if a, b := render(), render(); !bytes.Equal(a, b) {
		t.Error("two runs over the same inventory produced different reports")
	}
}

func TestCostInsights_DoNotMutateTheInput(t *testing.T) {
	assets := append(spendAssets(), orphanLB())
	in := spendInput(t, assets, spendPrices())

	before, err := json.Marshal(in.Assets)
	if err != nil {
		t.Fatal(err)
	}
	Run(context.Background(), in, Options{Insights: costInsights()})
	after, err := json.Marshal(in.Assets)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("an insight mutated the shared asset list")
	}
}

// TestCostInsights_RenderWithTheirCaveats guards the thing the renderers are
// for: whatever the format, the caveat travels in the same output as the
// number.
func TestCostInsights_RenderWithTheirCaveats(t *testing.T) {
	in := spendInput(t, append(spendAssets(), orphanLB()), spendPrices())
	rep := Run(context.Background(), in, Options{Insights: costInsights()})

	// The human formats wrap prose, so both sides are compared with runs of
	// whitespace collapsed — the check is that the words are there beside the
	// number, not that the line breaks fall anywhere in particular.
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for _, format := range []string{"table", "markdown"} {
		var buf bytes.Buffer
		if err := Render(rep, format, &buf); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		out := flat(buf.String())
		for _, f := range rep.Findings {
			if !strings.Contains(out, flat(f.Caveat)) {
				t.Errorf("%s: finding %s lost its caveat", format, f.ID)
			}
		}
		if !strings.Contains(out, flat(cost.DisclaimerShort)) {
			t.Errorf("%s: money-bearing findings must carry internal/cost's short disclaimer", format)
		}
	}

	// JSON carries every caveat verbatim, punctuation and all.
	var buf bytes.Buffer
	if err := Render(rep, "json", &buf); err != nil {
		t.Fatal(err)
	}
	// Decoded field by field rather than into a Report: a Money marshals to
	// strings on purpose (so nobody can sum an estimate into an invoice) and
	// therefore does not round-trip back into floats.
	var round struct {
		Findings []struct {
			ID     string `json:"id"`
			Basis  string `json:"basis"`
			Caveat string `json:"caveat"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(buf.Bytes(), &round); err != nil {
		t.Fatal(err)
	}
	if len(round.Findings) != len(rep.Findings) {
		t.Fatalf("json round trip lost findings: %d of %d", len(round.Findings), len(rep.Findings))
	}
	for i, f := range rep.Findings {
		if round.Findings[i].Caveat != f.Caveat || round.Findings[i].Basis != f.Basis {
			t.Errorf("json: finding %s did not round-trip its basis/caveat", f.ID)
		}
	}
}
