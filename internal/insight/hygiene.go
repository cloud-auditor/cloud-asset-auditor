package insight

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Hygiene insights: the questions about an estate's housekeeping that an
// inventory genuinely can answer, because they are questions about the records
// themselves rather than about what the resources do.
//
// The trap in this family is that every one of these is trivially convertible
// into an accusation, and every one of them has a mundane explanation that this
// tool cannot see. An untagged resource may be tagged in a system this tool does
// not read (OCI's defined tags are the concrete case — see ownershipInsight). A
// resource created four years ago may be the load-bearing one. A single-replica
// workload may be a leader-elected singleton for which a second replica would be
// a bug. A namespace with no NetworkPolicy may be on a cluster whose CNI does
// not enforce them, where the policy and its absence are equally inert.
//
// So each finding here states the observation, and its Caveat states the
// mundane explanation. Neither is optional.

// Kubernetes types this file reads.
const (
	hygienePodType           = "v1.Pod"
	hygieneNetworkPolicyType = "networking.k8s.io/v1.NetworkPolicy"
	hygieneDeploymentType    = "apps/v1.Deployment"
	hygieneStatefulSetType   = "apps/v1.StatefulSet"
)

func init() {
	Register(NewOwnershipInsight())
	Register(recentlyCreatedInsight{})
	Register(ageingInsight{})
	Register(expiryInsight{})
	Register(namespacePolicyInsight{})
	Register(singleReplicaInsight{})
}

// ----------------------------------------------------------------------
// ownership
// ----------------------------------------------------------------------

// DefaultOwnerTagKeys is the tag vocabulary ownershipInsight looks for when no
// other is supplied. Keys are matched after normalisation (lower-cased, with
// separators removed), so one entry covers cost-center, cost_center, CostCenter
// and costCenter; both spellings of centre are listed because normalisation
// cannot bridge them.
//
// The list is deliberately short. Every key added makes it easier for a
// resource to look owned, and the finding this feeds is about resources whose
// siblings carry a key they do not — so a vocabulary padded with near-synonyms
// silently converts real gaps into apparent compliance.
//
// Read-mostly: NewOwnershipInsight copies it, so a caller that replaces this
// slice after construction does not retune an insight that is already running.
var DefaultOwnerTagKeys = []string{
	"owner", "team", "env", "environment", "cost-center", "cost-centre",
	"service", "project", "contact", "business-unit",
}

// hygieneUnsetValues are the tag *values* that mean the key was filled in
// rather than answered. A resource tagged owner=TBD is not owned by TBD, and
// counting it as tagged is how a governance report reaches 100% while nobody
// can find who to page.
var hygieneUnsetValues = map[string]bool{
	"": true, "-": true, "n/a": true, "na": true, "none": true, "null": true,
	"nil": true, "unknown": true, "unassigned": true, "tbd": true, "todo": true,
	"changeme": true, "default": true,
}

// ownershipInsight reports resources missing an ownership tag, relative to the
// convention their own type already follows.
type ownershipInsight struct {
	keys []string // normalised
	raw  []string // as configured, for the Basis line
}

// NewOwnershipInsight builds the ownership insight over a tag vocabulary. No
// arguments uses DefaultOwnerTagKeys.
//
// Exported because "which tags mean ownership here" is an organisational fact
// this package cannot know, and the runner accepts an explicit insight set
// (Options.Insights) — so a caller with its own vocabulary can swap this one out
// without the registry needing a configuration channel of its own.
func NewOwnershipInsight(keys ...string) Insight {
	if len(keys) == 0 {
		keys = DefaultOwnerTagKeys
	}
	// raw keeps the caller's order, which is the order a reader recognises:
	// DefaultOwnerTagKeys leads with owner and team, and a summary that quoted
	// the alphabetically first three would lead with business-unit.
	var ins ownershipInsight
	seen := map[string]bool{}
	for _, k := range keys {
		n := hygieneNormalizeKey(k)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		ins.keys = append(ins.keys, n)
		ins.raw = append(ins.raw, k)
	}
	sort.Strings(ins.keys)
	return ins
}

func (ownershipInsight) ID() string     { return "hygiene.ownership" }
func (ownershipInsight) Title() string  { return "Resources with no ownership tag" }
func (ownershipInsight) Family() Family { return FamilyHygiene }

