package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
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
  auditor topology --hostname api.example.com -o mermaid
  auditor topology -o json | jq '.edges[] | select(.kind == "lb-backend")'
  auditor topology -o excalidraw > topology.excalidraw   # drag into excalidraw.com to edit
  auditor topology -o html > topology.html               # self-contained interactive viewer
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := s.v.BindPFlags(cmd.Flags()); err != nil {
				return fmt.Errorf("bind flags: %w", err)
			}
			v := s.v

			providers := v.GetStringSlice("provider")
			format := v.GetString("output")
			outFile := v.GetString("output-file")
			hostnames := v.GetStringSlice("hostname")
			includeOrphans := v.GetBool("include-orphans")
			timeout := v.GetDuration("timeout")
			maxConcurrency := v.GetInt("max-concurrency")

			renderer, err := topology.New(format)
			if err != nil {
				return err
			}

			w, closeOut, err := openOutput(outFile)
			if err != nil {
				return err
			}
			defer closeOut()

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			selected := selectProviders(providers)
			// Force --include-raw=true and apply other knobs. Resolvers
			// parse Raw for Service / Ingress / HTTPRoute payloads — the
			// graph would be empty for those without it.
			applyProviderOptions(selected, providerOptions{
				maxConcurrency:        maxConcurrency,
				includeRaw:            true,
				ociProfile:            v.GetString("oci-profile"),
				ociRegions:            v.GetStringSlice("oci-regions"),
				kubeContext:           v.GetString("kube-context"),
				kubeNamespace:         v.GetString("kube-namespace"),
				kubeExcludeNamespaces: v.GetStringSlice("kube-exclude-namespaces"),
			})

			// Materialize every asset before building the topology — the
			// resolvers need the full set for index lookups, not a stream.
			assets, errs := runProviders(ctx, selected)
			collected := make([]core.Asset, 0, 1024)
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
			for a := range assets {
				collected = append(collected, a)
			}
			<-errsDone

			topo := topology.Build(collected)
			if len(hostnames) > 0 {
				topo = topo.FilterByHostname(hostnames)
			}
			if !includeOrphans {
				topo = topo.DropOrphans()
			}

			if err := renderer.Render(topo, w); err != nil {
				return err
			}
			if len(provErrs) > 0 {
				return errors.Join(append([]error{ErrPartial}, provErrs...)...)
			}
			return nil
		},
	}

	cmd.Flags().StringSlice("provider", nil,
		`providers to run (default: all registered; use "none" to run zero)`)
	cmd.Flags().StringP("output", "o", "json", "output format: json|dot|mermaid|excalidraw|html")
	cmd.Flags().String("output-file", "", "write output to this file instead of stdout")
	cmd.Flags().StringSlice("hostname", nil, "trace only these hostnames (default: all)")
	cmd.Flags().Bool("include-orphans", false, "include asset nodes that have no edges")
	cmd.Flags().Int("max-concurrency", 5, "per-provider parallelism (mirrors `audit`)")
	cmd.Flags().Duration("timeout", 10*time.Minute, "overall audit + resolve timeout")

	// Provider-scoped flags mirrored from `audit` so a single invocation
	// can target the same cluster / tenancy / profile.
	cmd.Flags().String("oci-profile", "", "OCI config profile name")
	cmd.Flags().StringSlice("oci-regions", nil, `OCI regions to scan (default: every subscribed region)`)
	cmd.Flags().String("kube-context", "", "kubeconfig context name")
	cmd.Flags().String("kube-namespace", "", "limit Kubernetes audit to a single namespace")
	cmd.Flags().StringSlice("kube-exclude-namespaces",
		[]string{"kube-system", "kube-public", "kube-node-lease"},
		"Kubernetes namespaces to skip")

	return cmd
}
