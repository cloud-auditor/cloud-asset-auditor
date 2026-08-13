package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Shared plumbing for the graph-consuming commands (`topology`, `reach`).
//
// Both need the same thing: every asset materialised in memory with Raw
// attached, obtained either from a live audit or a saved snapshot. Keeping one
// implementation means a new provider flag reaches both commands at once —
// they drifted apart once already, and a knob that silently applies to only
// one of two sibling commands is a bad surprise.

// addGraphSourceFlags registers the flags that decide *where the assets come
// from* and how the providers are configured. It deliberately does not
// register output/rendering flags — those differ per command.
func addGraphSourceFlags(cmd *cobra.Command) {
	cmd.Flags().StringSlice("provider", nil,
		`providers to run (default: all registered; use "none" to run zero)`)
	cmd.Flags().String("from-snapshot", "",
		"read assets from a saved 'audit -o json' snapshot (array or NDJSON) instead of running a live audit")
	// Backticks are load-bearing to cobra — UnquoteUsage reads the first
	// backticked word as the flag's type name, so "mirrors `audit`" renders as
	// "--max-concurrency audit". Keep flag usage strings backtick-free.
	cmd.Flags().Int("max-concurrency", 5, "per-provider parallelism (mirrors 'audit')")
	cmd.Flags().Duration("timeout", 10*time.Minute, "overall audit + resolve timeout")

	// Provider-scoped flags mirrored from `audit` so one invocation can target
	// the same cluster / tenancy / profile.
	cmd.Flags().String("oci-profile", "", "OCI config profile name")
	cmd.Flags().StringSlice("oci-regions", nil, `OCI regions to scan (default: every subscribed region)`)
	cmd.Flags().StringSlice("oci-compartments", nil,
		`OCI compartments to scan, by OCID or name (default: every accessible compartment); subtree-inclusive`)
	cmd.Flags().String("kube-context", "", "kubeconfig context name (single cluster)")
	cmd.Flags().StringSlice("kube-contexts", nil,
		`kubeconfig contexts to scan (comma-separated; "all" = every context). Overrides --kube-context`)
	cmd.Flags().String("kube-namespace", "", "limit Kubernetes audit to a single namespace")
	cmd.Flags().StringSlice("kube-exclude-namespaces",
		[]string{"kube-system", "kube-public", "kube-node-lease"},
		"Kubernetes namespaces to skip")
	cmd.Flags().Bool("kube-exclude-helm-secrets", false,
		"skip Helm v3 release-state Secrets (type helm.sh/release.v1)")
	cmd.Flags().Bool("kube-exclude-events", false,
		"skip Kubernetes Event objects (core v1 + events.k8s.io) — high-volume and ephemeral, never part of the graph")
	cmd.Flags().String("netbird-management-url", "",
		"NetBird self-hosted Management API base URL (default: NetBird cloud)")
	cmd.Flags().String("gcp-project", "",
		"GCP project to inventory (default: $GOOGLE_CLOUD_PROJECT)")
	cmd.Flags().String("gcp-scope", "",
		"GCP Asset Inventory scope override: projects/<id>|folders/<num>|organizations/<num>")
	cmd.Flags().String("tailscale-tailnet", "",
		`Tailscale tailnet to inventory (default: "-", the API token's own tailnet)`)
	cmd.Flags().String("tailscale-api-url", "",
		"Tailscale control-plane API base URL for self-hosted/Headscale planes")
}

// graphProviderOptions reads the provider knobs a graph command was given.
//
// includeRaw is forced true, never read from a flag: the Kubernetes resolvers
// parse Ingress / HTTPRoute / Service / NetworkPolicy payloads out of Raw, and
// a graph built without them is silently missing whole classes of edge. That
// is worse than slow, so it is not the user's choice to make here.
func graphProviderOptions(v *viper.Viper) providerOptions {
	return providerOptions{
		maxConcurrency:         v.GetInt("max-concurrency"),
		includeRaw:             true,
		ociProfile:             v.GetString("oci-profile"),
		ociRegions:             v.GetStringSlice("oci-regions"),
		ociCompartments:        v.GetStringSlice("oci-compartments"),
		kubeContext:            v.GetString("kube-context"),
		kubeContexts:           v.GetStringSlice("kube-contexts"),
		kubeNamespace:          v.GetString("kube-namespace"),
		kubeExcludeNamespaces:  v.GetStringSlice("kube-exclude-namespaces"),
		kubeExcludeHelmSecrets: v.GetBool("kube-exclude-helm-secrets"),
		kubeExcludeEvents:      v.GetBool("kube-exclude-events"),
		netbirdManagementURL:   v.GetString("netbird-management-url"),
		gcpScope:               resolveGCPScope(v.GetString("gcp-scope"), v.GetString("gcp-project")),
		tailscaleTailnet:       v.GetString("tailscale-tailnet"),
		tailscaleAPIURL:        v.GetString("tailscale-api-url"),
	}
}

// gatherForGraph materialises every asset the graph should be built from.
//
// Returns (assets, non-fatal provider errors, fatal error). The resolvers need
// the full set for index lookups, so this is one of the documented exceptions
// to the streaming invariant — a graph cannot be built from a stream.
func (s *cliState) gatherForGraph(ctx context.Context, v *viper.Viper) ([]core.Asset, []error, error) {
	if snapshot := v.GetString("from-snapshot"); snapshot != "" {
		// Snapshot path: no providers, no audit. Mirrors POST /api/v1/topology.
		assets, err := loadSnapshot(snapshot)
		return assets, nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, v.GetDuration("timeout"))
	defer cancel()

	selected := selectProviders(v.GetStringSlice("provider"))
	applyProviderOptions(selected, graphProviderOptions(v))

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

	return collected, provErrs, nil
}
