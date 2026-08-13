// Package insight derives findings from an inventory that has already been
// collected. It is not a collector: every number it reports comes from assets
// the audit already holds plus the topology graph it already infers, so an
// insight run costs no API calls and can run against a snapshot from six
// months ago.
//
// # The rule this package lives or dies by
//
// An inventory is a list of what exists. It is not a record of what happens.
// It cannot see consumption, it cannot see traffic, and it cannot see intent —
// and those three absences are behind almost everything a reader would want an
// insight to tell them. So the discipline the rest of this project already
// keeps (a topology edge says exact or heuristic; an unpriced asset renders as
// "unknown", never as $0; an empty reachability result says "absence of a path
// is not proof of isolation"; an orphan is "a fact about the graph, not about
// the resource") is not decoration here. It is the feature.
//
// Concretely: every Finding MUST carry a Caveat naming what it cannot know,
// and the Caveat is rendered in the same visual unit as the number, never
// pooled into a footer. A finding that reads as an accusation when the
// evidence only supports a question is the failure this package exists to
// prevent — "this instance is underutilized" is a claim an inventory cannot
// make, while "this instance requests 8 CPU and the sampled window shows at
// most 0.2" is a fact plus an invitation to go and look.
//
// The requirement is enforced in three places, because one is not enough:
// Validate rejects a malformed insight at registration; Run refuses to publish
// a Finding whose Caveat is empty and reports the refusal as a framework
// defect rather than dropping it silently; and a test runs every registered
// insight over a synthetic inventory and fails on the first bare finding.
//
// # Shape
//
// An Insight is a small object with an id, a title, a family and a Run method.
// The runner builds one Input — assets indexed by type, provider and id, the
// topology's adjacency, memoized Raw parses, an optional cost estimator — and
// hands the same one to every insight, so eight insights do not each walk
// 50,000 assets. Findings are ordered by family then id, which makes two runs
// over the same inventory byte-identical, and two runs over *different*
// inventories diffable.
package insight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/cost"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

// ----------------------------------------------------------------------
// severity and family
// ----------------------------------------------------------------------

// Severity is how much attention a finding deserves — not how bad the
// underlying resource is. The distinction matters: this package can observe
// that a bucket is public, and that is worth a look regardless of whether the
// bucket is *meant* to be public, which it cannot see. Severity ranks the
// question, and the Caveat says why it is still a question.
type Severity string

const (
	// SeverityInfo describes the estate. Nothing to do; useful for orientation.
	SeverityInfo Severity = "info"
	// SeverityNotable is a pattern worth knowing about that may well be
	// deliberate — an unusual shape, not a defect.
	SeverityNotable Severity = "notable"
	// SeverityWarn is probably unintended and cheap to check.
	SeverityWarn Severity = "warn"
	// SeverityRisk is reserved for findings whose evidence is exact and whose
	// consequence is a security or availability incident. Reach for it rarely:
	// an inferred graph plus a heuristic join does not add up to a risk, and a
	// report where everything is a risk is a report nobody reads twice.
	SeverityRisk Severity = "risk"
)

var severityRank = map[Severity]int{
	SeverityInfo:    0,
	SeverityNotable: 1,
	SeverityWarn:    2,
	SeverityRisk:    3,
}

// Rank orders severities: info < notable < warn < risk.
func (s Severity) Rank() int { return severityRank[s] }

// Valid reports whether s is one of the four.
func (s Severity) Valid() bool {
	_, ok := severityRank[s]
	return ok
}

// Mark is the glyph that carries severity when colour is unavailable — which
// is always, here: nothing in this project's table output emits ANSI, because
// the output is pasted into tickets, diffed, and read over SSH. The glyph and
// the spelled-out word are both printed, so the signal survives a screenshot,
// a colourblind reader, and a `| grep`.
func (s Severity) Mark() string {
	switch s {
	case SeverityRisk:
		return "!!"
	case SeverityWarn:
		return "! "
	case SeverityNotable:
		return "* "
	default:
		return "  "
	}
}

// ParseSeverity normalizes a user-supplied severity name, for --min-severity
// and the like. "warning" is accepted as an alias for "warn" because
// internal/policy spells it that way and a user should not have to remember
// which command wanted which.
func ParseSeverity(s string) (Severity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return SeverityInfo, nil
	case "notable":
		return SeverityNotable, nil
	case "warn", "warning":
		return SeverityWarn, nil
	case "risk":
		return SeverityRisk, nil
	default:
		return "", fmt.Errorf("unknown severity %q (want info|notable|warn|risk)", s)
	}
}