// hygieneNormalizeKey folds a tag key to lower-case alphanumerics so the four
// spellings every estate accumulates (owner, Owner, cost-center, cost_center)
// compare equal. Namespaced keys keep only their last segment, which is what
// makes app.kubernetes.io/part-of comparable to part-of.
func hygieneNormalizeKey(k string) string {
	if i := strings.LastIndex(k, "/"); i >= 0 {
		k = k[i+1:]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(k) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// owns reports which of the configured keys an asset actually carries with a
// meaningful value.
func (o ownershipInsight) owns(a core.Asset) []string {
	var found []string
	for k, v := range a.Tags {
		n := hygieneNormalizeKey(k)
		if !hygieneContains(o.keys, n) {
			continue
		}
		if hygieneUnsetValues[strings.ToLower(strings.TrimSpace(v))] {
			continue
		}
		found = append(found, n)
	}
	sort.Strings(found)
	return found
}

func (o ownershipInsight) Run(_ context.Context, in *Input) []Finding {
	if len(in.Assets) == 0 {
		return nil
	}

	// Grouped by (provider, type), because the convention is what makes the
	// finding: an estate that never tags DNS records has not failed to tag its
	// DNS records. A group where some assets carry an ownership key and some do
	// not is evidence the convention exists and these missed it — which is a
	// far stronger claim than "this resource has no owner tag", and it is the
	// only one an inventory can support.
	type group struct {
		provider, typ string
		total         int
		tagged        int
		keysUsed      map[string]int
	}
	var (
		groups = map[string]*group{}
		order  []string
		tagged int
		// The per-provider tally is only used by the no-convention finding, but
		// it is counted here so the whole thing stays one pass over what may be
		// 50,000 assets.
		perProv = map[string]int{}
	)
	for _, a := range in.Assets {
		key := a.Provider + "\x00" + a.Type
		g, ok := groups[key]
		if !ok {
			g = &group{provider: a.Provider, typ: a.Type, keysUsed: map[string]int{}}
			groups[key] = g
			order = append(order, key)
		}
		g.total++
		found := o.owns(a)
		if len(found) == 0 {
			perProv[a.Provider]++
			continue
		}
		g.tagged++
		tagged++
		for _, k := range found {
			g.keysUsed[k]++
		}
	}

	if tagged == 0 {
		return []Finding{o.noConvention(in, perProv)}
	}

	var (
		rows    []Row
		missing int
	)
	for _, k := range order {
		g := groups[k]
		gap := g.total - g.tagged
		if g.tagged == 0 || gap == 0 {
			continue
		}
		missing += gap
		rows = append(rows, Row{
			Label: g.typ,
			Value: fmt.Sprintf("%s of %s", formatInt(gap), formatInt(g.total)),
			Fact: fmt.Sprintf("%s: the other %s %s %s", g.provider, formatInt(g.tagged),
				pluralVerb(g.tagged, "uses", "use"), strings.Join(hygieneTopKeys(g.keysUsed), ", ")),
		})
	}
	if missing == 0 {
		return nil
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Label < rows[j].Label })

	return []Finding{{
		ID:    "hygiene.unowned-resources",
		Title: "Resources missing the ownership tag their siblings carry",
		Summary: fmt.Sprintf("%s asset%s %s none of %s, in %s resource type%s where other assets of "+
			"the same type do.", formatInt(missing), plural(missing),
			pluralVerb(missing, "carries", "carry"), o.vocabulary(), formatInt(len(rows)),
			plural(len(rows))),
		// Cheap to check and rarely deliberate — somebody set the convention and
		// these fell out of it. Not a risk: an untagged resource is a resource
		// nobody can attribute, not a resource that is exposed.
		Severity: SeverityWarn,
		Count:    missing,
		Basis: fmt.Sprintf("Asset.Tags keys normalised to lower-case alphanumerics (last path segment "+
			"only) and matched against %s, grouped by (provider, type); only groups where at least one "+
			"asset does carry a key are reported", strings.Join(o.raw, ", ")),
		Caveat: "A tag this tool cannot see is indistinguishable from a tag that does not exist, and " +
			"there is a known gap: the OCI collector reads freeform tags only, so a resource governed " +
			"by OCI defined tags — the namespaced kind, which is where a tenancy's cost-centre and " +
			"owner tags usually live — appears untagged here. Ownership is also frequently recorded " +
			"outside the cloud altogether, in a CMDB or a repository's CODEOWNERS, and nothing about " +
			"an untagged resource says it is unowned, unused or safe to delete.",
		Rows: rows,
	}}
}

// noConvention is the finding for an estate where the vocabulary matched
// nothing at all. It is separated from the main finding because it is a
// different claim: not "these fell out of the convention" but "no convention is
// visible", which could equally mean the vocabulary is wrong for this estate.
func (o ownershipInsight) noConvention(in *Input, perProv map[string]int) Finding {
	rows := make([]Row, 0, len(perProv))
	for _, p := range sortedKeys(perProv) {
		n := perProv[p]
		rows = append(rows, Row{
			Label: p,
			Value: fmt.Sprintf("%s asset%s", formatInt(n), plural(n)),
			Fact:  "no matching ownership tag on any of them",
		})
	}
	return Finding{
		ID:    "hygiene.no-ownership-convention",
		Title: "No ownership tag vocabulary is visible in this inventory",
		Summary: fmt.Sprintf("Not one of the %s collected assets carries any of %s as a tag.",
			formatInt(len(in.Assets)), o.vocabulary()),
		// Notable rather than warn: the likeliest explanation is that this
		// estate names ownership differently, which is a fact about the
		// vocabulary this insight was given, not a defect in the estate.
		Severity: SeverityNotable,
		Count:    len(in.Assets),
		Basis: fmt.Sprintf("every collected asset's Tags, normalised and matched against %s",
			strings.Join(o.raw, ", ")),
		Caveat: "This is at least as likely to be a statement about the tag names this insight was " +
			"given as about the estate — re-run it with your own vocabulary before concluding " +
			"anything. It also cannot see tags stored where this tool does not read: OCI defined " +
			"tags, Kubernetes annotations (only labels become Tags), or any external CMDB.",
		Rows: rows,
	}
}

// vocabulary is the key list as a summary reads it: three names and a count.
// The Basis carries all of them — a one-line summary that spends forty
// characters on a slash-separated list is a line nobody finishes.
func (o ownershipInsight) vocabulary() string {
	if len(o.raw) <= 3 {
		return strings.Join(o.raw, ", ")
	}
	return fmt.Sprintf("%s and %s more ownership tag%s",
		strings.Join(o.raw[:3], ", "), formatInt(len(o.raw)-3), plural(len(o.raw)-3))
}

// hygieneTopKeys names the keys a group actually uses, most common first, so
// the row says what the convention *is* rather than what it is not.
func hygieneTopKeys(counts map[string]int) []string {
	keys := sortedKeys(counts)
	sort.SliceStable(keys, func(i, j int) bool { return counts[keys[i]] > counts[keys[j]] })
	if len(keys) > 3 {
		keys = keys[:3]
	}
	return keys
}

func hygieneContains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------
// age
// ----------------------------------------------------------------------

// The two windows. 30 days is the shape of an observation period an auditor
// asks about ("what appeared since the last review"), with 7 called out inside
// it because that is the window where a change is still in somebody's head.
const (
	hygieneRecentWindow = 30 * 24 * time.Hour
	hygieneFreshWindow  = 7 * 24 * time.Hour
	hygieneAgeingWindow = 2 * 365 * 24 * time.Hour
)

// hygieneAges walks the inventory once and returns the assets whose CreatedAt
// falls inside a window, newest first, plus how many assets carried a creation
// time at all — which is the number every caveat in this section needs.
func hygieneAges(in *Input, keep func(age time.Duration) bool) (rows []core.Asset, ages []time.Duration, dated int) {
	type aged struct {
		a   core.Asset
		age time.Duration
	}
	var hits []aged
	for _, a := range in.Assets {
		if a.CreatedAt == nil || a.CreatedAt.IsZero() {
			continue
		}
		dated++
		age := in.Now.Sub(*a.CreatedAt)
		if !keep(age) {
			continue
		}
		hits = append(hits, aged{a, age})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].age != hits[j].age {
			return hits[i].age < hits[j].age
		}
		return hits[i].a.ID < hits[j].a.ID
	})
	for _, h := range hits {
		rows = append(rows, h.a)
		ages = append(ages, h.age)
	}
	return rows, ages, dated
}

