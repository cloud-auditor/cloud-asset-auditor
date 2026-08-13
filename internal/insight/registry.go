package insight

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/filter"
)

// The registry and the runner.
//
// Two things matter here and both are about determinism. Insights register
// from init(), so registration order is Go's file order — an implementation
// detail that changes when a file is renamed. Registered() therefore sorts,
// and Run emits findings in that same order, so two runs over one inventory
// produce byte-identical output and two runs over different inventories
// produce a diff that is about the estate rather than about the linker.
//
// The second is that a run has three ways to produce nothing, and they mean
// completely different things: the insight looked and found nothing; the
// insight could not look (Requirements unmet); or the insight produced a
// finding this framework refused to publish (no Caveat). Collapsing those into
// one empty report would be the same mistake as rendering an unpriced asset as
// $0, so Run keeps all three and the renderers print all three.

var (
	registryMu sync.RWMutex
	registry   = map[string]Insight{}
)

// Register installs an insight. Re-registering an id, or registering one that
// fails Validate, panics — both are programmer errors at init time, not user
// errors at report time, and the alternative is a report that silently omits a
// section nobody notices is missing.
func Register(i Insight) {
	if err := Validate(i); err != nil {
		panic("insight: " + err.Error())
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[i.ID()]; exists {
		panic(fmt.Sprintf("insight: %q already registered", i.ID()))
	}
	registry[i.ID()] = i
}

// Lookup returns the insight registered under id.
func Lookup(id string) (Insight, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	i, ok := registry[id]
	return i, ok
}

// Registered returns every registered insight, ordered by family then id. That
// is the report's section order, so what you see in `--list` is what you get
// in the output.
func Registered() []Insight {
	registryMu.RLock()
	out := make([]Insight, 0, len(registry))
	for _, i := range registry {
		out = append(out, i)
	}
	registryMu.RUnlock()
	sortInsights(out)
	return out
}

func sortInsights(in []Insight) {
	sort.SliceStable(in, func(a, b int) bool {
		fa, fb := in[a].Family(), in[b].Family()
		if ra, rb := familyRank(fa), familyRank(fb); ra != rb {
			return ra < rb
		}
		if fa != fb {
			return fa < fb
		}
		return in[a].ID() < in[b].ID()
	})
}

// ----------------------------------------------------------------------
// the report
// ----------------------------------------------------------------------

// Report is the whole answer, and the shape the JSON renderer and any HTTP
// endpoint serialise.
//
// Disclaimer is a required top-level field rather than an optional one, for
// the same reason internal/cost makes it required: a consumer that drops it
// has to do so deliberately, and a shareable artifact travels without its
// author.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Disclaimer  string    `json:"disclaimer"`

	Scope Scope `json:"scope"`

	// Findings, ordered by family then id.
	Findings []Finding `json:"findings"`

	// Skipped are insights that did not run because their Requirements were
	// unmet. Present so an empty section is never mistaken for a clean one.
	Skipped []Skipped `json:"skipped,omitempty"`

	// Suppressed are findings this framework refused to publish. A non-empty
	// list is a bug in an insight, not a property of the inventory, and it is
	// rendered as loudly as a finding so it gets fixed.
	Suppressed []Suppressed `json:"suppressed,omitempty"`

	// Hidden counts findings dropped by MinSeverity. Reported so a filtered
	// report cannot be mistaken for a quiet one.
	Hidden int `json:"hidden,omitempty"`

	// Complete is false when the context was cancelled mid-run. A partial
	// report is a different document from a full one and must say so.
	Complete bool `json:"complete"`

	// Notes are run-level caveats derived from Scope — the conditions that
	// most often explain a report being empty or wrong. Rendered directly
	// under the disclaimer, not at the bottom: they qualify everything below
	// them, and a qualification printed after the thing it qualifies is a
	// footnote.
	Notes []string `json:"notes,omitempty"`
}

// Skipped is one insight that could not run, and why.
type Skipped struct {
	Insight string `json:"insight"`
	Title   string `json:"title,omitempty"`
	Family  Family `json:"family,omitempty"`
	Reason  string `json:"reason"`
}

// Suppressed is one finding the framework refused to publish.
type Suppressed struct {
	Insight string `json:"insight"`
	Finding string `json:"finding,omitempty"`
	Title   string `json:"title,omitempty"`
	Reason  string `json:"reason"`
}

// Severities counts findings by severity, for a header line or an exit-code
// decision.
func (r *Report) Severities() map[Severity]int {
	out := make(map[Severity]int, 4)
	for _, f := range r.Findings {
		out[f.Severity]++
	}
	return out
}

// MaxSeverity is the highest severity present, and false when there are no
// findings. The natural input to a `--exit-code` gate.
func (r *Report) MaxSeverity() (Severity, bool) {
	if len(r.Findings) == 0 {
		return "", false
	}
	worst := r.Findings[0].Severity
	for _, f := range r.Findings[1:] {
		if f.Severity.Rank() > worst.Rank() {
			worst = f.Severity
		}
	}
	return worst, true
}