// Family groups findings into the sections a reader scans. It is a free-form
// slug rather than a closed enum so a new insight can open a new section
// without editing this file — the constants below are the ones already in use,
// not the permitted set.
type Family string

// The families in use. familyOrder below decides which section leads.
const (
	FamilyExposure   Family = "exposure"   // what is reachable from outside
	FamilyNetwork    Family = "network"    // how traffic is arranged
	FamilyAccess     Family = "access"     // who or what may act
	FamilyResilience Family = "resilience" // what breaks when one thing does
	FamilyCost       Family = "cost"       // money-shaped questions
	FamilyHygiene    Family = "hygiene"    // leftovers, drift, missing metadata
)

// familyOrder puts the consequential sections first. A family that is not
// listed sorts alphabetically after every family that is, so an insight can
// invent a family without touching this table — the ordering degrades to
// alphabetical rather than to nondeterministic.
var familyOrder = map[Family]int{
	FamilyExposure:   0,
	FamilyAccess:     1,
	FamilyNetwork:    2,
	FamilyResilience: 3,
	FamilyCost:       4,
	FamilyHygiene:    5,
}

func familyRank(f Family) int {
	if r, ok := familyOrder[f]; ok {
		return r
	}
	return len(familyOrder)
}

// Title renders a family as a section heading. Slugs are lower-case words, so
// upper-casing is enough and a display-name table would be one more thing to
// keep in sync.
func (f Family) Title() string { return strings.ToUpper(strings.ReplaceAll(string(f), "-", " ")) }

// ----------------------------------------------------------------------
// findings
// ----------------------------------------------------------------------

// Money is one currency's share of a figure, split by whether it was measured
// from a billing API or estimated from list price.
//
// It is an alias, not a redefinition: internal/cost already refuses to render
// an empty aggregate as "0.00" (it renders cost.NoMoney), already keeps the
// measured and estimated halves apart, and already marshals to a string so no
// consumer can sum an estimate into an invoice. A parallel money type here
// would be a second vocabulary for one idea, and the second one always drifts.
type Money = cost.Money

// Finding is one derived answer. It is deliberately small: an id to key it, a
// sentence to read, a number to scan, the evidence it came from, the ignorance
// it is bounded by, and the rows to go and look at.
//
// Basis and Caveat are a pair and both are required. Basis says what was
// actually joined ("every asset the topology graph marks as an entry point,
// joined to the traffic-allow edges covering it"); Caveat says what that join
// cannot settle ("whether any traffic reaches these, and whether a firewall
// outside this inventory already blocks it"). A finding with a Basis and no
// Caveat states more than it knows; a finding with a Caveat and no Basis is
// unfalsifiable. Run refuses to publish either.
type Finding struct {
	// ID is a stable slug — "network.public-endpoints". Stable is the operative
	// word: it is the key a consumer suppresses, a CI gate allowlists, and two
	// reports are diffed on, so renaming one is a breaking change.
	ID string `json:"id"`

	// Family is stamped by the runner from the producing Insight, not set by
	// the insight itself, so a finding and its section can never disagree.
	Family Family `json:"family"`

	Title string `json:"title"`

	// Summary is one sentence carrying the number, written so it survives
	// being quoted on its own — it is what lands in a Slack paste. Phrase it as
	// an observation, not a verdict.
	Summary string `json:"summary"`

	Severity Severity `json:"severity"`

	// Count is the magnitude the Summary quotes. It is not required to equal
	// len(Rows): rows are frequently a sample, and a finding may count
	// namespaces while listing pods.
	Count int `json:"count"`

	// Basis is what the finding was derived from, concretely enough that a
	// reader can reproduce it — name the asset types, the tags, the edge kinds.
	// "Analysis of the inventory" is not a basis.
	Basis string `json:"basis"`

	// Caveat names what this finding cannot know. REQUIRED, and enforced.
	// Write the specific ignorance, not a general disclaimer: "an inventory
	// records that the port is open, not that anything connects to it" beats
	// "results may be inaccurate", which qualifies nothing and is ignored.
	Caveat string `json:"caveat"`

	// Rows are the tabular detail, each pointing at the assets it is about.
	// A long list should be a representative sample rather than everything;
	// the renderers cap what they print, and the JSON is where completeness
	// belongs.
	Rows []Row `json:"rows,omitempty"`

	// Total is set only by cost-bearing findings. A Total that silently omits
	// the assets that could not be priced is the classic way to understate a
	// number, so a finding carrying one must say in its Caveat how many assets
	// it covers out of how many it looked at.
	Total *Money `json:"total,omitempty"`
}

