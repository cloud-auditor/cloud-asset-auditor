package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/diff"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/store"
)

// ErrDrift signals that --exit-code was set and the snapshots differ.
// Execute() in root.go maps any error that isn't ErrPartial to exit code 1,
// which is exactly the `git diff --exit-code` contract — so a plain
// sentinel returned from RunE is enough; no exit-code plumbing or os.Exit
// (which would skip the deferred telemetry flush and output-file close) is
// needed. The report is always rendered before the sentinel is returned,
// so a CI gate gets both the drift details on stdout and the failing code.
var ErrDrift = errors.New("drift detected")

// diff takes a *cliState only for the --since mode, which reads the stored
// snapshots out of --db (and, with --against live, runs the providers). The
// two-file form still touches none of it.
func newDiffCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff [old.json new.json]",
		Short: "Compare two audit snapshots and report drift.",
		Long: `Compares two snapshots and reports drift between them.

There are two ways to name the pair, and they cannot be mixed:

  auditor diff old.json new.json     two files from "auditor audit -o json"
                                     (JSON array or NDJSON; auto-detected)
  auditor diff --since 30d           the stored snapshot from 30 days ago
                                     against the newest stored snapshot

--since selects the newest snapshot taken AT OR BEFORE that point, never one
taken after it: a baseline from the wrong side of the line produces drift
that is confident and wrong. When no snapshot is that old the command says
how far back the history actually goes instead of picking something else.

The comparison stays inside one provider set (the cache key), so a one-off
"--provider netbird" snapshot is never diffed against a full audit — every
absent provider's assets would otherwise be reported as removed.

Assets are matched across snapshots by (provider, id). Drift falls into
three categories:

  added    present only in the new snapshot
  removed  present only in the old snapshot
  changed  present in both, but name/type/region/account_id/status or
           tags differ (raw and created_at are deliberately not compared)

Examples:
  auditor audit -o json --output-file before.json   # ... time passes ...
  auditor audit -o json --output-file after.json
  auditor diff before.json after.json
  auditor diff before.json after.json -o markdown >> "$GITHUB_STEP_SUMMARY"
  auditor diff before.json after.json --exit-code   # CI gate: exit 1 on drift

  auditor audit --cache -o json >/dev/null          # populate the store, repeatedly
  auditor diff --since 30d                          # a month of drift
  auditor diff --since 2026-06-01 --against live    # baseline vs a live run
  auditor cache list                                # which snapshots exist
`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Plain flag reads (no viper binding) — like `version`, this
			// command's own knobs have no env/config-file surface. The --db
			// path and the provider knobs a live run needs come from the
			// shared viper, where the root command already put them.
			format, _ := cmd.Flags().GetString("output")
			outFile, _ := cmd.Flags().GetString("output-file")
			exitCode, _ := cmd.Flags().GetBool("exit-code")
			since, _ := cmd.Flags().GetString("since")
			against, _ := cmd.Flags().GetString("against")
			wantProviders, _ := cmd.Flags().GetStringSlice("providers")

			renderReport, err := diffRenderer(format)
			if err != nil {
				return err
			}
			if err := validateDiffMode(args, since, against, cmd.Flags().Changed("against"),
				wantProviders); err != nil {
				return err
			}

			var (
				oldAssets, newAssets []core.Asset
				provenance           string
			)
			if since == "" {
				if oldAssets, err = loadSnapshot(args[0]); err != nil {
					return err
				}
				if newAssets, err = loadSnapshot(args[1]); err != nil {
					return err
				}
			} else {
				timeout, _ := cmd.Flags().GetDuration("timeout")
				if oldAssets, newAssets, provenance, err = s.storedDiffPair(cmd.Context(), since, against,
					wantProviders, timeout); err != nil {
					return err
				}
			}

			w, closeOut, err := openOutput(outFile)
			if err != nil {
				return err
			}
			defer closeOut()

			// Provenance goes to the report, not to a log line, and only in
			// the human formats: "which two snapshots did I just compare" is
			// part of the answer when the command chose them itself. JSON
			// stays a clean machine document.
			if provenance != "" && !strings.EqualFold(format, "json") {
				fmt.Fprintf(w, "%s\n", provenance)
			}

			res := diff.Compute(oldAssets, newAssets)
			if err := renderReport(w, res, len(oldAssets), len(newAssets)); err != nil {
				return err
			}

			if exitCode && !res.Empty() {
				return ErrDrift
			}
			return nil
		},
	}

	cmd.Flags().StringP("output", "o", "table", "output format: table|json|markdown")
	cmd.Flags().String("output-file", "", "write output to this file instead of stdout")
	// No backticks in a usage string: cobra's UnquoteUsage reads the first
	// backticked word as the flag's TYPE, so the old "mirrors `git diff
	// --exit-code`" rendered the flag as "--exit-code git diff --exit-code".
	cmd.Flags().Bool("exit-code", false,
		"exit 1 when any drift is found, 0 when the snapshots match (mirrors 'git diff --exit-code')")
	cmd.Flags().String("since", "",
		"diff the stored snapshot from this far back instead of two files: a duration (720h), a day count (30d), a date (2026-06-01), or an RFC3339 timestamp. Requires snapshots written by 'audit --cache'")
	cmd.Flags().String("against", diffAgainstNewest,
		"with --since, what to compare the baseline to: newest (the latest stored snapshot) or live (collect from the providers now)")
	cmd.Flags().Duration("timeout", 10*time.Minute, "with --against live, the overall audit timeout")
	cmd.Flags().StringSlice("providers", nil,
		"with --since, pin the comparison to the snapshot series for this exact provider set (default: whichever series the baseline lands in)")
	return cmd
}

