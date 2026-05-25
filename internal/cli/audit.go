package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/output"
)

func newAuditCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Collect assets from configured providers and render them.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Bind audit-scope flags so env (AUDITOR_*) and config-file
			// values can override defaults via viper precedence.
			if err := s.v.BindPFlags(cmd.Flags()); err != nil {
				return fmt.Errorf("bind flags: %w", err)
			}
			v := s.v

			providers := v.GetStringSlice("provider")
			format := v.GetString("output")
			outFile := v.GetString("output-file")
			stream := v.GetBool("stream")
			timeout := v.GetDuration("timeout")
			opts := providerOptions{
				maxConcurrency:        v.GetInt("max-concurrency"),
				includeRaw:            v.GetBool("include-raw"),
				ociProfile:            v.GetString("oci-profile"),
				ociRegions:            v.GetStringSlice("oci-regions"),
				kubeContext:           v.GetString("kube-context"),
				kubeNamespace:         v.GetString("kube-namespace"),
				kubeExcludeNamespaces: v.GetStringSlice("kube-exclude-namespaces"),
			}

			renderer, err := buildRenderer(format, stream)
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
			applyProviderOptions(selected, opts)
			assets, errs := runProviders(ctx, selected)

			// Drain provider errors in the background so the renderer
			// (consumer of assets) and providers (sender of errs) can never
			// deadlock each other.
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

			renderErr := renderer.Render(ctx, assets, w)
			<-errsDone

			if renderErr != nil {
				return renderErr
			}
			if len(provErrs) > 0 {
				return errors.Join(append([]error{ErrPartial}, provErrs...)...)
			}
			return nil
		},
	}

	cmd.Flags().StringSlice("provider", nil,
		`providers to run (e.g. oci,cloudflare,kubernetes; use "none" to run zero; default: all registered)`)
	cmd.Flags().StringP("output", "o", "json", "output format: json|csv")
	cmd.Flags().String("output-file", "", "write output to this file instead of stdout")
	cmd.Flags().Bool("stream", false, "with -o json, emit NDJSON (one object per line) instead of an array")
	cmd.Flags().Bool("include-raw", false, "include the full provider payload in each asset")
	cmd.Flags().Int("max-concurrency", 5, "per-provider parallelism")
	cmd.Flags().Duration("timeout", 10*time.Minute, "overall audit timeout")

	// Provider-scoped flags — declared from day one so the surface area in
	// init-plan.md §4 is stable. Wired to real behavior in Phases 2–4.
	cmd.Flags().String("oci-profile", "", "OCI config profile name")
	cmd.Flags().StringSlice("oci-regions", nil, `OCI regions to scan, or "all" for every subscribed region`)
	cmd.Flags().String("kube-context", "", "kubeconfig context name")
	cmd.Flags().String("kube-namespace", "", "limit Kubernetes audit to a single namespace")
	cmd.Flags().StringSlice("kube-exclude-namespaces",
		[]string{"kube-system", "kube-public", "kube-node-lease"},
		"Kubernetes namespaces to skip")

	return cmd
}

func buildRenderer(format string, stream bool) (output.Renderer, error) {
	switch strings.ToLower(format) {
	case "json":
		return &output.JSON{Stream: stream}, nil
	case "csv":
		if stream {
			return nil, errors.New("--stream is only meaningful with -o json")
		}
		return &output.CSV{}, nil
	default:
		return nil, fmt.Errorf("unknown output format %q (supported: json, csv)", format)
	}
}

func openOutput(path string) (io.Writer, func(), error) {
	if path == "" || path == "-" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("create output file: %w", err)
	}
	return f, func() { _ = f.Close() }, nil
}

// providerOptions bundles every CLI-derived knob the audit command pushes
// down to providers. Adding a new flag here is the right place to wire it.
type providerOptions struct {
	maxConcurrency        int
	includeRaw            bool
	ociProfile            string
	ociRegions            []string
	kubeContext           string
	kubeNamespace         string
	kubeExcludeNamespaces []string
}

// applyProviderOptions type-asserts each provider against the optional
// Configurable interfaces in core and applies the corresponding flag value.
// Providers that didn't opt into a given interface are silently skipped —
// these are knobs, not requirements.
func applyProviderOptions(providers []core.Provider, opts providerOptions) {
	for _, p := range providers {
		if c, ok := p.(core.ConcurrencyConfigurable); ok {
			c.SetMaxConcurrency(opts.maxConcurrency)
		}
		if c, ok := p.(core.IncludeRawConfigurable); ok {
			c.SetIncludeRaw(opts.includeRaw)
		}
		if c, ok := p.(core.ProfileConfigurable); ok {
			c.SetProfile(opts.ociProfile)
		}
		if c, ok := p.(core.RegionsConfigurable); ok {
			c.SetRegions(opts.ociRegions)
		}
		if c, ok := p.(core.KubeConfigurable); ok {
			c.SetKubeContext(opts.kubeContext)
			c.SetKubeNamespace(opts.kubeNamespace)
			c.SetKubeExcludeNamespaces(opts.kubeExcludeNamespaces)
		}
	}
}

// selectProviders resolves the requested provider names into instantiated
// Providers. An empty input means "all registered". The literal "none" is a
// sentinel that filters out (the exit criterion of Phase 1 uses it).
func selectProviders(names []string) []core.Provider {
	if len(names) == 0 {
		names = core.Registered()
	}

	out := make([]core.Provider, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || strings.EqualFold(n, "none") {
			continue
		}
		factory, ok := core.Lookup(n)
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: provider %q not registered\n", n)
			continue
		}
		p, err := factory()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: provider %q failed to initialize: %v\n", n, err)
			continue
		}
		out = append(out, p)
	}
	return out
}

// runProviders fans every provider's Collect channels into a single pair of
// channels. Both returned channels are closed exactly once, when every
// provider has finished. If providers is empty, the channels close
// immediately so the renderer emits an empty result.
func runProviders(ctx context.Context, providers []core.Provider) (<-chan core.Asset, <-chan error) {
	assets := make(chan core.Asset)
	errs := make(chan error)

	if len(providers) == 0 {
		close(assets)
		close(errs)
		return assets, errs
	}

	var wg sync.WaitGroup
	for _, p := range providers {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			pAssets, pErrs := p.Collect(ctx)
			forward(ctx, pAssets, pErrs, assets, errs)
		}()
	}
	go func() {
		wg.Wait()
		close(assets)
		close(errs)
	}()
	return assets, errs
}

// forward copies values from a single provider's channels onto the fan-in
// channels until both source channels close or ctx is cancelled.
func forward(
	ctx context.Context,
	srcAssets <-chan core.Asset, srcErrs <-chan error,
	dstAssets chan<- core.Asset, dstErrs chan<- error,
) {
	for srcAssets != nil || srcErrs != nil {
		select {
		case <-ctx.Done():
			return
		case a, ok := <-srcAssets:
			if !ok {
				srcAssets = nil
				continue
			}
			select {
			case dstAssets <- a:
			case <-ctx.Done():
				return
			}
		case e, ok := <-srcErrs:
			if !ok {
				srcErrs = nil
				continue
			}
			if e == nil {
				continue
			}
			select {
			case dstErrs <- e:
			case <-ctx.Done():
				return
			}
		}
	}
}