// Row is one line of a finding's detail table.
//
// Label is what the row is *about* and is always printed. Asset is set when
// that subject is a single collected asset, which is the common case; it is a
// pointer so an aggregate row ("namespace prod", "us-ashburn-1") can leave it
// out rather than fabricate a reference to an asset that does not exist.
type Row struct {
	Label string `json:"label"`

	// Asset identifies the row's subject so a consumer can join back to the
	// inventory. Nil on aggregate rows.
	Asset *core.AssetRef `json:"asset,omitempty"`

	// Fact is the specific observation for this row, in the row's own terms —
	// "requests 8 CPU, limit unset", "expires in 9 days". Keep it checkable.
	Fact string `json:"fact,omitempty"`

	// Value is a measure rendered right-aligned, so a column of them scans.
	// A string, not a number, so a row can say "unknown" where it must.
	Value string `json:"value,omitempty"`

	// Money is this row's cost contribution, when there is one.
	Money *Money `json:"money,omitempty"`

	// Related are the other assets implicated in this row — the zone a record
	// belongs to, the rule that permits the flow. They exist so a row does not
	// have to encode asset ids inside prose, which no consumer can parse.
	Related []core.AssetRef `json:"related,omitempty"`
}

// AssetRow builds the common row: a single asset, displayed by name, with one
// observation. Provided so four insights do not write four display-name
// helpers that disagree about what to do with an unnamed asset.
func AssetRow(a core.Asset, fact string) Row {
	ref := a.AsRef()
	return Row{Label: DisplayName(a), Asset: &ref, Fact: fact}
}

// DisplayName is the name to print for an asset, falling back to the id.
// Cloud resources are frequently unnamed, and a blank cell in a table reads as
// a bug in the tool rather than as an absent name upstream.
func DisplayName(a core.Asset) string {
	if a.Name != "" {
		return a.Name
	}
	return a.ID
}

// ----------------------------------------------------------------------
// validation — the enforcement half of the house rule
// ----------------------------------------------------------------------

// ErrNoCaveat is returned for a Finding with an empty (or placeholder) Caveat.
// A sentinel rather than a formatted string so tests and callers can assert on
// the one rule that is not negotiable.
var ErrNoCaveat = errors.New("finding has no caveat: every finding must name what it cannot know")

// slugRE is the id format: lower-case words joined by dots or dashes. Enforced
// because ids are a public key — they end up in CI allowlists and suppression
// files, where "Network.PublicEndpoints" and "network.public-endpoints" being
// two spellings of one thing is a support ticket.
var slugRE = regexp.MustCompile(`^[a-z0-9]+([.-][a-z0-9]+)*$`)

// placeholderCaveats are the strings that satisfy "non-empty" while saying
// nothing. This list cannot stop a determined bypass — anyone can write "it
// might be wrong" — but it catches the accidental one, which is the stub left
// in while the real sentence was still being thought about.
var placeholderCaveats = map[string]bool{
	"n/a": true, "na": true, "none": true, "-": true, "todo": true,
	"tbd": true, "unknown": true, "nothing": true,
}

// Validate checks an insight's declared shape. Called by Register, so a
// malformed insight fails at process start rather than at report time.
//
// It cannot check the thing that matters most — an insight's Findings carry a
// Caveat — because that is a property of a run, not of a declaration. That
// half is enforced by ValidateFinding at run time and by the test that runs
// every registered insight over a fixture.
func Validate(i Insight) error {
	if i == nil {
		return errors.New("nil insight")
	}
	id := i.ID()
	switch {
	case id == "":
		return errors.New("insight has no id")
	case !slugRE.MatchString(id):
		return fmt.Errorf("insight id %q is not a slug (want lower-case words joined by . or -)", id)
	}
	if strings.TrimSpace(i.Title()) == "" {
		return fmt.Errorf("insight %q has no title", id)
	}
	fam := string(i.Family())
	switch {
	case fam == "":
		return fmt.Errorf("insight %q has no family", id)
	case !slugRE.MatchString(fam):
		return fmt.Errorf("insight %q has family %q, which is not a slug", id, fam)
	}
	return nil
}