// The two things --since can be compared against.
const (
	diffAgainstNewest = "newest"
	diffAgainstLive   = "live"
)

// validateDiffMode enforces that exactly one of the two ways to name a pair
// is in play. Silently preferring one over the other is the failure this
// command exists to prevent, so an ambiguous invocation is an error rather
// than a guess.
func validateDiffMode(args []string, since, against string, againstSet bool, providers []string) error {
	switch {
	case since != "" && len(args) > 0:
		return errors.New("--since and two snapshot files are two ways to name the same pair: pass either the files or --since, not both")
	case since == "" && againstSet:
		return errors.New("--against only applies to --since (with two files, the second file IS what you are comparing against)")
	case since == "" && len(providers) > 0:
		return errors.New("--providers only applies to --since (it picks which stored snapshot series to read; two files name themselves)")
	case since == "" && len(args) != 2:
		return fmt.Errorf("diff needs two snapshot files, or --since with none (got %d file argument(s)); see `auditor diff --help`", len(args))
	}
	if since != "" && !strings.EqualFold(against, diffAgainstNewest) && !strings.EqualFold(against, diffAgainstLive) {
		return fmt.Errorf("unknown --against %q (supported: %s, %s)", against, diffAgainstNewest, diffAgainstLive)
	}
	return nil
}

// diffRenderer resolves -o to one of internal/diff's renderers. Resolved
// before anything is loaded so a typo costs a millisecond rather than a
// ten-minute live audit.
func diffRenderer(format string) (func(io.Writer, diff.Result, int, int) error, error) {
	switch strings.ToLower(format) {
	case "table":
		return diff.RenderTable, nil
	case "json":
		return diff.RenderJSON, nil
	case "markdown":
		return diff.RenderMarkdown, nil
	default:
		return nil, fmt.Errorf("unknown output format %q (supported: table, json, markdown)", format)
	}
}

