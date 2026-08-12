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
			groupBy := v.GetString("group-by")

			detail, err := topology.ParseDetail(v.GetString("detail"))
			if err != nil {
				return err
			}

			renderer, err := topology.New(format, topology.WithGroupBy(topology.RenderGroupBy(detail, groupBy)))
			if err != nil {
				return err
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
	cmd.Flags().String("group-by", "",
		"cluster nodes by provider|account|region in the dot/mermaid/d2/drawio renderers (default: flat)")
	cmd.Flags().String("detail", "low",
		"diagram detail: low (every asset) | medium (one node per group+type) | high (one node per group). medium/high bucket by --group-by, defaulting to provider")

	addGraphSourceFlags(cmd)
	return cmd
}