// ValidateFinding checks one published finding. Run calls it on everything an
// insight returns; nothing reaches a renderer without passing.
func ValidateFinding(f Finding) error {
	switch {
	case f.ID == "":
		return errors.New("finding has no id")
	case !slugRE.MatchString(f.ID):
		return fmt.Errorf("finding id %q is not a slug", f.ID)
	case strings.TrimSpace(f.Title) == "":
		return fmt.Errorf("finding %q has no title", f.ID)
	case strings.TrimSpace(f.Summary) == "":
		return fmt.Errorf("finding %q has no summary", f.ID)
	case strings.ContainsAny(f.Summary, "\n\r"):
		// One sentence, one line: the summary is laid out beside a count and
		// quoted on its own elsewhere. A multi-line summary breaks both.
		return fmt.Errorf("finding %q has a multi-line summary (it must be one sentence)", f.ID)
	case !f.Severity.Valid():
		return fmt.Errorf("finding %q has severity %q (want info|notable|warn|risk)", f.ID, f.Severity)
	case f.Count < 0:
		return fmt.Errorf("finding %q has a negative count (%d)", f.ID, f.Count)
	case strings.TrimSpace(f.Basis) == "":
		return fmt.Errorf("finding %q has no basis: name what it was derived from", f.ID)
	}
	if err := validateCaveat(f); err != nil {
		return err
	}
	for i, r := range f.Rows {
		if strings.TrimSpace(r.Label) == "" {
			return fmt.Errorf("finding %q row %d has no label", f.ID, i)
		}
		if r.Asset != nil && r.Asset.ID == "" {
			return fmt.Errorf("finding %q row %d references an asset with no id", f.ID, i)
		}
	}
	return nil
}

func validateCaveat(f Finding) error {
	c := strings.ToLower(strings.Trim(strings.TrimSpace(f.Caveat), ".!"))
	switch {
	case c == "":
		return fmt.Errorf("%w (finding %q)", ErrNoCaveat, f.ID)
	case placeholderCaveats[c]:
		return fmt.Errorf("%w: %q is a placeholder (finding %q)", ErrNoCaveat, f.Caveat, f.ID)
	case !strings.Contains(c, " "):
		// A single word is a label, not a statement. The caveat has to be a
		// sentence someone could disagree with.
		return fmt.Errorf("%w: %q is one word, not a statement (finding %q)", ErrNoCaveat, f.Caveat, f.ID)
	}
	return nil
}

// ----------------------------------------------------------------------
// the insight contract
// ----------------------------------------------------------------------

// Insight is one derived question, implemented once and registered from an
// init(). Run receives the shared Input and returns however many Findings it
// has evidence for — including none, which is the normal case and must not be
// dressed up as a clean bill of health.
//
// Run must be pure with respect to the Input: the same Input twice must give
// the same Findings in the same order, and it must not mutate anything it is
// handed (see Input's read-only contract). Determinism is not a nicety here —
// this project diffs its own output.
type Insight interface {
	ID() string
	Title() string
	Family() Family
	Run(ctx context.Context, in *Input) []Finding
}

// Requirements declares what an insight needs before its silence means
// anything. Unmet requirements make Run skip the insight and *say so*, rather
// than let it return zero findings that read as "nothing wrong here" — a
// certificate-expiry insight over an audit where Cloudflare never ran finds
// nothing, and reporting that as good news is the failure mode.
type Requirements struct {
	// Raw is set when the insight parses Asset.Raw. Met when any asset in the
	// input carries a payload (--include-raw).
	Raw bool
	// Topology is set when the insight reads the graph rather than the flat
	// asset list. Met when a graph was built and has at least one edge.
	Topology bool
	// Cost is set when the insight cannot answer without price estimates.
	Cost bool
	// Providers must all have contributed at least one asset.
	Providers []string
	// Types: at least one asset of at least one listed type must be present.
	// A list rather than a set of ANDs because the alternatives are usually
	// equivalent (v1.Ingress or an HTTPRoute; an OCI LB or a Kubernetes one).
	Types []string
}

// Requiring is the optional interface an insight implements to declare
// Requirements. It is optional, and type-asserted like the Configurable
// interfaces in internal/core, so an insight that always has what it needs
// stays a four-method object.
type Requiring interface {
	Requires() Requirements
}

// ----------------------------------------------------------------------
// input
// ----------------------------------------------------------------------

