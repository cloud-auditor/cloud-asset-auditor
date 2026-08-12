package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/filter"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/metrics"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/output"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/telemetry"
)

// demoProviderName mirrors the demo package's registry name. It is a literal
// rather than an import so this file keeps compiling if the demo provider is
// ever built out of a distribution.
const demoProviderName = "demo"

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
			sheetBy := v.GetString("sheet-by")
			summary := v.GetBool("summary")
			timeout := v.GetDuration("timeout")
			opts := providerOptions{
				maxConcurrency:         v.GetInt("max-concurrency"),
				includeRaw:             v.GetBool("include-raw"),
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

			renderer, err := buildRenderer(format, stream, sheetBy, summary)
			if err != nil {
				return err
			}

			assetFilter, err := filter.Parse(v.GetStringSlice("filter"))
			if err != nil {
				return err
			}

			// xlsx is binary — refuse to spew it at an interactive terminal.
			if strings.EqualFold(format, "xlsx") && (outFile == "" || outFile == "-") && isCharDevice(os.Stdout) {
				return errors.New("xlsx is a binary format: pass --output-file <path>.xlsx (or redirect stdout to a file)")
			}

			w, closeOut, err := openOutput(outFile)
			if err != nil {
				return err
			}
			defer closeOut()

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			cacheMaxAge := v.GetDuration("cache-max-age")
			cacheWrite := v.GetBool("cache") || cacheMaxAge > 0
			dbPath := v.GetString("db")

			// Resolve the providers that will ACTUALLY run before touching the
			// cache — the cache key must describe the realized set, not the raw
			// request. selectProviders drops the "none" sentinel, unregistered
			// names, and providers whose factory fails (e.g. missing creds), so
			// keying on the request would let a partial set masquerade as a full
			// audit on a later cache hit.
			selected := selectProviders(providers)
			applyProviderOptions(selected, opts)
			keyProviders := providerNames(selected)

			// Cache read: serve a recent snapshot for this exact provider set
			// instead of collecting. Skipped when nothing resolved (no key).
			if cacheMaxAge > 0 && len(keyProviders) > 0 {
				if cached, runAt, fresh := readCache(ctx, dbPath, keyProviders, cacheMaxAge); fresh {
					slog.Info("serving audit from cache (skipping providers)",
						"providers", strings.Join(keyProviders, ","),
						"assets", len(cached), "age", time.Since(runAt).Round(time.Second))
					return renderer.Render(ctx, assetChan(ctx, assetFilter.Slice(cached)), w)
				}
			}

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

			var renderErr error
			if cacheWrite {
				// Caching inherently needs the full snapshot, so buffer it (a
				// documented exception to the streaming rule, like xlsx/html),
				// render from the buffer, then persist it for next time.
				var collected []core.Asset
				for a := range assets {
					collected = append(collected, a)
				}
				<-errsDone
				// The cache stores the FULL snapshot; --filter applies at
				// render time only, so a later run with a different (or no)
				// filter can still reuse this cache entry.
				renderErr = renderer.Render(ctx, assetChan(ctx, assetFilter.Slice(collected)), w)
				// Cache only a COMPLETE, uncancelled audit: never persist a
				// snapshot truncated by a provider failure (provErrs) or a ctx
				// timeout/cancel (ctx.Err) — a later cache hit would otherwise
				// serve missing assets as authoritative.
				if renderErr == nil && len(provErrs) == 0 && ctx.Err() == nil && len(keyProviders) > 0 {
					writeCache(ctx, dbPath, keyProviders, collected)
				}
			} else {
				if !assetFilter.Empty() {
					assets = assetFilter.Chan(ctx, assets)
				}
				renderErr = renderer.Render(ctx, assets, w)
				<-errsDone
			}

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
	cmd.Flags().StringP("output", "o", "json", "output format: json|csv|xlsx|html")
	cmd.Flags().String("output-file", "", "write output to this file instead of stdout")
	cmd.Flags().Bool("stream", false, "with -o json, emit NDJSON (one object per line) instead of an array")
	cmd.Flags().String("sheet-by", "provider",
		"with -o xlsx, split worksheets by one or more '+'-joined dimensions: none|provider|type|region|account|tag:KEY (e.g. tag:compartment_id, or region+tag:compartment_id for a sheet per region/compartment)")
	cmd.Flags().Bool("summary", false, "with -o xlsx, prepend a Summary worksheet (totals + per-sheet and per-type counts, linked to each sheet)")
	cmd.Flags().Bool("include-raw", false, "include the full provider payload in each asset")
	cmd.Flags().StringArray("filter", nil,
		`keep only matching assets: key=value[,value...] or key!=value[,value...] where key is provider|account|region|type|id|name|status|tag:KEY and values are case-insensitive globs (* wildcard); repeat the flag to AND conditions (e.g. --filter provider=oci --filter 'tag:env!=dev')`)
	cmd.Flags().Int("max-concurrency", 5, "per-provider parallelism")
	cmd.Flags().Duration("timeout", 10*time.Minute, "overall audit timeout")

	// Provider-scoped flags — declared from day one so the surface area in
	// init-plan.md §4 is stable. Wired to real behavior in Phases 2–4.
	cmd.Flags().String("oci-profile", "", "OCI config profile name")
	cmd.Flags().StringSlice("oci-regions", nil, `OCI regions to scan (default: every subscribed region); pass a comma-separated list to narrow`)
	cmd.Flags().StringSlice("oci-compartments", nil,
		`OCI compartments to scan, by OCID or name (default: every accessible compartment); subtree-inclusive — a named compartment pulls in its children`)
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
		"skip Kubernetes Event objects (core v1 + events.k8s.io) — high-volume and ephemeral")
	cmd.Flags().String("netbird-management-url", "",
		"NetBird self-hosted Management API base URL (default: NetBird cloud); env NETBIRD_MANAGEMENT_URL")
	cmd.Flags().String("gcp-project", "",
		"GCP project to inventory via Cloud Asset Inventory (default: $GOOGLE_CLOUD_PROJECT)")
	cmd.Flags().String("gcp-scope", "",
		"GCP Asset Inventory scope override: projects/<id>|folders/<num>|organizations/<num> (default: $GCP_SCOPE or the project)")
	cmd.Flags().String("tailscale-tailnet", "",
		`Tailscale tailnet to inventory (default: "-", the API token's own tailnet); env TAILSCALE_TAILNET`)
	cmd.Flags().String("tailscale-api-url", "",
		"Tailscale control-plane API base URL for self-hosted/Headscale planes (default: Tailscale cloud); env TAILSCALE_API_BASE_URL")
	cmd.Flags().Bool("cache", false,
		"persist the audit snapshot to the --db cache after collecting")
	cmd.Flags().Duration("cache-max-age", 0,
		"reuse a cached snapshot (from --db) instead of running providers when one newer than this exists (0 = always run live; implies --cache)")

	return cmd
}

