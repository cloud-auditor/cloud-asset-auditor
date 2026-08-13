package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/diff"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/store"
)

func newHistoryCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history <id-or-glob>",
		Short: "Show one asset's timeline across the stored snapshots.",
		Long: `Reconstructs an asset's life from the snapshots "auditor audit --cache"
has stored in --db: when it first appeared, when it was last seen, whether it
is still in the newest snapshot, and which fields changed between which runs.

The selector is a case-insensitive glob matched against each asset's id AND
name — the same grammar ` + "`auditor reach --from`" + ` uses, so
'ocid1.instance.*', 'api.example.com' and '*-prod' all work.

Absence is only reported against snapshots that COULD have contained the
asset: a run scoped to --provider netbird says nothing about whether a
Cloudflare zone still exists, so those snapshots are skipped rather than read
as the asset having disappeared.

Examples:
  auditor history p-abc123                  # one asset by id
  auditor history '*-prod'                  # everything named like production
  auditor history 'ocid1.instance.*' -o json
  auditor cache list                        # which snapshots exist to search
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, _ := cmd.Flags().GetString("output")
			outFile, _ := cmd.Flags().GetString("output-file")
			limit, _ := cmd.Flags().GetInt("limit")

			if f := strings.ToLower(format); f != "table" && f != "json" {
				return fmt.Errorf("unknown output format %q (supported: table, json)", format)
			}

			st, err := openStore(s)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			report, err := buildHistory(cmd.Context(), st, args[0], limit)
			if err != nil {
				return err
			}

			w, closeOut, err := openOutput(outFile)
			if err != nil {
				return err
			}
			defer closeOut()

			if strings.EqualFold(format, "json") {
				enc := json.NewEncoder(w)
				enc.SetEscapeHTML(false)
				return enc.Encode(report)
			}
			return renderHistoryTable(w, report)
		},
	}

	cmd.Flags().StringP("output", "o", "table", "output format: table|json")
	cmd.Flags().String("output-file", "", "write output to this file instead of stdout")
	cmd.Flags().Int("limit", 25,
		"report at most this many matching assets (0 = no limit); the report says when it truncated")
	return cmd
}

// historyReport is what one `auditor history` invocation answers.
type historyReport struct {
	Selector string `json:"selector"`
	// Matched is how many assets the selector hit, which can exceed
	// len(Timelines) when --limit truncated. Reporting the cap rather than
	// silently capping is the same rule `reach` follows for --max-paths.
	Matched   int             `json:"matched"`
	Truncated bool            `json:"truncated"`
	Snapshots int             `json:"snapshots"`
	Timelines []assetTimeline `json:"timelines"`
}

// assetTimeline is one asset's life across the snapshots that could have
// contained it.
type assetTimeline struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	// Name and Type come from the LAST observation: an asset that was
	// renamed should be listed under what it is called now.
	Name string `json:"name"`
	Type string `json:"type"`

	FirstSeen snapshotRef `json:"first_seen"`
	LastSeen  snapshotRef `json:"last_seen"`
	// Newest is the most recent snapshot covering this asset's provider,
	// which is what InNewest is a statement about.
	Newest   snapshotRef `json:"newest_snapshot"`
	InNewest bool        `json:"in_newest_snapshot"`

	// Observed of Candidates: how many of the snapshots that could have held
	// this asset actually did.
	Observed   int             `json:"observed_in"`
	Candidates int             `json:"candidate_snapshots"`
	Events     []timelineEvent `json:"events"`
}

// snapshotRef names one stored snapshot.
type snapshotRef struct {
	AuditID int64     `json:"audit_id"`
	RunAt   time.Time `json:"run_at"`
}

// The four things that can happen to an asset between two snapshots.
const (
	eventAppeared    = "appeared"
	eventChanged     = "changed"
	eventDisappeared = "disappeared"
	eventReappeared  = "reappeared"
)

// timelineEvent is one transition. From is the snapshot the asset was last
// seen in and is absent for the first appearance; At is the snapshot where
// the transition was observed.
type timelineEvent struct {
	Kind   string             `json:"kind"`
	From   *snapshotRef       `json:"from,omitempty"`
	At     snapshotRef        `json:"at"`
	Fields []diff.FieldChange `json:"fields,omitempty"`
}

// buildHistory resolves the selector and assembles a timeline per match.
func buildHistory(ctx context.Context, st *store.Store, selector string, limit int) (historyReport, error) {
	audits, err := st.ListAudits(ctx) // newest first
	if err != nil {
		return historyReport{}, err
	}
	if len(audits) == 0 {
		return historyReport{}, fmt.Errorf("no stored snapshots to search: `auditor history` reads what "+
			"`auditor audit --cache` writes to %s, and it holds none yet", st.Path())
	}

	ids, err := st.MatchAssetIdentities(ctx, selector)
	if err != nil {
		return historyReport{}, err
	}
	if len(ids) == 0 {
		// Same wording contract as reach's requireMatch: an empty report
		// would otherwise read as "this asset has no history", which is a
		// very different claim from "nothing matched your selector".
		return historyReport{}, fmt.Errorf("selector %q matched no asset in any of the %d stored snapshot(s) "+
			"(selectors are globs over asset id and name — try widening it, e.g. %q)",
			selector, len(audits), "*"+selector+"*")
	}

	report := historyReport{
		Selector:  selector,
		Matched:   len(ids),
		Snapshots: len(audits),
	}
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
		report.Truncated = true
	}

	obs, err := st.AssetTimeline(ctx, ids)
	if err != nil {
		return historyReport{}, err
	}

	byIdentity := map[store.AssetIdentity][]store.Observation{}
	for _, o := range obs {
		key := store.AssetIdentity{Provider: o.Asset.Provider, ID: o.Asset.ID}
		byIdentity[key] = append(byIdentity[key], o)
	}
	for _, id := range ids {
		if seen := byIdentity[id]; len(seen) > 0 {
			report.Timelines = append(report.Timelines, timelineFor(seen, audits))
		}
	}
	return report, nil
}

// timelineFor walks one asset through the snapshots that could have contained
// it. observations must be oldest-first (AssetTimeline guarantees that);
// audits is newest-first (ListAudits' order).
func timelineFor(observations []store.Observation, audits []store.AuditMeta) assetTimeline {
	last := observations[len(observations)-1]
	t := assetTimeline{
		Provider:  last.Asset.Provider,
		ID:        last.Asset.ID,
		Name:      last.Asset.Name,
		Type:      last.Asset.Type,
		FirstSeen: refOf(observations[0]),
		LastSeen:  refOf(last),
		Observed:  len(observations),
	}

	seenAt := make(map[int64]store.Observation, len(observations))
	for _, o := range observations {
		seenAt[o.AuditID] = o
	}

	// Only snapshots whose provider set includes this asset's provider are
	// evidence about it. A netbird-only run does not mean the Cloudflare
	// zones were deleted — treating it as one would manufacture a
	// "disappeared" event on every narrow audit.
	candidates := make([]store.AuditMeta, 0, len(audits))
	for _, a := range audits {
		if coversProvider(a, t.Provider) {
			candidates = append(candidates, a)
		}
	}
	// Oldest first, id breaking a tie the same way ListAudits does — two
	// snapshots can share a run_at second, and the walk below reads the order
	// as chronology.
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].RunAt.Equal(candidates[j].RunAt) {
			return candidates[i].RunAt.Before(candidates[j].RunAt)
		}
		return candidates[i].ID < candidates[j].ID
	})
	t.Candidates = len(candidates)

	if n := len(candidates); n > 0 {
		newest := candidates[n-1]
		t.Newest = snapshotRef{AuditID: newest.ID, RunAt: newest.RunAt}
		_, t.InNewest = seenAt[newest.ID]
	}

	// prev is the previous snapshot's observation (nil when the asset was
	// absent from it); lastSeen survives a gap, so a reappearance can be
	// compared against what the asset looked like before it vanished.
	var prev, lastSeen *store.Observation
	for _, a := range candidates {
		cur, present := seenAt[a.ID]
		at := snapshotRef{AuditID: a.ID, RunAt: a.RunAt}
		switch {
		case present && lastSeen == nil:
			t.Events = append(t.Events, timelineEvent{Kind: eventAppeared, At: at})
		case present:
			// Reuse the drift comparison rather than writing a second one: a
			// timeline that disagreed with `auditor diff` about what counts
			// as a change would be worse than no timeline.
			kind := eventChanged
			if prev == nil {
				kind = eventReappeared
			}
			from := refOf(*lastSeen)
			var fields []diff.FieldChange
			if res := diff.Compute([]core.Asset{lastSeen.Asset}, []core.Asset{cur.Asset}); len(res.Changed) > 0 {
				fields = res.Changed[0].Fields
			}
			// An unchanged asset in an unchanged run is not an event; a
			// reappearance always is, changed or not.
			if kind == eventReappeared || len(fields) > 0 {
				t.Events = append(t.Events, timelineEvent{Kind: kind, From: &from, At: at, Fields: fields})
			}
		case lastSeen != nil && prev != nil:
			from := refOf(*prev)
			t.Events = append(t.Events, timelineEvent{Kind: eventDisappeared, From: &from, At: at})
		}
		if present {
			obs := cur
			prev, lastSeen = &obs, &obs
		} else {
			prev = nil
		}
	}
	return t
}

func refOf(o store.Observation) snapshotRef {
	return snapshotRef{AuditID: o.AuditID, RunAt: o.RunAt}
}

// coversProvider reports whether a snapshot ran the given provider. An audit
// with no recorded provider set (nothing resolved) covers nothing.
func coversProvider(a store.AuditMeta, provider string) bool {
	for _, p := range a.Providers {
		if strings.EqualFold(p, provider) {
			return true
		}
	}
	return false
}

// renderHistoryTable writes the human report: one block per asset, its
// summary lines, then the events in chronological order.
func renderHistoryTable(w io.Writer, r historyReport) error {
	bw := bufio.NewWriter(w)

	fmt.Fprintf(bw, "%d asset(s) matching %q across %d stored snapshot(s)",
		r.Matched, r.Selector, r.Snapshots)
	if r.Truncated {
		fmt.Fprintf(bw, " — showing the first %d", len(r.Timelines))
	}
	fmt.Fprintln(bw)

	for _, t := range r.Timelines {
		fmt.Fprintf(bw, "\n%s/%s %s", t.Provider, t.Type, t.ID)
		if t.Name != "" && t.Name != t.ID {
			fmt.Fprintf(bw, " (%s)", t.Name)
		}
		fmt.Fprintln(bw)

		fmt.Fprintf(bw, "  first seen   %s  (snapshot #%d)\n", formatInstant(t.FirstSeen.RunAt), t.FirstSeen.AuditID)
		fmt.Fprintf(bw, "  last seen    %s  (snapshot #%d, %s ago)\n",
			formatInstant(t.LastSeen.RunAt), t.LastSeen.AuditID, humanAge(time.Since(t.LastSeen.RunAt)))
		fmt.Fprintf(bw, "  in newest    %s  (snapshot #%d from %s)\n",
			yesNo(t.InNewest), t.Newest.AuditID, formatInstant(t.Newest.RunAt))
		fmt.Fprintf(bw, "  observed in  %d of %d snapshot(s) covering %s\n",
			t.Observed, t.Candidates, t.Provider)

		if len(t.Events) == 0 {
			fmt.Fprintf(bw, "  no changes recorded\n")
			continue
		}
		fmt.Fprintln(bw)
		for _, e := range t.Events {
			fmt.Fprintf(bw, "  %s  #%-4d %s", formatInstant(e.At.RunAt), e.At.AuditID, e.Kind)
			switch {
			case e.Kind == eventDisappeared && e.From != nil:
				fmt.Fprintf(bw, " (last seen in #%d)", e.From.AuditID)
			case e.From != nil:
				fmt.Fprintf(bw, " since #%d", e.From.AuditID)
			}
			fmt.Fprintln(bw)
			for _, f := range e.Fields {
				fmt.Fprintf(bw, "        %s: %q -> %q\n", f.Field, f.Old, f.New)
			}
		}
	}
	return bw.Flush()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no "
}