// hygieneDateCoverage is the sentence every age-derived caveat carries. An
// asset with no CreatedAt is invisible to these findings, and the providers
// differ: Kubernetes and OCI stamp creation on everything, Cloudflare on some
// types, and a resource type that reports none is simply absent from the list
// rather than reported as old or new.
func hygieneDateCoverage(dated, total int) string {
	// The full-coverage case needs its own sentence. "Only 649 of 649 assets
	// report a creation time; the rest are absent" describes a remainder that
	// does not exist, and a caveat carrying an arithmetic absurdity is read as
	// boilerplate — which costs the sentences around it, the ones that matter.
	coverage := fmt.Sprintf("Only %s of %s collected assets report a creation time at all; the rest "+
		"are absent from this finding entirely, so it is a floor and not a census.",
		formatInt(dated), formatInt(total))
	if dated >= total {
		coverage = fmt.Sprintf("All %s collected assets report a creation time, so this one is not "+
			"narrowed by missing dates.", formatInt(total))
	}
	return coverage + " A creation time is also the resource's, not the configuration's — something " +
		"created three years ago and reconfigured this morning reads as old here, and something " +
		"recreated by a pipeline yesterday reads as new however long the workload has existed."
}

type recentlyCreatedInsight struct{}

func (recentlyCreatedInsight) ID() string     { return "hygiene.recently-created" }
func (recentlyCreatedInsight) Title() string  { return "Resources created during the last 30 days" }
func (recentlyCreatedInsight) Family() Family { return FamilyHygiene }

