package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/filter"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

func newTopologyCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "topology",
		Short: "Infer the request-path graph between collected assets.",
		Long: `Builds the cross-provider topology from the inventory:

  DNS (Cloudflare) → security rules → cloud LB (OCI) → Gateway (K8s) → Service

Forces --include-raw on providers so resolvers can read upstream payload
fields (e.g. Service.spec.ports, Ingress.spec.rules). The rendered output
omits Raw to stay readable.

Examples:
  auditor topology -o dot | dot -Tsvg > flow.svg
  auditor topology -o dot --group-by provider | dot -Tsvg > flow.svg  # cluster per cloud

  # High-level network diagram: one box per provider, arrows weighted by
  # how many underlying relationships they stand for.
  auditor topology --detail high -o dot | dot -Tsvg > overview.svg
  auditor topology --detail medium --group-by account -o mermaid   # per account, per type

  auditor topology --hostname api.example.com -o mermaid
  auditor topology -o d2 > topology.d2 && d2 topology.d2 topology.svg  # d2lang.com layout
  auditor topology -o graphml > topology.graphml         # import into yEd / Gephi / Cytoscape
  auditor topology -o json | jq '.edges[] | select(.kind == "lb-backend")'
  auditor topology -o excalidraw > topology.excalidraw   # drag into excalidraw.com to edit
  auditor topology -o drawio > topology.drawio           # open in draw.io / diagrams.net
  auditor topology -o html > topology.html               # self-contained interactive viewer

  # Build from a saved snapshot instead of a live audit (instant; pair
  # with 'audit -o json --include-raw' so the K8s resolvers see payloads):
  auditor topology --from-snapshot assets.json -o html > topology.html

  # Report what the graph connects to nothing, instead of drawing it. Read
  # the caveat it prints: an orphan is a gap in the inferred graph, not a
  # verdict on the resource.
  auditor topology --orphans
  auditor topology --orphans --group-by region -o json
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := s.v.BindPFlags(cmd.Flags()); err != nil {
				return fmt.Errorf("bind flags: %w", err)
			}
			v := s.v

			format := v.GetString("output")
			outFile := v.GetString("output-file")
			hostnames := v.GetStringSlice("hostname")
			includeOrphans := v.GetBool("include-orphans")
			orphanReport := v.GetBool("orphans")
			groupBy := v.GetString("group-by")

			detail, err := topology.ParseDetail(v.GetString("detail"))
			if err != nil {
				return err
			}

			// The orphan report replaces the diagram rather than annotating it,
			// so it takes over format selection and the renderer is never built.
			var renderer topology.Renderer
			if orphanReport {
				if detail != topology.DetailLow {
					return fmt.Errorf(
						"--orphans cannot be combined with --detail %s: collapsing replaces every asset "+
							"with one summary node per group and drops the edges that stayed inside a group, "+
							"so a collapsed node reads as having no edges while every asset inside it is "+
							"connected. Re-run with --detail low (the default)", detail)
				}
				format = orphanFormat(cmd, format)
			} else {
				renderer, err = topology.New(format, topology.WithGroupBy(topology.RenderGroupBy(detail, groupBy)))
				if err != nil {
					return err
				}
			}

			assetFilter, err := filter.Parse(v.GetStringSlice("filter"))
			if err != nil {
				return err
			}

			w, closeOut, err := openOutput(outFile)
			if err != nil {
				return err
			}
			defer closeOut()

			collected, provErrs, err := s.gatherForGraph(cmd.Context(), v)
			if err != nil {
				return err
			}

			topo := topology.Build(assetFilter.Slice(collected))
			if len(hostnames) > 0 {
				topo = topo.FilterByHostname(hostnames)
			}

			if orphanReport {
				// Reported off the graph as built — before DropOrphans, whose
				// keep set is this report's exact complement, and before
				// Collapse, which the --detail guard above already ruled out.
				report := topo.Orphans(groupBy)
				report.Caveat = append(report.Caveat,
					narrowingCaveats(hostnames, v.GetStringSlice("filter"))...)
				if err := topology.RenderOrphans(report, format, w); err != nil {
					return err
				}
				if len(provErrs) > 0 {
					return errors.Join(append([]error{ErrPartial}, provErrs...)...)
				}
				return nil
			}

			if !includeOrphans {
				topo = topo.DropOrphans()
			}
			// Collapse last: orphan-dropping and hostname tracing are
			// statements about individual assets, so they must run while the
			// individual assets are still there to be filtered.
			topo = topo.Collapse(detail, groupBy)

			if err := renderer.Render(topo, w); err != nil {
				return err
			}
			if len(provErrs) > 0 {
				return errors.Join(append([]error{ErrPartial}, provErrs...)...)
			}
			return nil
		},
	}

	cmd.Flags().StringP("output", "o", "json", "output format: json|dot|mermaid|d2|graphml|excalidraw|drawio|html")
	cmd.Flags().String("output-file", "", "write output to this file instead of stdout")
	cmd.Flags().StringSlice("hostname", nil, "trace only these hostnames (default: all)")
	cmd.Flags().StringArray("filter", nil,
		`build the graph from matching assets only: key=value[,value...] / key!=... with key provider|account|region|type|id|name|status|tag:KEY and glob values; repeatable (ANDed)`)
	cmd.Flags().Bool("include-orphans", false, "include asset nodes that have no edges")
	cmd.Flags().Bool("orphans", false,
		"report the assets the graph connects to nothing instead of drawing it (table|json; groups by --group-by, needs --detail low)")
	cmd.Flags().String("group-by", "",
		"cluster nodes by provider|account|region in the dot/mermaid/d2/drawio renderers (default: flat)")
	cmd.Flags().String("detail", "low",
		"diagram detail: low (every asset) | medium (one node per group+type) | high (one node per group). medium/high bucket by --group-by, defaulting to provider")

	addGraphSourceFlags(cmd)
	return cmd
}

// narrowingCaveats names the flags that were applied *before* the orphan count
// was taken, because each one moves the number in a direction a reader would
// otherwise attribute to the estate.
//
// Both distortions point opposite ways, which is why neither can be left
// unsaid: --hostname deflates the count almost to zero, and --filter inflates
// it by cutting the far end off relationships that do exist.
func narrowingCaveats(hostnames, filters []string) []string {
	var out []string
	if len(hostnames) > 0 {
		out = append(out, "--hostname narrowed the graph to the component reachable from the named "+
			"record(s) before this report ran. Everything that survives that filter has an edge by "+
			"construction, apart from a matched record that points at nothing — so a low count here "+
			"is a property of the filter and says nothing about the rest of the estate.")
	}
	if len(filters) > 0 {
		out = append(out, "--filter was applied before the graph was built, so any relationship whose "+
			"far end the filter excluded could not be inferred, and its near end is counted here as "+
			"an orphan. Re-run without --filter to tell the two apart.")
	}
	return out
}

// orphanFormat picks the output format for --orphans.
//
// `topology` defaults -o to json because its product is a diagram to pipe
// somewhere. An orphan report's product is a warning: the list of counts is
// worthless, and actively dangerous, without the paragraphs explaining that a
// degree-0 node is a gap in the inferred graph and not a verdict on the
// resource. Both formats carry that text, but only the table puts it in front
// of a human by default — so an unspecified -o means table here, and an
// explicit one is honoured as given (including an unsupported one, which
// RenderOrphans rejects with a message naming the two that work).
//
// Deliberately no --exit-code: gating CI on this number would institutionalise
// exactly the reading the report spends its first paragraph disowning.
func orphanFormat(cmd *cobra.Command, resolved string) string {
	if cmd.Flags().Changed("output") {
		return resolved
	}
	if def := cmd.Flags().Lookup("output").DefValue; resolved == def {
		// Unset on the command line and unchanged by config/env: take the
		// orphan default rather than topology's diagram default.
		return "table"
	}
	return resolved
}