// Estimator prices one asset. An interface rather than *cost.Estimator so an
// insight can be tested against a stub, and so "cost is off" is a nil field
// rather than a special mode.
//
// Note that *cost.Estimator tolerates a nil receiver (it prices everything as
// unknown), so even a typed-nil stored here degrades to "no figures" instead
// of panicking. Priced() is still the right thing to consult before promising
// a reader any money.
type Estimator interface {
	Estimate(core.Asset) cost.Estimate
}

// Input is the shared working set, built once by NewInput and handed to every
// insight. Doing the indexing here is the difference between one pass over
// 50,000 assets and eight.
//
// # Read-only
//
// Every slice and map reachable from an Input is shared by every insight in
// the run. Do not sort, append to, or otherwise mutate what you are handed:
// ByType returns the index's own bucket, not a copy, because copying 30,000
// Pods per lookup would undo the point of the index. Sort a copy when you need
// an order — and note that Assets is already in a canonical order, so most of
// the time you do not.
type Input struct {
	// Assets is every collected asset, sorted by (provider, type, id).
	//
	// The sort is load-bearing rather than tidy: assets arrive in whatever
	// order providers finished in, which differs between two runs over an
	// unchanged estate. An insight that iterates this slice and appends rows
	// as it goes is therefore deterministic for free, and one that sorts by
	// its own key with sort.SliceStable keeps a deterministic tiebreak.
	Assets []core.Asset

	// Graph is the inferred topology. Never nil — NewInput substitutes an
	// empty graph — so an insight can read Graph.Edges without a guard, but
	// see Requirements.Topology before concluding anything from its silence.
	Graph *topology.Topology

	// Now is the clock for anything time-relative (expiry, age). Injected so
	// two runs of the same fixture produce the same report; tests set it.
	Now time.Time

	// Scope is what this run could see. It is the raw material for an honest
	// Caveat: an insight that finds nothing in Kubernetes should check whether
	// Kubernetes was even in the audit before saying so.
	Scope Scope

	// Cost is nil when cost estimation is off.
	Cost Estimator

	byType     map[string][]core.Asset
	byProvider map[string][]core.Asset
	byRef      map[string]core.Asset
	byBareID   map[string]core.Asset
	edgesFrom  map[string][]core.Edge
	edgesTo    map[string][]core.Edge

	// rawMu guards the memoized Raw parses. Insights run sequentially today;
	// the mutex costs nothing and means a future concurrent runner is not a
	// silent data race.
	rawMu sync.Mutex
	raw   map[string]map[string]any
}

// Scope records what the run had to work with. Every field here exists because
// its absence is a plausible explanation for a finding being empty, large, or
// wrong — which is exactly what a report-level note has to be able to say.
type Scope struct {
	Assets    int      `json:"assets"`
	Types     int      `json:"types"`
	Providers []string `json:"providers"`
	Edges     int      `json:"edges"`
	// RawAssets is how many assets carried an Asset.Raw payload. Zero means
	// every raw-reading insight and three topology resolvers were inert.
	RawAssets int `json:"raw_assets"`
	// Priced reports that a cost estimator was supplied.
	Priced bool `json:"priced"`
}

// RawAvailable reports whether any asset carried a payload.
func (s Scope) RawAvailable() bool { return s.RawAssets > 0 }

// InputOption configures an Input. Functional options because the optional
// halves (a prebuilt graph, an estimator, a fixed clock) are genuinely
// optional and a five-argument constructor would read as four zero values.
type InputOption func(*Input)

// WithTopology supplies a graph that has already been built — the CLI has one
// in hand for `topology` and `reach`, and building it twice over 50,000 assets
// is a visible pause. Omit it and NewInput builds one.
func WithTopology(t *topology.Topology) InputOption {
	return func(in *Input) { in.Graph = t }
}

// WithEstimator turns cost-bearing insights on. A nil estimator is ignored, so
// a caller can pass whatever it has without branching.
func WithEstimator(e Estimator) InputOption {
	return func(in *Input) {
		if e != nil {
			in.Cost = e
		}
	}
}

// WithNow fixes the clock.
func WithNow(t time.Time) InputOption {
	return func(in *Input) { in.Now = t }
}