func (recentlyCreatedInsight) Run(_ context.Context, in *Input) []Finding {
	assets, ages, dated := hygieneAges(in, func(age time.Duration) bool {
		return age <= hygieneRecentWindow
	})
	if len(assets) == 0 {
		return nil
	}
	fresh := 0
	rows := make([]Row, 0, len(assets))
	for i, a := range assets {
		if ages[i] <= hygieneFreshWindow {
			fresh++
		}
		row := AssetRow(a, hygieneWhereFact(a))
		row.Value = hygieneWhen(-ages[i])
		rows = append(rows, row)
	}

	return []Finding{{
		ID:    "hygiene.recently-created",
		Title: "Resources created during the last 30 days",
		Summary: fmt.Sprintf("%s asset%s %s created in the 30 days before this audit, %s of them in "+
			"the last 7.", formatInt(len(assets)), plural(len(assets)),
			pluralVerb(len(assets), "was", "were"), formatInt(fresh)),
		// Orientation, not a defect. New resources are what a working estate
		// produces; the finding exists because "what appeared during the
		// observation window" is a question somebody is required to answer.
		Severity: SeverityInfo,
		Count:    len(assets),
		Basis: fmt.Sprintf("Asset.CreatedAt within %d days of the audit clock (%s), across every "+
			"provider that reports one", int(hygieneRecentWindow.Hours()/24),
			in.Now.UTC().Format(time.RFC3339)),
		Caveat: "Appearing here is not a defect and absence from here is not proof of age. " +
			hygieneDateCoverage(dated, len(in.Assets)) +
			" Nothing in an inventory records who created a resource or why, so this list answers " +
			"\"what is new\" and not \"what was unexpected\".",
		Rows: rows,
	}}
}

type ageingInsight struct{}

func (ageingInsight) ID() string     { return "hygiene.ageing-resources" }
func (ageingInsight) Title() string  { return "Resources running for more than two years" }
func (ageingInsight) Family() Family { return FamilyHygiene }

func (ageingInsight) Run(_ context.Context, in *Input) []Finding {
	assets, ages, dated := hygieneAges(in, func(age time.Duration) bool {
		return age >= hygieneAgeingWindow
	})
	if len(assets) == 0 {
		return nil
	}
	// Oldest first: hygieneAges sorts youngest-first, which is right for the
	// recent finding and backwards for this one.
	rows := make([]Row, 0, len(assets))
	for i := len(assets) - 1; i >= 0; i-- {
		row := AssetRow(assets[i], hygieneWhereFact(assets[i]))
		row.Value = hygieneWhen(-ages[i])
		rows = append(rows, row)
	}
	oldest := ages[len(ages)-1]

	return []Finding{{
		ID:    "hygiene.ageing-resources",
		Title: "Resources running for more than two years",
		Summary: fmt.Sprintf("%s asset%s created more than two years ago %s still present; "+
			"the oldest dates to %s.", formatInt(len(assets)), plural(len(assets)),
			pluralVerb(len(assets), "is", "are"), hygieneWhen(-oldest)),
		// Worth knowing, frequently deliberate. Long-lived infrastructure is
		// usually the infrastructure that works.
		Severity: SeverityNotable,
		Count:    len(assets),
		Basis: fmt.Sprintf("Asset.CreatedAt more than %d days before the audit clock (%s)",
			int(hygieneAgeingWindow.Hours()/24), in.Now.UTC().Format(time.RFC3339)),
		Caveat: "Age is not disuse and it is not risk: the oldest resource in an estate is often the " +
			"load-bearing one, and a long uptime is evidence something works. What an inventory cannot " +
			"see is the thing that would make age matter — whether the resource is still reached, " +
			"whether its image or engine version is still supported, and whether anyone still knows " +
			"what it is for. " + hygieneDateCoverage(dated, len(in.Assets)),
		Rows: rows,
	}}
}