// storedDiffPair resolves --since into a concrete (old, new) pair plus a line
// naming both sides. Returns a fatal error rather than a pair whenever the
// honest answer is "I cannot make this comparison".
func (s *cliState) storedDiffPair(ctx context.Context, since, against string, want []string, timeout time.Duration) (oldAssets, newAssets []core.Asset, provenance string, err error) {
	at, err := parseSince(since, time.Now())
	if err != nil {
		return nil, nil, "", err
	}

	st, err := openStore(s)
	if err != nil {
		return nil, nil, "", err
	}
	defer func() { _ = st.Close() }()

	baseline, err := st.AuditBefore(ctx, want, at)
	if errors.Is(err, store.ErrAuditNotFound) {
		return nil, nil, "", noBaselineError(ctx, st, since, at, want)
	}
	if err != nil {
		return nil, nil, "", err
	}
	if oldAssets, err = st.AuditAssets(ctx, baseline.ID); err != nil {
		return nil, nil, "", err
	}

	oldLine := fmt.Sprintf("Baseline: snapshot #%d from %s (%s ago, providers %s, %d assets)",
		baseline.ID, formatInstant(baseline.RunAt), humanAge(time.Since(baseline.RunAt)),
		strings.Join(baseline.Providers, ","), baseline.AssetCount)

	// The store can hold several independent series. Whichever one the baseline
	// landed in scopes the whole answer, so any other series has to be named:
	// see unexaminedSeriesWarning for why silence here is the worst outcome
	// this command can produce.
	warning := unexaminedSeriesWarning(ctx, st, baseline.Providers, want)

	if strings.EqualFold(against, diffAgainstLive) {
		// A live run collects whatever the baseline's provider set collected,
		// so the two sides describe the same scope.
		newAssets, err = s.liveSnapshot(ctx, baseline.Providers, timeout)
		if err != nil {
			return nil, nil, "", err
		}
		return oldAssets, newAssets, oldLine + fmt.Sprintf("\nCurrent:  live audit of %s (%d assets)\n",
			strings.Join(baseline.Providers, ","), len(newAssets)) + warning, nil
	}

	// Same provider set on both sides: comparing a netbird-only snapshot to a
	// full audit would report every Cloudflare and OCI asset as added.
	newest, err := st.NewestAudit(ctx, baseline.Providers)
	if err != nil {
		return nil, nil, "", err
	}
	if newest.ID == baseline.ID {
		return nil, nil, "", fmt.Errorf(
			"the snapshot nearest --since %s is also the newest one stored for providers %s "+
				"(#%d from %s) — comparing it against itself would report no drift while proving nothing. "+
				"Take a newer one with `auditor audit --cache`, or use --against live",
			since, strings.Join(baseline.Providers, ","), baseline.ID, formatInstant(baseline.RunAt))
	}
	if newAssets, err = st.AuditAssets(ctx, newest.ID); err != nil {
		return nil, nil, "", err
	}
	return oldAssets, newAssets, oldLine + fmt.Sprintf("\nCurrent:  snapshot #%d from %s (%s ago, %d assets)\n",
		newest.ID, formatInstant(newest.RunAt), humanAge(time.Since(newest.RunAt)), newest.AssetCount) + warning, nil
}

// unexaminedSeriesWarning names the provider series this comparison did NOT
// look at, and returns "" when there is nothing to warn about.
//
// The baseline is chosen by timestamp across every series in the store, and it
// then scopes both sides of the diff. A store holding a nightly full audit and
// an hourly "--provider netbird" run holds two independent histories: ask for
// "--since 3d" when the netbird series happens to sit nearest that instant and
// the answer is computed over netbird alone. The comparison is internally
// consistent — that part is already handled, both sides share a provider set —
// but it silently answers a much narrower question than the one asked, and it
// can answer it with "No drift", which is the most over-read output this tool
// produces. A clean bill of health over 43 of 590 assets is worse than an
// error, so the scope is stated and the lever to change it is named.
//
// Suppressed when --providers pinned the series: the user has already said
// which history they mean, so repeating it back is noise.
func unexaminedSeriesWarning(ctx context.Context, st *store.Store, chosen, want []string) string {
	if len(want) > 0 {
		return ""
	}
	sets, err := st.ProviderSets(ctx)
	if err != nil || len(sets) < 2 {
		// A failure here must not fail the diff: the comparison is still valid,
		// it is only the "what I ignored" note that is missing.
		return ""
	}

	chosenKey := store.ProviderSet{Providers: chosen}.Key()
	var others []string
	for _, set := range sets {
		if set.Key() == chosenKey {
			continue
		}
		others = append(others, fmt.Sprintf("        %s — %d snapshot(s), newest %s ago",
			strings.Join(set.Providers, ","), set.Count, humanAge(time.Since(set.Newest))))
	}
	if len(others) == 0 {
		return ""
	}

	return fmt.Sprintf("\nNote: this compares the %q series only — every count below is scoped to it.\n"+
		"      The store holds other, independent series that this answer says nothing about:\n%s\n"+
		"      Pin the one you meant with --providers, e.g. --providers %s\n",
		strings.Join(chosen, ","), strings.Join(others, "\n"),
		strings.Join(sets[0].Providers, ","))
}