// NewInput does the shared work once: it copies and canonically sorts the
// asset list, indexes it four ways, builds the graph if one was not supplied,
// and indexes the graph's adjacency.
//
// The asset slice is copied before sorting. Callers hand us the audit's own
// buffer, and reordering it under them would be a rude surprise for whatever
// renders next.
func NewInput(assets []core.Asset, opts ...InputOption) *Input {
	in := &Input{
		Assets:     make([]core.Asset, len(assets)),
		Now:        time.Now().UTC(),
		byType:     make(map[string][]core.Asset),
		byProvider: make(map[string][]core.Asset),
		byRef:      make(map[string]core.Asset, len(assets)),
		byBareID:   make(map[string]core.Asset, len(assets)),
		edgesFrom:  make(map[string][]core.Edge),
		edgesTo:    make(map[string][]core.Edge),
		raw:        make(map[string]map[string]any),
	}
	copy(in.Assets, assets)
	sort.SliceStable(in.Assets, func(i, j int) bool {
		a, b := in.Assets[i], in.Assets[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.ID < b.ID
	})

	for _, o := range opts {
		o(in)
	}

	providers := map[string]struct{}{}
	for _, a := range in.Assets {
		in.byType[a.Type] = append(in.byType[a.Type], a)
		in.byProvider[a.Provider] = append(in.byProvider[a.Provider], a)
		in.byRef[refKey(a.AsRef())] = a
		if a.ID != "" {
			in.byBareID[a.ID] = a
		}
		providers[a.Provider] = struct{}{}
		if len(a.Raw) > 0 {
			in.Scope.RawAssets++
		}
	}

	if in.Graph == nil {
		in.Graph = topology.Build(in.Assets)
	}
	for _, e := range in.Graph.Edges {
		in.edgesFrom[refKey(e.From)] = append(in.edgesFrom[refKey(e.From)], e)
		in.edgesTo[refKey(e.To)] = append(in.edgesTo[refKey(e.To)], e)
	}

	in.Scope.Assets = len(in.Assets)
	in.Scope.Types = len(in.byType)
	in.Scope.Edges = len(in.Graph.Edges)
	in.Scope.Providers = sortedKeys(providers)
	in.Scope.Priced = in.Cost != nil
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	return in
}

// refKey is the canonical key for an asset, matching the identity the rest of
// the project uses: (provider, id). Two providers colliding on an id would
// false-merge here, which is the same trade internal/topology already makes.
func refKey(r core.AssetRef) string { return r.Provider + "\x00" + r.ID }

// ByType returns every asset of one type, in the canonical order.
//
// The returned slice is the index's own — treat it as read-only (see Input).
func (in *Input) ByType(types ...string) []core.Asset {
	if len(types) == 1 {
		return in.byType[types[0]]
	}
	// Several types is a new slice by necessity; it is still cheaper than a
	// scan, and the concatenation stays in canonical order because the caller's
	// type list is a fixed literal at every call site.
	var out []core.Asset
	for _, t := range types {
		out = append(out, in.byType[t]...)
	}
	return out
}

// ByProvider returns every asset from one provider, read-only.
func (in *Input) ByProvider(provider string) []core.Asset { return in.byProvider[provider] }

// Types lists every asset type present, sorted.
func (in *Input) Types() []string { return sortedKeys(in.byType) }

// Count is how many assets of a type were collected.
func (in *Input) Count(typ string) int { return len(in.byType[typ]) }

// HasProvider reports whether a provider contributed any asset. The honest
// guard before concluding anything from an empty result.
func (in *Input) HasProvider(name string) bool { return len(in.byProvider[name]) > 0 }

// Asset resolves a reference — the (provider, id) identity used everywhere
// else in this project.
func (in *Input) Asset(ref core.AssetRef) (core.Asset, bool) {
	a, ok := in.byRef[refKey(ref)]
	return a, ok
}

// AssetByID resolves a bare id, for the joins where all you have is an
// identifier out of a tag — an OCID in vcn_id, a zone id on a ruleset.
//
// Ids are unique per provider, not globally, so a collision across providers
// resolves to whichever asset sorted last. That is the same trade
// internal/topology's index makes for the same reason: the identifiers this is
// used with (OCIDs, UUIDs, Cloudflare ids) do not collide in practice, and
// requiring a provider here would make every tag join carry a hard-coded
// provider name.
func (in *Input) AssetByID(id string) (core.Asset, bool) {
	a, ok := in.byBareID[id]
	return a, ok
}

// EdgesFrom returns the edges leaving an asset, read-only.
func (in *Input) EdgesFrom(ref core.AssetRef) []core.Edge { return in.edgesFrom[refKey(ref)] }

// EdgesTo returns the edges arriving at an asset, read-only.
func (in *Input) EdgesTo(ref core.AssetRef) []core.Edge { return in.edgesTo[refKey(ref)] }

// Degree is the number of edges incident to an asset in either direction.
// Zero means the graph relates this asset to nothing — which is a fact about
// the graph, not about the resource (see internal/topology's orphan report for
// the full version of that warning, and borrow its language in your Caveat).
func (in *Input) Degree(ref core.AssetRef) int {
	return len(in.edgesFrom[refKey(ref)]) + len(in.edgesTo[refKey(ref)])
}

// Raw parses an asset's payload, memoized per asset for the run. Several
// insights reading the same Pod spec is the expected case, and unmarshalling a
// 40 KB payload once per insight is the kind of cost that turns a fast report
// into a slow one.
//
// A payload that does not parse is indistinguishable from an absent one here:
// both mean "this insight gets no answer", and the alternative — an error
// return every caller ignores — buys nothing.
func (in *Input) Raw(a core.Asset) (map[string]any, bool) {
	if len(a.Raw) == 0 {
		return nil, false
	}
	key := refKey(a.AsRef())

	in.rawMu.Lock()
	defer in.rawMu.Unlock()
	if doc, ok := in.raw[key]; ok {
		return doc, doc != nil
	}
	var doc map[string]any
	if err := json.Unmarshal(a.Raw, &doc); err != nil {
		doc = nil
	}
	in.raw[key] = doc
	return doc, doc != nil
}

// RawPath walks a dotted path into an asset's payload —
// RawPath(pod, "spec.nodeName"). One implementation so four insights do not
// write four subtly different walkers.
//
// Numeric segments index into an array, so "spec.rules.0.host" works; a path
// through an array without an index does not fan out, because a helper that
// silently returned the first match would make "the Ingress has one rule" and
// "the Ingress has forty" look identical to the caller.
func (in *Input) RawPath(a core.Asset, path string) (any, bool) {
	doc, ok := in.Raw(a)
	if !ok {
		return nil, false
	}
	var cur any = doc
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	if cur == nil {
		return nil, false
	}
	return cur, true
}

// RawString reads a string out of a payload, the commonest RawPath shape.
func (in *Input) RawString(a core.Asset, path string) (string, bool) {
	v, ok := in.RawPath(a, path)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok && s != ""
}

// Priced reports whether cost estimates are available. Consult it before
// promising a reader money: with cost off, a "spend" finding has to say that
// it is counting resources rather than currency.
func (in *Input) Priced() bool { return in.Cost != nil }

// Estimate prices one asset, or reports false when cost is off.
func (in *Input) Estimate(a core.Asset) (cost.Estimate, bool) {
	if in.Cost == nil {
		return cost.Estimate{}, false
	}
	return in.Cost.Estimate(a), true
}

// Monthly is one asset's cost as a Money, and false when there is no
// defensible figure — cost is off, no rule matched, the billing model is
// metered, or the figure belongs to another asset (an attributed pod).
//
// It exists so that no insight has to re-derive the measured/estimated split
// itself. Getting that split wrong is precisely how a list-price guess gets
// laundered into an invoice: a Money's two halves render as "$412.90 + ~$8.50"
// and never as one number.
func (in *Input) Monthly(a core.Asset) (Money, bool) {
	est, ok := in.Estimate(a)
	if !ok || !est.Priced {
		return Money{}, false
	}
	m := Money{Currency: est.Currency}
	if est.Basis == cost.BasisMeasured {
		m.Measured = est.Monthly
	} else {
		m.Estimated = est.Monthly
	}
	return m, true
}

// SumMoney adds figures of one currency, and refuses when they disagree.
//
// The refusal is the point. No exchange rate is applied anywhere in this tool
// (see internal/cost), so a total mixing NetBird's EUR with everything else's
// USD is not a number — a finding that gets false here should drop its Total
// and say in the Caveat that the assets it counted are billed in two
// currencies.
func SumMoney(ms []Money) (Money, bool) {
	var out Money
	for _, m := range ms {
		if out.Currency == "" {
			out.Currency = m.Currency
		} else if m.Currency != "" && m.Currency != out.Currency {
			return Money{}, false
		}
		out.Measured += m.Measured
		out.Estimated += m.Estimated
	}
	return out, out.Currency != ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