// hygieneWhereFact places an asset without repeating its name: type, and region
// or account when there is one.
func hygieneWhereFact(a core.Asset) string {
	parts := []string{a.Type}
	switch {
	case a.Region != "":
		parts = append(parts, a.Region)
	case a.AccountID != "":
		parts = append(parts, a.AccountID)
	}
	return strings.Join(parts, " · ")
}

// ----------------------------------------------------------------------
// expiry
// ----------------------------------------------------------------------

// hygieneExpiryKeys are the normalised tag keys that carry an expiry this tool
// can read without --include-raw: Cloudflare certificates stamp expires_on,
// Tailscale keys and devices stamp expires, NetBird setup keys stamp expires.
//
// Tags rather than Raw on purpose — the same reasoning that puts NetBird peer
// addresses in tags so the topology join survives a plain snapshot. An expiry
// finding that only worked under --include-raw would be absent from most runs.
var hygieneExpiryKeys = []string{
	"expireson", "expires", "expiry", "expiresat", "expiration",
	"expirationdate", "notafter", "validuntil",
}

// hygieneExpiryHorizon is how far ahead is worth reporting. A quarter is too
// far to act on and a week is too late to renew a certificate through a change
// process.
const hygieneExpiryHorizon = 30 * 24 * time.Hour

// hygieneTimeFormats are the layouts seen on these tags. RFC3339 covers every
// current provider; the date-only and space-separated forms are here because a
// tag is a string and a provider changing its serialisation should degrade to
// "not parsed" rather than to a wrong year.
var hygieneTimeFormats = []string{
	time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02",
}

type expiryInsight struct{}

func (expiryInsight) ID() string     { return "hygiene.expiry" }
func (expiryInsight) Title() string  { return "Credentials and certificates with a visible expiry" }
func (expiryInsight) Family() Family { return FamilyHygiene }

type hygieneExpiring struct {
	asset core.Asset
	at    time.Time
	key   string
}

func (expiryInsight) Run(_ context.Context, in *Input) []Finding {
	var (
		expired  []hygieneExpiring
		soon     []hygieneExpiring
		visible  int
		unparsed int
	)
	for _, a := range in.Assets {
		key, value, ok := hygieneExpiryTag(a)
		if !ok {
			continue
		}
		// A Tailscale node with key expiry disabled still reports the
		// timestamp its key would have had. Reading that as an expiry
		// manufactures an expired credential for every long-lived server in
		// the tailnet — the loudest possible false positive.
		if strings.EqualFold(a.Tags["key_expiry_disabled"], "true") {
			continue
		}
		at, ok := hygieneParseTime(value)
		if !ok {
			unparsed++
			continue
		}
		visible++
		e := hygieneExpiring{asset: a, at: at, key: key}
		switch d := at.Sub(in.Now); {
		case d < 0:
			expired = append(expired, e)
		case d <= hygieneExpiryHorizon:
			soon = append(soon, e)
		}
	}
	if visible == 0 {
		return nil
	}

	coverage := fmt.Sprintf("Only the %s assets whose expiry is published as a tag are considered — "+
		"Cloudflare certificates, Tailscale keys and devices, NetBird setup keys. An expiry that lives "+
		"inside a payload (a Kubernetes kubernetes.io/tls Secret, an OCI vault key, a certificate pack's "+
		"member certificates) is not read here at all, so absence from these lists is not evidence that "+
		"nothing else expires soon.", formatInt(visible))
	if unparsed > 0 {
		coverage += fmt.Sprintf(" A further %s asset%s carried an expiry tag this tool could not parse "+
			"and was skipped rather than guessed at.", formatInt(unparsed), plural(unparsed))
	}
	const basis = "the Asset.Tags keys expires_on, expires, expiry, expires_at, expiration, " +
		"not_after and valid_until (matched case- and separator-insensitively), parsed as RFC3339; " +
		"assets tagged key_expiry_disabled=true are excluded because their timestamp is inert"

	var out []Finding
	if len(soon) > 0 {
		out = append(out, Finding{
			ID:    "hygiene.expiring-credentials",
			Title: "Credentials and certificates expiring within 30 days",
			Summary: fmt.Sprintf("%s of %s assets with a readable expiry date %s it within 30 days.",
				formatInt(len(soon)), formatInt(visible), pluralVerb(len(soon), "reaches", "reach")),
			// Cheap to check, and the consequence of missing it is an outage
			// rather than a compromise — a warning, not a risk.
			Severity: SeverityWarn,
			Count:    len(soon),
			Basis:    basis,
			Caveat: "Many of these renew themselves — Cloudflare universal and advanced certificates " +
				"are reissued automatically, and an auth key close to expiry may be one nobody intends " +
				"to keep. This tool sees the date, not the renewal pipeline behind it, and not whether " +
				"anything still depends on the credential. " + coverage,
			Rows: hygieneExpiryRows(soon, in.Now),
		})
	}
	if len(expired) > 0 {
		out = append(out, Finding{
			ID:    "hygiene.expired-credentials",
			Title: "Credentials and certificates past their expiry, still listed",
			Summary: fmt.Sprintf("%s of %s assets with a readable expiry date %s already past it and "+
				"still listed in the inventory.", formatInt(len(expired)), formatInt(visible),
				pluralVerb(len(expired), "is", "are")),
			Severity: SeverityWarn,
			Count:    len(expired),
			Basis:    basis,
			Caveat: "An expired credential that is still listed is usually inert — the provider stops " +
				"honouring it and the record lingers — so this is more often a cleanup item than an " +
				"incident. It is not proof that anything broke: a replacement may already be in place, " +
				"and this tool cannot tell which certificate a listener is actually serving. " + coverage,
			Rows: hygieneExpiryRows(expired, in.Now),
		})
	}
	return out
}