func buildRenderer(format string, stream bool, sheetBy string, summary bool) (output.Renderer, error) {
	switch strings.ToLower(format) {
	case "json":
		return &output.JSON{Stream: stream}, nil
	case "csv":
		if stream {
			return nil, errors.New("--stream is only meaningful with -o json")
		}
		return &output.CSV{}, nil
	case "xlsx":
		if stream {
			return nil, errors.New("--stream is only meaningful with -o json")
		}
		r := &output.XLSX{SheetBy: sheetBy, Summary: summary}
		if err := r.Validate(); err != nil {
			return nil, err
		}
		return r, nil
	case "html":
		// Like csv, html ignores the xlsx-only --sheet-by/--summary knobs.
		if stream {
			return nil, errors.New("--stream is only meaningful with -o json")
		}
		return &output.HTML{}, nil
	default:
		return nil, fmt.Errorf("unknown output format %q (supported: json, csv, xlsx, html)", format)
	}
}

// isCharDevice reports whether f is an interactive terminal (vs a pipe or
// regular file) — used to avoid writing binary xlsx to a TTY.
func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func openOutput(path string) (io.Writer, func(), error) {
	if path == "" || path == "-" {
		return os.Stdout, func() {}, nil
	}
	// G304: the path is operator-supplied via --output-file or
	// AUDITOR_OUTPUT_FILE on a CLI process the operator owns. There's
	// no untrusted input here — the binary is the trust boundary.
	// (gosec uses its own #nosec directive; golangci-lint's //nolint is ignored when gosec runs standalone.)
	f, err := os.Create(path) // #nosec G304
	if err != nil {
		return nil, nil, fmt.Errorf("create output file: %w", err)
	}
	return f, func() { _ = f.Close() }, nil
}

// providerOptions bundles every CLI-derived knob the audit command pushes
// down to providers. Adding a new flag here is the right place to wire it.
type providerOptions struct {
	maxConcurrency         int
	includeRaw             bool
	ociProfile             string
	ociRegions             []string
	ociCompartments        []string
	kubeContext            string
	kubeContexts           []string
	kubeNamespace          string
	kubeExcludeNamespaces  []string
	kubeExcludeHelmSecrets bool
	kubeExcludeEvents      bool
	netbirdManagementURL   string
	gcpScope               string
	tailscaleTailnet       string
	tailscaleAPIURL        string
}

