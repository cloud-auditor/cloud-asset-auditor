package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/diff"
)

// ErrDrift signals that --exit-code was set and the snapshots differ.
// Execute() in root.go maps any error that isn't ErrPartial to exit code 1,
// which is exactly the `git diff --exit-code` contract — so a plain
// sentinel returned from RunE is enough; no exit-code plumbing or os.Exit
// (which would skip the deferred telemetry flush and output-file close) is
// needed. The report is always rendered before the sentinel is returned,
// so a CI gate gets both the drift details on stdout and the failing code.
var ErrDrift = errors.New("drift detected")

// newDiffCmd takes no *cliState: diff is a pure local-file comparison with
// no providers, viper-bound knobs, or cross-cutting state to reach.
func newDiffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff <old.json> <new.json>",
		Short: "Compare two audit snapshots and report drift.",
		Long: `Compares two snapshots produced by "auditor audit -o json" (either the
default JSON array or NDJSON from --stream; the format is auto-detected)
and reports drift between them.

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
`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Plain flag reads (no viper binding) — like `version`, this
			// command has no env/config-file surface to honor.
			format, _ := cmd.Flags().GetString("output")
			outFile, _ := cmd.Flags().GetString("output-file")
			exitCode, _ := cmd.Flags().GetBool("exit-code")

			oldAssets, err := loadSnapshot(args[0])
			if err != nil {
				return err
			}
			newAssets, err := loadSnapshot(args[1])
			if err != nil {
				return err
			}

			res := diff.Compute(oldAssets, newAssets)

			w, closeOut, err := openOutput(outFile)
			if err != nil {
				return err
			}
			defer closeOut()

			switch strings.ToLower(format) {
			case "table":
				err = diff.RenderTable(w, res, len(oldAssets), len(newAssets))
			case "json":
				err = diff.RenderJSON(w, res, len(oldAssets), len(newAssets))
			case "markdown":
				err = diff.RenderMarkdown(w, res, len(oldAssets), len(newAssets))
			default:
				return fmt.Errorf("unknown output format %q (supported: table, json, markdown)", format)
			}
			if err != nil {
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
	cmd.Flags().Bool("exit-code", false,
		"exit 1 when any drift is found, 0 when the snapshots match (mirrors `git diff --exit-code`)")
	return cmd
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