// hygieneExpiryRows renders soonest-first, which for the expired list means
// longest-expired last — the same reading order in both: act on the top row.
func hygieneExpiryRows(es []hygieneExpiring, now time.Time) []Row {
	sort.SliceStable(es, func(i, j int) bool {
		if !es[i].at.Equal(es[j].at) {
			return es[i].at.Before(es[j].at)
		}
		return es[i].asset.ID < es[j].asset.ID
	})
	rows := make([]Row, 0, len(es))
	for _, e := range es {
		row := AssetRow(e.asset, e.asset.Type)
		row.Value = hygieneWhen(e.at.Sub(now))
		rows = append(rows, row)
	}
	return rows
}

// hygieneExpiryTag finds an asset's expiry tag, returning the key as written so
// the reader can go and look at the same field.
func hygieneExpiryTag(a core.Asset) (key, value string, ok bool) {
	// Tags is a map, so a resource carrying two expiry-shaped keys would
	// otherwise resolve differently between runs. Sorting costs nothing at this
	// size and buys a byte-identical report.
	for _, k := range sortedKeys(a.Tags) {
		if !hygieneContains(hygieneExpiryKeys, hygieneNormalizeKey(k)) {
			continue
		}
		if v := strings.TrimSpace(a.Tags[k]); v != "" {
			return k, v, true
		}
	}
	return "", "", false
}

// hygieneParseTime parses an expiry tag. The zero time is rejected rather than
// treated as 1 January year 1: providers use it to mean "no expiry", and
// reporting it as forty thousand years overdue would be a comedy.
func hygieneParseTime(v string) (time.Time, bool) {
	for _, layout := range hygieneTimeFormats {
		t, err := time.Parse(layout, v)
		if err != nil {
			continue
		}
		if t.IsZero() || t.Year() <= 1 {
			return time.Time{}, false
		}
		return t.UTC(), true
	}
	return time.Time{}, false
}

// ----------------------------------------------------------------------
// namespaces with no NetworkPolicy
// ----------------------------------------------------------------------

type namespacePolicyInsight struct{}

func (namespacePolicyInsight) ID() string { return "hygiene.namespaces-without-network-policy" }

func (namespacePolicyInsight) Title() string {
	return "Namespaces running pods with no NetworkPolicy"
}

func (namespacePolicyInsight) Family() Family { return FamilyHygiene }

// Pods are enough; this reads the presence of NetworkPolicy assets and the
// traffic edges the graph already derived, never a policy spec, so it needs no
// Raw of its own.
func (namespacePolicyInsight) Requires() Requirements {
	return Requirements{Types: []string{hygienePodType}}
}