// resolveGCPScope turns the --gcp-scope / --gcp-project flags into a Cloud
// Asset Inventory scope. An explicit --gcp-scope wins; otherwise --gcp-project
// becomes projects/<id>. Empty means "leave the provider's env default".
func resolveGCPScope(scope, project string) string {
	if scope != "" {
		return scope
	}
	if project != "" {
		return "projects/" + project
	}
	return ""
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
		if c, ok := p.(core.CompartmentsConfigurable); ok {
			c.SetCompartments(opts.ociCompartments)
		}
		if c, ok := p.(core.KubeConfigurable); ok {
			c.SetKubeContext(opts.kubeContext)
			c.SetKubeContexts(opts.kubeContexts)
			c.SetKubeNamespace(opts.kubeNamespace)
			c.SetKubeExcludeNamespaces(opts.kubeExcludeNamespaces)
			c.SetKubeExcludeHelmSecrets(opts.kubeExcludeHelmSecrets)
			c.SetKubeExcludeEvents(opts.kubeExcludeEvents)
		}
		if c, ok := p.(core.NetbirdConfigurable); ok {
			c.SetManagementURL(opts.netbirdManagementURL)
		}
		if c, ok := p.(core.GCPConfigurable); ok {
			c.SetScope(opts.gcpScope)
		}
		if c, ok := p.(core.TailscaleConfigurable); ok {
			c.SetTailnet(opts.tailscaleTailnet)
			c.SetAPIBaseURL(opts.tailscaleAPIURL)
		}
	}
}

// demoMode records whether --demo was passed. It is read by selectProviders,
// which is the single funnel every provider-running command goes through
// (audit, topology, reach, check) — expressing the defaulting there means one
// place decides it instead of four. Atomic because the cobra tree is built
// per-Execute and the CLI test binary runs commands from several goroutines.
var demoMode atomic.Bool

func setDemoMode(on bool) { demoMode.Store(on) }

// selectProviders resolves the requested provider names into instantiated
// Providers. An empty input means "all registered" — or, under --demo, just
// the demo provider: a demo run that also fired every real provider at the
// operator's live credentials would be a nasty surprise, and mixing fabricated
// assets into a real inventory is worse. An explicit --provider always wins,
// so `--demo --provider demo,kubernetes` still scans the cluster.
//
// The literal "none" is a sentinel that filters out (the exit criterion of
// Phase 1 uses it).
func selectProviders(names []string) []core.Provider {
	if len(names) == 0 {
		if demoMode.Load() {
			names = []string{demoProviderName}
		} else {
			names = core.Registered()
		}
	}

	out := make([]core.Provider, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || strings.EqualFold(n, "none") {
			continue
		}
		factory, ok := core.Lookup(n)
		if !ok {
			slog.Warn("provider not registered", "provider", n)
			continue
		}
		p, err := factory()
		if err != nil {
			slog.Warn("provider failed to initialize", "provider", n, "error", err)
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
//
// Instrumentation: wraps the work in an "audit" span and emits a
// "provider.collect" child span per provider. The child span's ctx is
// passed into Provider.Collect so SDK calls inherit the trace.
func runProviders(ctx context.Context, providers []core.Provider) (<-chan core.Asset, <-chan error) {
	assets := make(chan core.Asset)
	errs := make(chan error)

	// Open the parent span BEFORE the no-providers check so every audit
	// — including the smoke-test empty one — produces a trace. Ops want
	// to see "user ran an audit but selected zero providers" too.
	names := make([]string, len(providers))
	for i, p := range providers {
		names[i] = p.Name()
	}
	ctx, parentSpan := telemetry.Tracer().Start(ctx, "audit",
		trace.WithAttributes(
			attribute.StringSlice("audit.providers", names),
			attribute.Int("audit.provider_count", len(providers)),
		))

	if len(providers) == 0 {
		parentSpan.End()
		close(assets)
		close(errs)
		return assets, errs
	}

	var wg sync.WaitGroup
	for _, p := range providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// auditor_audit_duration_seconds{provider=...} — observes
			// the elapsed time of this provider's Collect + forward.
			timer := prometheus.NewTimer(metrics.AuditDurationSeconds.WithLabelValues(p.Name()))
			defer timer.ObserveDuration()

			pCtx, pSpan := telemetry.Tracer().Start(ctx, "provider.collect",
				trace.WithAttributes(attribute.String("provider.name", p.Name())))
			defer pSpan.End()
			pAssets, pErrs := p.Collect(pCtx)
			forward(pCtx, p.Name(), pAssets, pErrs, assets, errs)
		}()
	}
	go func() {
		wg.Wait()
		close(assets)
		close(errs)
		parentSpan.End()
	}()
	return assets, errs
}

// forward copies values from a single provider's channels onto the fan-in
// channels until both source channels close or ctx is cancelled. Each
// asset / error increments the matching Prometheus counter — instrumenting
// here (rather than in every per-resource collector) keeps providers SDK-only
// and routes every emission through exactly one accounting site.
func forward(
	ctx context.Context,
	providerName string,
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
			metrics.AssetsCollectedTotal.WithLabelValues(providerName, a.Type).Inc()
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
			metrics.AuditErrorsTotal.WithLabelValues(providerName).Inc()
			select {
			case dstErrs <- e:
			case <-ctx.Done():
				return
			}
		}
	}
}