// Families returns the findings grouped into sections, in report order. The
// renderers all group by family, so the grouping is computed once here rather
// than three times in render.go.
func (r *Report) Families() []Section {
	var out []Section
	for _, f := range r.Findings {
		if n := len(out); n > 0 && out[n-1].Family == f.Family {
			out[n-1].Findings = append(out[n-1].Findings, f)
			continue
		}
		out = append(out, Section{Family: f.Family, Findings: []Finding{f}})
	}
	return out
}

// Section is one family's findings, in report order.
type Section struct {
	Family   Family    `json:"family"`
	Findings []Finding `json:"findings"`
}

// ----------------------------------------------------------------------
// running
// ----------------------------------------------------------------------

// Options configures a run. A struct rather than functional options, matching
// the other report-shaped commands in this project (cost.Options,
// topology.ReachOptions).
type Options struct {
	// Insights is the explicit set to run. Nil means the registry, which is
	// what every real caller wants; tests pass their own.
	Insights []Insight

	// Only keeps insights whose id or family matches one of these
	// case-insensitive globs. Empty runs everything. Same selector semantics
	// as `auditor reach`, so "*.cert*" and "exposure" both work.
	Only []string

	// MinSeverity drops findings below this severity. Empty keeps everything.
	// What is dropped is counted in Report.Hidden — a filter that hides its
	// own effect turns a quiet report into a false one.
	MinSeverity Severity
}

// Run evaluates every selected insight over one Input.
//
// It returns a Report rather than (Report, error) because an insight cannot
// fail in a way the report should not describe: an unmet requirement is a
// Skipped entry, a malformed finding is a Suppressed entry, and a cancelled
// context sets Complete=false. All three are results, and all three are
// rendered.
func Run(ctx context.Context, in *Input, opts Options) *Report {
	insights := opts.Insights
	if insights == nil {
		insights = Registered()
	} else {
		// A caller-supplied set is sorted too: the ordering guarantee is a
		// property of the report, not of the registry.
		insights = append([]Insight(nil), insights...)
		sortInsights(insights)
	}

	rep := &Report{
		GeneratedAt: in.Now,
		Disclaimer:  Disclaimer,
		Scope:       in.Scope,
		Complete:    true,
	}

	for _, ins := range insights {
		if !selected(ins, opts.Only) {
			continue
		}
		if err := ctx.Err(); err != nil {
			// Stop rather than run the rest: the remaining insights would be
			// racing the same cancellation and a half-populated section is
			// worse than an absent one, as long as the absence is declared.
			rep.Complete = false
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"The run was cancelled (%v) before %q and everything after it. This report is "+
					"partial: a section that is empty here may simply never have run.", err, ins.ID()))
			break
		}
		if reason, ok := unmet(ins, in); !ok {
			rep.Skipped = append(rep.Skipped, Skipped{
				Insight: ins.ID(), Title: ins.Title(), Family: ins.Family(), Reason: reason,
			})
			continue
		}

		for _, f := range ins.Run(ctx, in) {
			// The family is stamped here, from the insight, so a finding and
			// the section it prints under cannot disagree.
			f.Family = ins.Family()
			if err := ValidateFinding(f); err != nil {
				rep.Suppressed = append(rep.Suppressed, Suppressed{
					Insight: ins.ID(), Finding: f.ID, Title: f.Title, Reason: err.Error(),
				})
				continue
			}
			if opts.MinSeverity != "" && f.Severity.Rank() < opts.MinSeverity.Rank() {
				rep.Hidden++
				continue
			}
			rep.Findings = append(rep.Findings, f)
		}
	}

	sortFindings(rep.Findings)
	rep.Notes = append(rep.Notes, scopeNotes(in.Scope, rep)...)
	return rep
}

// sortFindings orders the report: family, then id.
//
// Deliberately not by severity. Severity ordering makes the same finding move
// up and down the page as the estate changes, which destroys the one thing a
// stable id buys — that two reports can be diffed. Severity is marked on every
// line instead, and a reader scanning for "!!" finds it faster than they would
// scan a re-ordered page. Ties within an id break on severity then title so
// the order is total.
func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(a, b int) bool {
		x, y := fs[a], fs[b]
		if ra, rb := familyRank(x.Family), familyRank(y.Family); ra != rb {
			return ra < rb
		}
		if x.Family != y.Family {
			return x.Family < y.Family
		}
		if x.ID != y.ID {
			return x.ID < y.ID
		}
		if x.Severity != y.Severity {
			return x.Severity.Rank() > y.Severity.Rank()
		}
		return x.Title < y.Title
	})
}

// selected reports whether an insight matches the --only selectors, which are
// matched against both id and family so "exposure" and "exposure.*" and
// "*public*" all do something sensible.
func selected(i Insight, only []string) bool {
	if len(only) == 0 {
		return true
	}
	for _, sel := range only {
		if sel = strings.TrimSpace(sel); sel == "" {
			continue
		}
		if filter.Glob(sel, i.ID()) || filter.Glob(sel, string(i.Family())) {
			return true
		}
	}
	return false
}