func (namespacePolicyInsight) Run(_ context.Context, in *Input) []Finding {
	// Reuse rather than re-parse: internal/topology's kubeNetworkPolicyFlow has
	// already read every NetworkPolicy body and turned it into traffic edges. A
	// pod on either end of one is covered by a policy statement this run could
	// actually read, whatever namespace that policy lives in — which is a
	// stronger test than counting policy objects, and it costs one pass over
	// the edges instead of a second parse of every spec.
	covered := map[string]bool{}
	for _, e := range in.Graph.Edges {
		switch e.Kind {
		case core.EdgeKindTrafficAllow, core.EdgeKindTrafficDeny:
			covered[refKey(e.From)] = true
			covered[refKey(e.To)] = true
		}
	}

	policyNS := map[string]int{}
	for _, p := range in.ByType(hygieneNetworkPolicyType) {
		policyNS[p.AccountID+"\x00"+utilNamespace(p)]++
	}

	type nsGroup struct {
		label   string
		pods    int
		covered bool
	}
	var (
		groups = map[string]*nsGroup{}
		order  []string
	)
	for _, p := range in.ByType(hygienePodType) {
		if !utilHoldsCapacity(p) {
			// Succeeded and Failed pods are the record of something that ran.
			// A namespace whose only pods are finished CronJob runs is not a
			// workload the absence of a policy exposes.
			continue
		}
		key := p.AccountID + "\x00" + utilNamespace(p)
		g, ok := groups[key]
		if !ok {
			g = &nsGroup{label: utilClusterName(p.AccountID) + "/" + utilNamespace(p)}
			groups[key] = g
			order = append(order, key)
		}
		g.pods++
		if covered[refKey(p.AsRef())] {
			g.covered = true
		}
	}

	var (
		rows     []Row
		open     int
		withPols int
	)
	sort.Strings(order)
	for _, k := range order {
		g := groups[k]
		if policyNS[k] > 0 {
			withPols++
			continue
		}
		if g.covered {
			// A policy elsewhere — another namespace's NetworkPolicy, a
			// Tailscale ACL, a NetBird rule — already names these pods.
			continue
		}
		open++
		rows = append(rows, Row{
			Label: g.label,
			Value: fmt.Sprintf("%s pod%s", formatInt(g.pods), plural(g.pods)),
			Fact:  "no NetworkPolicy collected here",
		})
	}
	if open == 0 {
		return nil
	}

	totalPolicies := in.Count(hygieneNetworkPolicyType)
	caveat := "A NetworkPolicy only does anything if the cluster's CNI enforces one, and an inventory " +
		"cannot see which plugin is installed or whether it is in enforcing mode — on a cluster " +
		"without an enforcing plugin the presence and the absence of a policy are equally inert. " +
		"Nor does the converse hold: a namespace excluded from this list because it has a policy may " +
		"have one that allows everything. Default-allow east-west traffic is also the Kubernetes " +
		"default and is a deliberate choice in plenty of clusters."
	if totalPolicies == 0 {
		// Ambiguity worth naming: zero policies anywhere is what an unpoliced
		// cluster looks like and also what an RBAC gap looks like, and the two
		// call for completely different responses.
		caveat += " No NetworkPolicy object was collected anywhere in this audit, which is what a " +
			"cluster with no policy regime looks like and equally what a service account that cannot " +
			"list networkpolicies looks like — check the collection errors before reading this as the " +
			"former."
	}

	return []Finding{{
		ID:    "hygiene.namespaces-without-network-policy",
		Title: "Namespaces running pods with no NetworkPolicy",
		Summary: fmt.Sprintf("%s namespace%s with running pods %s no NetworkPolicy and no policy "+
			"edge in the inferred graph, against %s that do.", formatInt(open), plural(open),
			pluralVerb(open, "has", "have"), formatInt(withPols)),
		// Notable, not warn. This is the platform's default state rather than a
		// mistake somebody made, and the cluster may not enforce policy at all.
		Severity: SeverityNotable,
		Count:    open,
		Basis: fmt.Sprintf("namespaces holding at least one %s outside a terminal phase, minus those "+
			"with a %s asset, minus those with a pod on either end of a traffic-allow or traffic-deny "+
			"edge in the inferred topology", hygienePodType, hygieneNetworkPolicyType),
		Caveat: caveat,
		Rows:   rows,
	}}
}

// ----------------------------------------------------------------------
// single-replica workloads
// ----------------------------------------------------------------------

type singleReplicaInsight struct{}