// noBaselineError explains what the store DOES hold. Answering "nothing that
// old" with a bare error would leave the user guessing whether to widen
// --since by a day or by a year; answering it by quietly using the oldest
// snapshot would report a month of drift as if it were a week's.
func noBaselineError(ctx context.Context, st *store.Store, since string, at time.Time, want []string) error {
	oldest, err := st.OldestAudit(ctx, want)
	if errors.Is(err, store.ErrAuditNotFound) {
		// With --providers set, "nothing that old" and "nothing for that set at
		// all" are different problems with different fixes, so they get
		// different messages rather than one that fits neither.
		if len(want) > 0 {
			return fmt.Errorf("no stored snapshots for providers %s in %s%s",
				strings.Join(want, ","), st.Path(), knownSeriesHint(ctx, st))
		}
		return fmt.Errorf("no stored snapshots to compare against: `auditor diff --since` reads the "+
			"snapshots that `auditor audit --cache` writes to %s, and it holds none yet",
			st.Path())
	}
	if err != nil {
		return err
	}
	// The suggestion is the oldest snapshot's own timestamp rather than a
	// rounded age: "--since 31d" can still land on the wrong side of it.
	return fmt.Errorf("no stored snapshot at or before %s (--since %s); the oldest one is #%d from %s "+
		"(%s ago, providers %s, %d assets). Widen the window — e.g. --since %s — or name it explicitly: "+
		"auditor cache show %d > old.json",
		formatInstant(at), since, oldest.ID, formatInstant(oldest.RunAt), humanAge(time.Since(oldest.RunAt)),
		strings.Join(oldest.Providers, ","), oldest.AssetCount,
		formatInstant(oldest.RunAt), oldest.ID)
}

// knownSeriesHint lists the provider sets that DO have history, so a --providers
// typo ("kubernetes" vs "k8s") is one line away from being fixed rather than a
// guessing game against an opaque store.
func knownSeriesHint(ctx context.Context, st *store.Store) string {
	sets, err := st.ProviderSets(ctx)
	if err != nil || len(sets) == 0 {
		return ""
	}
	names := make([]string, 0, len(sets))
	for _, set := range sets {
		names = append(names, strings.Join(set.Providers, ","))
	}
	return fmt.Sprintf(". It holds snapshots for: %s", strings.Join(names, "; "))
}

