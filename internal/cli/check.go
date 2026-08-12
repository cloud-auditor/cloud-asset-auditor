package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/filter"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/policy"
)

// ErrFindings is returned (after the report is rendered) when `check
// --exit-code` found findings at or above the --fail-on severity, so CI can
// gate on policy compliance the way `diff --exit-code` gates on drift.
// Execute() maps it to exit code 1.
var ErrFindings = errors.New("policy findings detected")

func newCheckCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Evaluate policy rules against the inventory.",
		Long: `Runs YAML policy rules over the assets and reports the violations.

Rules select assets with glob matchers (provider, type, region, account,
status, name, tags) and assert conditions on them (required/forbidden tags,
tag values, status, name patterns). A rule without assertions flags every
asset it matches — use that shape to forbid whole classes of assets.

Examples:
  auditor check --init > policy.yaml            # write a starter ruleset
  auditor check --rules policy.yaml             # live audit, table report
  auditor check --rules policy.yaml --from assets.json -o markdown
  auditor check --rules policy.yaml --exit-code --fail-on error  # CI gate
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := s.v.BindPFlags(cmd.Flags()); err != nil {
				return fmt.Errorf("bind flags: %w", err)
			}
			v := s.v

			w, closeOut, err := openOutput(v.GetString("output-file"))
			if err != nil {
				return err
			}
			defer closeOut()

			if v.GetBool("init") {
				_, err := fmt.Fprint(w, policy.StarterRules)
				return err
			}

			ruleFiles := v.GetStringSlice("rules")
			if len(ruleFiles) == 0 {
				return errors.New("--rules is required (or --init to print a starter ruleset)")
			}
			rules, err := loadRuleFiles(ruleFiles)
			if err != nil {
				return err
			}

			failOn, err := policy.ParseSeverity(v.GetString("fail-on"))
			if err != nil {
				return fmt.Errorf("--fail-on: %w", err)
			}

			format := strings.ToLower(v.GetString("output"))
			render, err := checkRenderer(format)
			if err != nil {
				return err
			}

			assetFilter, err := filter.Parse(v.GetStringSlice("filter"))
			if err != nil {
				return err
			}

			var (
				collected []core.Asset
				provErrs  []error
			)
			if from := v.GetString("from"); from != "" {
				collected, err = loadSnapshot(from)
				if err != nil {
					return err
				}
			} else {
				ctx, cancel := context.WithTimeout(cmd.Context(), v.GetDuration("timeout"))
				defer cancel()

				selected := selectProviders(v.GetStringSlice("provider"))
				applyProviderOptions(selected, providerOptions{
					maxConcurrency:         v.GetInt("max-concurrency"),
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
				})

				// Materialize the full set — rules see all assets at once,
				// mirroring the topology command's buffering exception.
				assets, errs := runProviders(ctx, selected)
				collected = make([]core.Asset, 0, 1024)
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
			}

			collected = assetFilter.Slice(collected)
			findings := policy.Evaluate(rules, collected)
			if err := render(w, findings, len(collected)); err != nil {
				return err
			}

			if len(provErrs) > 0 {
				return errors.Join(append([]error{ErrPartial}, provErrs...)...)
			}
			if v.GetBool("exit-code") {
				if max, ok := policy.MaxSeverity(findings); ok && max.Rank() >= failOn.Rank() {
					return ErrFindings
				}
			}
			return nil
		},
	}

	cmd.Flags().StringSlice("rules", nil, "policy rule file(s) to evaluate (YAML; repeatable)")
	cmd.Flags().Bool("init", false, "print a commented starter ruleset and exit")
	cmd.Flags().String("from", "",
		"check a saved 'audit -o json' snapshot instead of running a live audit")
	cmd.Flags().StringP("output", "o", "table", "report format: table|json|markdown")
	cmd.Flags().String("output-file", "", "write the report to this file instead of stdout")
	cmd.Flags().Bool("exit-code", false,
		"exit non-zero when findings reach the --fail-on severity (for CI)")
	cmd.Flags().String("fail-on", "error",
		"minimum severity that trips --exit-code: info|warning|error|critical")
	cmd.Flags().StringArray("filter", nil,
		`check matching assets only: key=value[,value...] / key!=... with key provider|account|region|type|id|name|status|tag:KEY and glob values; repeatable (ANDed)`)
	cmd.Flags().StringSlice("provider", nil,
		`providers to run (default: all registered; use "none" to run zero)`)
	cmd.Flags().Int("max-concurrency", 5, "per-provider parallelism (mirrors `audit`)")
	cmd.Flags().Duration("timeout", 10*time.Minute, "overall audit + evaluate timeout")

	// Provider-scoped flags mirrored from `audit`, same as `topology`.
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
		"skip Kubernetes Event objects (core v1 + events.k8s.io)")
	cmd.Flags().String("netbird-management-url", "",
		"NetBird self-hosted Management API base URL (default: NetBird cloud)")
	cmd.Flags().String("gcp-project", "",
		"GCP project to inventory (default: $GOOGLE_CLOUD_PROJECT)")
	cmd.Flags().String("gcp-scope", "",
		"GCP Asset Inventory scope override: projects/<id>|folders/<num>|organizations/<num>")

	return cmd
}

// loadRuleFiles loads and concatenates every rule file, enforcing that rule
// names stay unique across files (Load only checks within one file).
func loadRuleFiles(paths []string) ([]policy.Rule, error) {
	var rules []policy.Rule
	seen := make(map[string]string)
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open rules %s: %w", path, err)
		}
		loaded, err := policy.Load(f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, r := range loaded {
			if prev, dup := seen[r.Name]; dup {
				return nil, fmt.Errorf("%s: rule %q already defined in %s", path, r.Name, prev)
			}
			seen[r.Name] = path
		}
		rules = append(rules, loaded...)
	}
	return rules, nil
}

func checkRenderer(format string) (func(io.Writer, []policy.Finding, int) error, error) {
	switch format {
	case "table":
		return policy.RenderTable, nil
	case "json":
		return policy.RenderJSON, nil
	case "markdown", "md":
		return policy.RenderMarkdown, nil
	default:
		return nil, fmt.Errorf("unknown check output format %q (want table|json|markdown)", format)
	}
}