func (singleReplicaInsight) ID() string     { return "hygiene.single-replica-workloads" }
func (singleReplicaInsight) Title() string  { return "Workloads declaring a single replica" }
func (singleReplicaInsight) Family() Family { return FamilyHygiene }

func (singleReplicaInsight) Requires() Requirements {
	return Requirements{Raw: true, Types: []string{hygieneDeploymentType, hygieneStatefulSetType}}
}

func (singleReplicaInsight) Run(_ context.Context, in *Input) []Finding {
	var (
		rows       []Row
		scanned    int
		unreadable int
	)
	for _, w := range in.ByType(hygieneDeploymentType, hygieneStatefulSetType) {
		v, ok := in.RawPath(w, "spec.replicas")
		if !ok {
			// The API server defaults spec.replicas, so an unreadable one means
			// the payload is absent or truncated — not that it is 1.
			unreadable++
			continue
		}
		n, ok := v.(float64)
		if !ok {
			unreadable++
			continue
		}
		scanned++
		if n != 1 {
			continue // 0 is scaled down, which is a different question
		}
		row := AssetRow(w, hygieneReplicaFact(in, w))
		row.Label = utilNamespace(w) + "/" + DisplayName(w)
		row.Value = "1 replica"
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil
	}

	caveat := "Plenty of things are correctly single-replica: leader-elected controllers where a " +
		"second replica is a bug, a database whose replication is not Kubernetes' business, and " +
		"anything in an environment where the cost of a second copy is not worth the availability. " +
		"This reads spec.replicas as collected, so a workload a HorizontalPodAutoscaler happens to " +
		"be holding at 1 is indistinguishable from one pinned there, and nothing here can see whether " +
		"the workload tolerates a restart, how fast it reschedules, or whether a PodDisruptionBudget " +
		"already protects it. DaemonSets are not counted: one pod per node is their design."
	if unreadable > 0 {
		caveat += fmt.Sprintf(" %s workload%s had no readable spec.replicas and %s absent from this "+
			"list.", formatInt(unreadable), plural(unreadable), pluralVerb(unreadable, "is", "are"))
	}

	return []Finding{{
		ID:    "hygiene.single-replica-workloads",
		Title: "Workloads declaring a single replica",
		Summary: fmt.Sprintf("%s of %s Deployments and StatefulSets %s a single replica, so a node "+
			"loss removes the only copy until it reschedules.", formatInt(len(rows)),
			formatInt(scanned), pluralVerb(len(rows), "declares", "declare")),
		// Information: a single replica is a legitimate configuration far more
		// often than it is an oversight, and the reader knows which of theirs
		// are which in a way this tool never will.
		Severity: SeverityInfo,
		Count:    len(rows),
		Basis: fmt.Sprintf("spec.replicas == 1 read from Asset.Raw on %s and %s; workloads scaled to "+
			"zero are excluded", hygieneDeploymentType, hygieneStatefulSetType),
		Caveat: caveat,
		Rows:   rows,
	}}
}

// hygieneReplicaFact adds the observed side of the declaration when the payload
// carries it: "declares 1, 0 ready" is a materially different row from
// "declares 1, 1 ready".
func hygieneReplicaFact(in *Input, w core.Asset) string {
	fact := w.Type
	if v, ok := in.RawPath(w, "status.readyReplicas"); ok {
		if n, ok := v.(float64); ok {
			fact += fmt.Sprintf(", %s ready", utilNum(n, 0))
		}
	}
	return fact
}

// ----------------------------------------------------------------------
// formatting
// ----------------------------------------------------------------------

// hygieneWhen renders a signed duration relative to the audit clock: positive
// is ahead ("in 6 days"), negative behind ("42 days ago"). The units coarsen
// with distance because "1,187 days ago" is a number nobody converts.
func hygieneWhen(d time.Duration) string {
	ahead := d >= 0
	if !ahead {
		d = -d
	}
	days := d.Hours() / 24
	var mag string
	switch {
	case days < 1:
		return "today"
	case days < 90:
		n := int(days)
		mag = fmt.Sprintf("%d day%s", n, plural(n))
	case days < 730:
		// The month and year branches cannot produce a 1 — 90 days is three
		// months and 730 is two years — so only days needs the plural.
		mag = fmt.Sprintf("%d months", int(days/30.44))
	default:
		mag = utilNum(days/365.25, 1) + " years"
	}
	if ahead {
		return "in " + mag
	}
	return mag + " ago"
}