// unmet checks an insight's declared Requirements against the input, returning
// the reason it cannot run.
//
// The reason is written for the person reading the report, not for the
// developer: it names the flag or the provider that would fix it, because
// "requires raw" is a sentence only somebody who has read this package
// understands.
func unmet(i Insight, in *Input) (string, bool) {
	r, ok := i.(Requiring)
	if !ok {
		return "", true
	}
	req := r.Requires()

	if req.Raw && !in.Scope.RawAvailable() {
		return "no asset in this audit carries a Raw payload — re-run with --include-raw", false
	}
	if req.Topology && in.Scope.Edges == 0 {
		return "the topology graph has no edges, so there is no structure to read", false
	}
	if req.Cost && !in.Priced() {
		return "cost estimation is off — re-run with --cost", false
	}
	var missing []string
	for _, p := range req.Providers {
		if !in.HasProvider(p) {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("no assets from %s in this audit", strings.Join(missing, ", ")), false
	}
	if len(req.Types) > 0 {
		for _, t := range req.Types {
			if in.Count(t) > 0 {
				return "", true
			}
		}
		return fmt.Sprintf("this audit contains none of %s", strings.Join(req.Types, ", ")), false
	}
	return "", true
}

// scopeNotes derives the run-level caveats — the handful of conditions that
// explain a surprising report better than any individual finding's Caveat can,
// because they are properties of the audit rather than of an asset.
//
// Assembled per run rather than kept as a constant so each one can name the
// actual number, which is what makes the difference between a note a reader
// acts on and boilerplate they skip.
func scopeNotes(s Scope, rep *Report) []string {
	var out []string

	switch len(s.Providers) {
	case 0:
		out = append(out, "This audit collected no assets at all, so every section below is empty "+
			"for that reason and for no other.")
	case 1:
		out = append(out, fmt.Sprintf("Only one provider (%s) contributed to this audit. Any insight "+
			"that joins across providers had nothing on the far side, and its silence says nothing "+
			"about the estate.", s.Providers[0]))
	}

	if s.Assets > 0 && !s.RawAvailable() {
		out = append(out, "No asset carries an Asset.Raw payload, so every insight that reads a "+
			"resource's own document — Kubernetes specs, policy bodies, certificate details — was "+
			"either skipped or is working from tags alone. Re-collect with --include-raw.")
	}
	if s.Edges == 0 && s.Assets > 0 {
		out = append(out, "The topology graph inferred no edges, so nothing here is derived from "+
			"structure. That is usually a raw-less snapshot or a single-provider audit rather than "+
			"an estate with no relationships in it.")
	}
	if !s.Priced {
		out = append(out, "Cost estimation is off, so findings that would carry money are counting "+
			"resources instead. Re-run with --cost for figures.")
	}
	if len(rep.Findings) == 0 && rep.Complete {
		// The empty-result caveat, and the most important one in the file. It
		// is the same argument the reachability renderer makes about an absent
		// path: an inferred, partial view being quiet is much weaker evidence
		// than a real audit being quiet.
		out = append(out, "No findings. That is not a clean bill of health: these insights read one "+
			"inventory snapshot and the graph inferred from it, they cannot see consumption, traffic "+
			"or intent, and they only ask the questions somebody has implemented.")
	}
	return out
}

// Disclaimer is the canonical statement of what an insight is and is not.
// Every surface that shows a finding renders it verbatim — the CLI report as a
// box above the first finding, the JSON as a required field. A second copy
// would drift, and a drifted disclaimer is worse than none because it reads as
// deliberate precision.
const Disclaimer = `These findings are derived from one inventory snapshot and the graph
inferred from it. Nothing here was measured, and none of this data was
collected for the purpose.

An inventory cannot see:
  - Consumption. There are no CPU, memory, request or bandwidth
    measurements in this data. Any statement about a resource being idle,
    oversized or unused is a statement about its declared configuration,
    not about its behaviour.
  - Traffic. An edge in the graph means a relationship was inferred
    between two records, not that a packet has ever crossed it. An absent
    edge means no relationship was inferred, which is weaker still.
  - Intent. A public bucket, an open policy and a wildcard DNS record are
    frequently deliberate. This tool cannot tell a mistake from a decision.
  - Anything outside its own scope. A provider that was not audited, a
    token without the scope to list a resource type, and a snapshot taken
    without --include-raw all produce the same silence as an estate that
    is genuinely clean.

Every finding carries a caveat naming what that finding cannot know. Read
it before acting: these are questions to go and answer, not verdicts.`

// DisclaimerShort is the single line that travels with a finding into places
// where a paragraph would corrupt the output — a CSV cell, a Slack message, a
// commit status.
const DisclaimerShort = "Derived from an inventory snapshot: no consumption, traffic or intent is " +
	"visible to this tool. Each finding's caveat says what it cannot know."
