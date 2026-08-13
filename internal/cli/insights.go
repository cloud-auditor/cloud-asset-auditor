package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/cost"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/filter"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/insight"
)

// ErrInsights is returned (after the report is rendered) when --exit-code was
// set and a finding reached the --fail-on severity. Execute() maps it to exit
// code 1, the way `check --exit-code` gates on policy and `diff --exit-code`
// on drift.
var ErrInsights = errors.New("insight findings at or above the --fail-on severity")

// insightFormats mirrors internal/insight's Render switch so a typo'd -o is
// caught before a ten-minute audit runs rather than after it — same reason
// `cost` and `check` resolve their renderer up front.
var insightFormats = []string{"table", "json", "markdown"}

// listTypeSample caps how many required types --list prints per insight. A few
// insights accept a dozen alternative types, and a 400-column line in a listing
// is a line nobody reads.
const listTypeSample = 3

func newInsightsCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "insights",
		Short: "Derive findings from the inventory that has already been collected.",
		Long: `Reports what stands out in an inventory: what is reachable from outside, how
the network is arranged, what carries no owner, what expires soon, where the
money is.

This is not a collector. Every number comes from assets the audit already
holds plus the topology graph it already infers, so nothing here costs an
extra provider API call and the whole report works just as well against a
snapshot taken six months ago (--from-snapshot).

Every finding carries two lines that are part of the finding, not a footnote:
a BASIS naming concretely what was joined, and a CANNOT KNOW naming what that
join does not settle. Read the second before acting on the first. An inventory
cannot see consumption, cannot see traffic, and cannot see intent — so these
are questions to go and answer, not verdicts, and the report says so above the
first number rather than at the bottom.

Finding nothing is a normal result and is NOT a clean bill of health. An
insight that could not run at all (no raw payloads, no cost estimates, the
owning provider absent) is listed under NOT RUN instead of returning an empty
section, because "nothing found" and "never looked" must not look alike.

Like ` + "`topology`" + `, ` + "`reach`" + ` and ` + "`cost`" + `, this forces --include-raw on: half the
insights read a resource's own document — Kubernetes specs, policy bodies,
certificate details — and a report that quietly shrank because the payloads
were absent is the worse failure.

Money-shaped findings are off until --cost, because they need a price book and
because a report that silently prices some things and not others reads as a
complete accounting. They obey the same rules as ` + "`auditor cost`" + `: list-price
estimates marked with a leading ~, nothing unpriced ever rendered as 0.

Examples:
  auditor insights                                 # live audit, table report
  auditor insights --demo                          # synthetic estate, no credentials
  auditor insights --list                          # every question this binary asks
  auditor insights --only exposure,network
  auditor insights --only 'hygiene.*' --severity warn
  auditor insights --cost                          # include the money-shaped findings
  auditor insights --from-snapshot assets.json -o json | jq '.findings[].caveat'
  auditor insights -o markdown >> "$GITHUB_STEP_SUMMARY"
  auditor insights --exit-code                     # CI gate on risk-severity findings
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := s.v.BindPFlags(cmd.Flags()); err != nil {
				return fmt.Errorf("bind flags: %w", err)
			}
			v := s.v

			format := strings.ToLower(v.GetString("output"))
			if format == "md" {
				format = "markdown"
			}
			if !slices.Contains(insightFormats, format) {
				return fmt.Errorf("unknown insights output format %q (supported: %s)",
					format, strings.Join(insightFormats, ", "))
			}

			// Both severities are parsed before a provider is touched, for the
			// same reason the format is.
			var minSeverity insight.Severity
			if raw := v.GetString("severity"); strings.TrimSpace(raw) != "" {
				parsed, err := insight.ParseSeverity(raw)
				if err != nil {
					return fmt.Errorf("--severity: %w", err)
				}
				minSeverity = parsed
			}
			failOn, err := insight.ParseSeverity(v.GetString("fail-on"))
			if err != nil {
				return fmt.Errorf("--fail-on: %w", err)
			}

			assetFilter, err := filter.Parse(v.GetStringSlice("filter"))
			if err != nil {
				return err
			}

			w, closeOut, err := openOutput(v.GetString("output-file"))
			if err != nil {
				return err
			}
			defer closeOut()

			// --list is a question about this binary, not about an estate, so it
			// answers before a provider is touched. That makes it the one way to
			// audit what this tool will ask without handing it a credential —
			// the same role `cost --rates` plays for the price book.
			if v.GetBool("list") {
				return renderInsightList(w)
			}

			// The estimator is nil unless --cost, and nil is the switch: every
			// cost-bearing insight declares Requirements{Cost: true}, so with it
			// off they are reported as NOT RUN rather than silently absent.
			var inputOpts []insight.InputOption
			if v.GetBool("cost") {
				book, err := loadPriceBook(cmd.Context(), v)
				if err != nil {
					return err
				}
				inputOpts = append(inputOpts, insight.WithEstimator(cost.New(book)))
			}

			collected, provErrs, err := s.gatherForGraph(cmd.Context(), v)
			if err != nil {
				return err
			}

			// NewInput builds the topology itself from exactly the assets the
			// filter kept. Passing a graph built from the unfiltered set would
			// let an insight cite an edge to an asset the report never lists.
			in := insight.NewInput(assetFilter.Slice(collected), inputOpts...)
			report := insight.Run(cmd.Context(), in, insight.Options{
				Only:        v.GetStringSlice("only"),
				MinSeverity: minSeverity,
			})

			if err := insight.Render(report, format, w, insight.WithMaxRows(v.GetInt("max-rows"))); err != nil {
				return err
			}

			// Provider failures surface after the report, not instead of it: a
			// partial inventory still yields findings, and the report's own
			// scope line is what tells the reader how partial it was.
			if len(provErrs) > 0 {
				return errors.Join(append([]error{ErrPartial}, provErrs...)...)
			}
			if v.GetBool("exit-code") {
				if worst, ok := report.MaxSeverity(); ok && worst.Rank() >= failOn.Rank() {
					return ErrInsights
				}
			}
			return nil
		},
	}

	cmd.Flags().StringP("output", "o", "table", "report format: table|json|markdown")
	cmd.Flags().String("output-file", "", "write the report to this file instead of stdout")
	cmd.Flags().StringSlice("only", nil,
		"run only insights whose id or family matches these case-insensitive globs, e.g. exposure or 'hygiene.*' (repeatable)")
	cmd.Flags().String("severity", "",
		"drop findings below this severity: info|notable|warn|risk (default: report everything; the report still says how many it hid)")
	cmd.Flags().Bool("cost", false,
		"price the inventory and include the money-shaped findings (list-price estimates, same book and same disclaimer as 'auditor cost')")
	cmd.Flags().Bool("list", false,
		"print every registered insight, its family and what it needs, then exit — no audit, no credentials")
	cmd.Flags().Int("max-rows", 0,
		"detail rows to print per finding in table/markdown (0 = 12, negative = every row); -o json always carries every row")
	cmd.Flags().Bool("exit-code", false,
		"exit 1 when a finding reaches the --fail-on severity (for CI gating)")
	// risk, not warn. Of the four severities, only risk promises exact evidence
	// and an incident-shaped consequence; the other three are explicitly
	// questions, and a pipeline that fails on a question teaches the team to
	// stop reading the caveats — which is the one thing this feature exists to
	// make them do.
	cmd.Flags().String("fail-on", "risk",
		"minimum severity that trips --exit-code: info|notable|warn|risk")
	cmd.Flags().StringArray("filter", nil,
		`derive findings from matching assets only: key=value[,value...] / key!=... with key provider|account|region|type|id|name|status|tag:KEY and glob values; repeatable (ANDed)`)

	addPriceBookFlags(cmd)
	addGraphSourceFlags(cmd)
	return cmd
}

// renderInsightList prints the catalogue: what this binary can ask, grouped
// into the sections a report would print them in, with each insight's
// preconditions spelled out.
//
// The preconditions are the point. "Nothing found" is the commonest reading of
// a missing section, and the second commonest cause is that the insight never
// ran — so the listing names the flag or the provider that would have let it,
// in the same words the NOT RUN section uses.
func renderInsightList(w io.Writer) error {
	bw := bufio.NewWriter(w)

	all := insight.Registered()
	fmt.Fprintf(bw, "INSIGHTS  (%d registered)\n\n", len(all))
	fmt.Fprintln(bw, insight.DisclaimerShort)

	idWidth := 0
	for _, i := range all {
		idWidth = max(idWidth, len(i.ID()))
	}

	var family insight.Family
	for _, i := range all {
		if i.Family() != family {
			family = i.Family()
			fmt.Fprintf(bw, "\n%s\n", family.Title())
		}
		fmt.Fprintf(bw, "  %-*s  %s\n", idWidth, i.ID(), i.Title())
		if needs := insightNeeds(i); needs != "" {
			fmt.Fprintf(bw, "  %-*s  needs %s\n", idWidth, "", needs)
		}
	}

	fmt.Fprintf(bw, "\nAn insight whose needs are unmet is reported under NOT RUN rather than as an\n"+
		"empty section. Run `auditor insights` to see the findings, each with the basis\n"+
		"it was derived from and the thing it cannot know.\n")
	return bw.Flush()
}

// insightNeeds renders an insight's declared Requirements as one phrase, or ""
// when it declared none (the optional Requiring interface, type-asserted the
// way core's Configurable interfaces are).
func insightNeeds(i insight.Insight) string {
	r, ok := i.(insight.Requiring)
	if !ok {
		return ""
	}
	req := r.Requires()

	var parts []string
	if req.Raw {
		parts = append(parts, "raw payloads (this command forces --include-raw on)")
	}
	if req.Topology {
		parts = append(parts, "at least one inferred edge")
	}
	if req.Cost {
		parts = append(parts, "cost estimates (--cost)")
	}
	if len(req.Providers) > 0 {
		parts = append(parts, "assets from "+strings.Join(req.Providers, " and "))
	}
	if len(req.Types) > 0 {
		parts = append(parts, "any of "+sampleList(req.Types, listTypeSample))
	}
	return strings.Join(parts, "; ")
}

// sampleList joins the first n entries and counts the rest. The full list is a
// property of the code, not of the run, so truncating it here loses nothing a
// reader of this listing needs.
func sampleList(in []string, n int) string {
	if len(in) <= n {
		return strings.Join(in, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(in[:n], ", "), len(in)-n)
}