// liveSnapshot collects a fresh snapshot from the given provider set.
//
// A partial collection is refused outright rather than reported: every asset
// belonging to the provider that failed would show up as "removed", which is
// the most alarming and most wrong thing this tool could print. It is the same
// rule `audit --cache` applies when deciding whether to persist a snapshot.
func (s *cliState) liveSnapshot(ctx context.Context, providers []string, timeout time.Duration) ([]core.Asset, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	selected := selectProviders(providers)
	// selectProviders only WARNS when a provider is unregistered or its
	// factory fails on missing credentials, which is right for `audit` and
	// wrong here: the resulting live snapshot would be missing that
	// provider's assets entirely and they would all read as removed. Silence
	// on stderr is not consent, so the mismatch is fatal.
	if missing := missingProviders(providers, selected); len(missing) > 0 {
		return nil, fmt.Errorf("cannot collect a comparable live snapshot: the baseline covers %s but %s "+
			"did not start here (unregistered, or missing credentials — see the warnings above). "+
			"Every one of their assets would be reported as removed",
			strings.Join(providers, ","), strings.Join(missing, ","))
	}
	// diff registers none of the provider knobs itself, so these come from
	// the config file / AUDITOR_* env only — the natural home for "my
	// estate's OCI profile and kube context" when the same knobs have to
	// match the snapshot they are being compared against. Unset means the
	// zero value, which every provider treats as "use my default".
	opts := graphProviderOptions(s.v)
	// Raw is never compared (see internal/diff's package doc), so collecting
	// it would be pure cost — unlike topology/reach, which need it.
	opts.includeRaw = false
	applyProviderOptions(selected, opts)

	assets, errs := runProviders(ctx, selected)

	var provErrs []error
	errsDone := make(chan struct{})
	go func() {
		for e := range errs {
			if e != nil {
				provErrs = append(provErrs, e)
			}
		}
		close(errsDone)
	}()

	collected := make([]core.Asset, 0, 1024)
	for a := range assets {
		collected = append(collected, a)
	}
	<-errsDone

	if len(provErrs) > 0 {
		return nil, errors.Join(append([]error{fmt.Errorf(
			"refusing to report drift against an incomplete live audit — every asset of a provider that "+
				"failed would read as removed; %w", ErrPartial)}, provErrs...)...)
	}
	return collected, nil
}

// missingProviders reports which of the baseline's providers failed to start.
func missingProviders(want []string, got []core.Provider) []string {
	started := make(map[string]struct{}, len(got))
	for _, p := range got {
		started[strings.ToLower(p.Name())] = struct{}{}
	}
	var missing []string
	for _, name := range want {
		if _, ok := started[strings.ToLower(name)]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// dayCount matches the "30d" form. Go's duration grammar stops at hours
// because a day is not a fixed span, but "30 days ago" is the question people
// actually ask, so the suffix is accepted and expanded as 24h.
var dayCount = regexp.MustCompile(`^(\d+)d$`)

// parseSince turns --since into the instant a baseline must not be newer
// than. Bare dates resolve to local midnight — the start of that day, so
// "--since 2026-06-01" includes nothing from June 1st onwards.
func parseSince(spec string, now time.Time) (time.Time, error) {
	spec = strings.TrimSpace(spec)
	if m := dayCount.FindStringSubmatch(spec); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid --since %q: %w", spec, err)
		}
		return now.AddDate(0, 0, -days), nil
	}
	if d, err := time.ParseDuration(spec); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("invalid --since %q: the window must be positive (it looks backwards from now)", spec)
		}
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, spec); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, spec, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q (want a duration like 720h, a day count like 30d, "+
		"a date like 2026-06-01, or an RFC3339 timestamp)", spec)
}

// formatInstant is the one timestamp spelling every history command prints.
func formatInstant(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// humanAge spells "how long ago" at the scale the reader cares about.
// time.Duration's own String() renders a month as "720h0m0s", which is
// correct and unreadable — and these reports exist to be read at a glance.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		days := int(d.Hours()) / 24
		if hours := int(d.Hours()) % 24; hours > 0 {
			return fmt.Sprintf("%dd%dh", days, hours)
		}
		return fmt.Sprintf("%dd", days)
	}
}

// loadSnapshot opens and parses one snapshot file, wrapping errors with the
// path so "auditor diff a.json b.json" failures say which side broke.
func loadSnapshot(path string) ([]core.Asset, error) {
	// G304: same rationale as openOutput — the path is operator-supplied
	// on a CLI process the operator owns; the binary is the trust boundary.
	f, err := os.Open(path) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()

	assets, err := diff.Load(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return assets, nil
}
