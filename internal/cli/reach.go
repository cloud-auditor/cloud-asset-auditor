package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/filter"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

// ErrExposed is returned (after the report is rendered) when --exit-code was
// set and the analysis found something reachable. Execute() maps it to exit
// code 1 so a pipeline can gate on "nothing new is internet-facing" the same
// way `diff --exit-code` gates on drift and `check --exit-code` on policy.
var ErrExposed = errors.New("reachable assets found")

func newReachCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reach",
		Short: "Trace what can reach what across the topology graph.",
		Long: `Answers reachability questions over the inferred topology.

Edges point the way a request travels, so:

  --from X            what can X reach?          (follows edges forwards)
  --to Y              what can reach Y?          (follows edges backwards)
  --from X --to Y     how does X get to Y?       (enumerates routes)
  --exposed           what can the internet reach?

Selectors are case-insensitive globs matched against each asset's id AND
name, so 'api.example.com', 'ocid1.loadbalancer.*', and '*-prod' all work.

Like ` + "`topology`" + `, this forces --include-raw on so the Kubernetes
resolvers can see Ingress / Service / NetworkPolicy payloads — without them
the graph is thinner and paths will be missed.

Examples:
  auditor reach --exposed                              # internet-facing surface
  auditor reach --exposed --exit-code                  # CI gate
  auditor reach --to '*postgres*'                      # what can reach the database
  auditor reach --from api.example.com --to '*pod*'    # trace a request path
  auditor reach --to '*db*' --kinds traffic-allow      # policy-only view
  auditor reach --exposed --from-snapshot assets.json -o dot | dot -Tsvg > exposure.svg
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := s.v.BindPFlags(cmd.Flags()); err != nil {
				return fmt.Errorf("bind flags: %w", err)
			}
			v := s.v

			from := v.GetString("from")
			to := v.GetString("to")
			exposed := v.GetBool("exposed")
			format := v.GetString("output")

			if !exposed && from == "" && to == "" {
				return errors.New("specify at least one of --from, --to, or --exposed (see `auditor reach --help`)")
			}

			opts := topology.ReachOptions{
				MaxHops:     v.GetInt("max-hops"),
				MaxPaths:    v.GetInt("max-paths"),
				Kinds:       v.GetStringSlice("kinds"),
				IncludeDeny: v.GetBool("include-deny"),
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

			collected, provErrs, err := s.gatherForGraph(cmd.Context(), v)
			if err != nil {
				return err
			}

			topo := topology.Build(assetFilter.Slice(collected))
			res, err := runReach(topo, from, to, exposed, opts)
			if err != nil {
				return err
			}

			if err := topology.RenderReach(res, format, w); err != nil {
				return err
			}

			if v.GetBool("exit-code") && len(res.Paths) > 0 {
				return errors.Join(ErrExposed, errors.Join(provErrs...))
			}
			if len(provErrs) > 0 {
				return errors.Join(append([]error{ErrPartial}, provErrs...)...)
			}
			return nil
		},
	}

	cmd.Flags().String("from", "", "source selector (glob on asset id or name): what can it reach?")
	cmd.Flags().String("to", "", "target selector (glob on asset id or name): what can reach it?")
	cmd.Flags().Bool("exposed", false,
		"start from the estate's public entry points (public DNS records, internet-facing load balancers, published Kubernetes Services)")
	cmd.Flags().Int("max-hops", 6, "maximum path length")
	cmd.Flags().Int("max-paths", 25, "maximum number of paths to report (the report says when it truncated)")
	cmd.Flags().StringSlice("kinds", nil,
		"restrict traversal to these edge kinds (e.g. traffic-allow,traffic-deny for a policy-only view); default: all but traffic-deny")
	cmd.Flags().Bool("include-deny", false,
		"follow traffic-deny edges too. Off by default: a deny edge states traffic does NOT flow, so traversing it would manufacture forbidden routes")
	cmd.Flags().StringP("output", "o", "table",
		"output format: table|json|dot|mermaid|d2|graphml|excalidraw|drawio|html")
	cmd.Flags().String("output-file", "", "write output to this file instead of stdout")
	cmd.Flags().Bool("exit-code", false, "exit 1 when any path is found (for CI gating)")
	cmd.Flags().StringArray("filter", nil,
		`build the graph from matching assets only: key=value[,value...] / key!=... with key provider|account|region|type|id|name|status|tag:KEY and glob values; repeatable (ANDed)`)

	addGraphSourceFlags(cmd)
	return cmd
}

// runReach dispatches to the right traversal for the flag combination and
// builds the self-describing result. Split from the cobra glue so the
// question-selection logic is unit-testable without a command.
func runReach(topo *topology.Topology, from, to string, exposed bool, opts topology.ReachOptions) (topology.ReachResult, error) {
	var res topology.ReachResult

	switch {
	case exposed:
		exp := topo.Exposed(opts)
		res.Question = fmt.Sprintf("What can the internet reach? (from %d public entry point%s)",
			len(exp.Entries), pluralS(len(exp.Entries)))
		res.Sources = exp.Entries
		res.Paths = exp.Paths

	case from != "" && to != "":
		sources := topo.Select(from)
		targets := topo.Select(to)
		if err := requireMatch(from, sources); err != nil {
			return res, err
		}
		if err := requireMatch(to, targets); err != nil {
			return res, err
		}
		res.Question = fmt.Sprintf("How can %q reach %q?", from, to)
		res.Sources = sources
		res.Targets = targets
		res.Paths = topo.Paths(sources, targets, opts)

	case from != "":
		sources := topo.Select(from)
		if err := requireMatch(from, sources); err != nil {
			return res, err
		}
		res.Question = fmt.Sprintf("What can %q reach?", from)
		res.Sources = sources
		res.Paths = topo.Reachable(sources, topology.Downstream, opts)

	default: // to != ""
		targets := topo.Select(to)
		if err := requireMatch(to, targets); err != nil {
			return res, err
		}
		res.Question = fmt.Sprintf("What can reach %q?", to)
		res.Targets = targets
		res.Paths = topo.Reachable(targets, topology.Upstream, opts)
	}

	// Report truncation rather than silently capping: "these are all the ways
	// in" is the dangerous reading of a truncated security result.
	if max := opts.MaxPaths; max > 0 && len(res.Paths) >= max {
		res.Truncated = true
	}
	return res, nil
}

// requireMatch turns "your selector matched nothing" into a clear error rather
// than an empty report, which would otherwise read as "nothing can reach it".
func requireMatch(selector string, got []core.Asset) error {
	if len(got) > 0 {
		return nil
	}
	return fmt.Errorf("selector %q matched no assets "+
		"(selectors are globs over asset id and name — try widening it, e.g. %q, "+
		"or check that the owning provider ran)", selector, "*"+selector+"*")
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
