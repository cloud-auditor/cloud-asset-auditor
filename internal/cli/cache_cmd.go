package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/output"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/store"
)

// newCacheCmd is the `auditor cache` group: inspect and manage the audit
// snapshots that `auditor audit --cache` stores in the SQLite database
// (--db). Unlike the cache read/write on the audit path (which degrades to a
// live run on any store problem), these are explicit user actions on the
// cache itself, so store errors fail the command.
func newCacheCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Manage cached audit snapshots in the local database",
		Long: `Inspect and manage the audit snapshots that "auditor audit --cache" stores
in the SQLite database (--db).

Nothing here deletes anything you did not ask it to: snapshots accumulate
forever unless a retention policy is set (--cache-retain / --cache-retain-age),
because this database is the only copy of the history and a deleted snapshot
cannot be recomputed. "prune --dry-run" shows exactly what would go first.

Examples:
  auditor cache list                       # every snapshot + what it costs on disk
  auditor cache show 3 > snapshot.json     # re-emit a snapshot as audit JSON
  auditor diff <(auditor cache show 3) <(auditor cache show 5)
  auditor cache rm 3                       # delete one snapshot
  auditor cache prune --dry-run            # what the configured policy would remove
  auditor cache prune --keep 30            # keep the 30 newest per provider set
  auditor cache prune --max-age 168h       # delete snapshots older than a week
  auditor cache clear                      # delete them all (secrets untouched)`,
	}
	cmd.AddCommand(cacheListCmd(s), cacheShowCmd(s), cacheRmCmd(s), cachePruneCmd(s), cacheClearCmd(s))
	return cmd
}

func cacheListCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cached audit snapshots, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, _ := cmd.Flags().GetString("output")

			st, err := openStore(s)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			audits, err := st.ListAudits(cmd.Context())
			if err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			switch strings.ToLower(format) {
			case "table":
				if len(audits) == 0 {
					fmt.Fprintln(w, "No cached audits.")
					return nil
				}
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tWHEN\tAGE\tPROVIDERS\tASSETS")
				for _, a := range audits {
					fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\n",
						a.ID,
						a.RunAt.UTC().Format(time.RFC3339),
						time.Since(a.RunAt).Truncate(time.Second),
						strings.Join(a.Providers, ","),
						a.AssetCount)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
				// The footprint is the whole point of listing: unbounded
				// growth only bites people who never saw it accumulating.
				stats, err := st.CacheStats(cmd.Context())
				if err != nil {
					return err
				}
				fmt.Fprintf(w, "\n%d snapshot(s), %d asset row(s), %s on disk at %s\nRetention: %s\n",
					stats.Audits, stats.AssetRows, humanBytes(stats.Bytes), st.Path(),
					retentionFromViper(s.v).String())
				return nil
			case "json":
				return json.NewEncoder(w).Encode(audits)
			default:
				return fmt.Errorf("unknown output format %q (supported: table, json)", format)
			}
		},
	}
	cmd.Flags().StringP("output", "o", "table", "output format: table|json")
	return cmd
}

func cacheShowCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show ID",
		Short: "Re-emit a cached snapshot as audit JSON",
		Long: `Writes a cached snapshot's assets as a JSON array (or NDJSON with
--stream), byte-compatible with "auditor audit -o json" — so it can be piped
into "auditor diff" or "auditor topology --from".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseAuditID(args[0])
			if err != nil {
				return err
			}
			stream, _ := cmd.Flags().GetBool("stream")
			outFile, _ := cmd.Flags().GetString("output-file")

			st, err := openStore(s)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			assets, err := st.AuditAssets(cmd.Context(), id)
			if err != nil {
				return err
			}

			w, closeOut, err := openOutput(outFile)
			if err != nil {
				return err
			}
			defer closeOut()

			renderer := &output.JSON{Stream: stream}
			return renderer.Render(cmd.Context(), assetChan(cmd.Context(), assets), w)
		},
	}
	cmd.Flags().Bool("stream", false, "emit NDJSON (one asset per line) instead of a JSON array")
	cmd.Flags().String("output-file", "", "write output to this file instead of stdout")
	return cmd
}

func cacheRmCmd(s *cliState) *cobra.Command {
	return &cobra.Command{
		Use:     "rm ID",
		Aliases: []string{"remove", "delete"},
		Short:   "Delete one cached audit snapshot",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseAuditID(args[0])
			if err != nil {
				return err
			}
			st, err := openStore(s)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			found, err := st.DeleteAudit(cmd.Context(), id)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("audit %d not found", id)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted audit %d.\n", id)
			return nil
		},
	}
}

func cachePruneCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete cached snapshots a retention policy would not keep",
		Long: `Applies a retention policy to the stored snapshots.

  --max-age D   delete snapshots older than D
  --keep N      keep only the N newest snapshots PER PROVIDER SET
  (neither)     apply the configured --cache-retain / --cache-retain-age

--keep counts within a provider set because each set is an independent
series: an hourly "--provider netbird" run would otherwise evict every weekly
full audit long before N of them had accumulated. Given both, a snapshot has
to survive both.

Run it with --dry-run first. A snapshot is the only record of what the estate
looked like that day and nothing can recompute it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			policy, err := prunePolicy(cmd, s)
			if err != nil {
				return err
			}
			dryRun, _ := cmd.Flags().GetBool("dry-run")

			st, err := openStore(s)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			doomed, err := st.AuditsToPrune(cmd.Context(), policy, time.Now())
			if err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if len(doomed) == 0 {
				fmt.Fprintf(w, "Nothing to prune (%s).\n", policy.String())
				return nil
			}

			if dryRun {
				fmt.Fprintf(w, "Would prune %d audit snapshot(s) (%s):\n", len(doomed), policy.String())
				writeAuditRows(w, doomed)
				fmt.Fprintln(w, "\nNothing was deleted (--dry-run).")
				return nil
			}

			// Re-derive inside ApplyRetention rather than deleting the ids
			// listed above: the preview and the delete then share one
			// implementation, so they cannot drift apart.
			removed, err := st.ApplyRetention(cmd.Context(), policy, time.Now())
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "Pruned %d audit snapshot(s).\n", len(removed))
			writeAuditRows(w, removed)
			return nil
		},
	}
	cmd.Flags().Duration("max-age", 0, "delete snapshots older than this (e.g. 24h, 168h)")
	cmd.Flags().Int("keep", 0, "keep only this many of the newest snapshots per provider set")
	cmd.Flags().Bool("dry-run", false, "list what would be deleted and delete nothing")
	return cmd
}

// writeAuditRows prints the snapshot table shared by prune's preview and its
// report. Naming the casualties matters more than counting them: "pruned 40"
// is not something anyone can check afterwards.
func writeAuditRows(w io.Writer, audits []store.AuditMeta) {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tWHEN\tAGE\tPROVIDERS\tASSETS")
	for _, a := range audits {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\n",
			a.ID, a.RunAt.UTC().Format(time.RFC3339),
			time.Since(a.RunAt).Truncate(time.Second),
			strings.Join(a.Providers, ","), a.AssetCount)
	}
	_ = tw.Flush()
}

// prunePolicy resolves prune's flags into a policy, falling back to the
// configured standing policy when the command was given neither knob. It
// refuses to run with no policy at all rather than defaulting to something:
// "prune" with an implied window would delete history nobody asked it to.
func prunePolicy(cmd *cobra.Command, s *cliState) (store.RetentionPolicy, error) {
	maxAge, _ := cmd.Flags().GetDuration("max-age")
	keep, _ := cmd.Flags().GetInt("keep")

	if cmd.Flags().Changed("max-age") && maxAge <= 0 {
		return store.RetentionPolicy{}, errors.New("--max-age must be a positive duration (e.g. 24h)")
	}
	if cmd.Flags().Changed("keep") && keep <= 0 {
		return store.RetentionPolicy{}, errors.New("--keep must be at least 1 (use `auditor cache clear` to remove every snapshot)")
	}

	policy := store.RetentionPolicy{KeepLast: keep, MaxAge: maxAge}
	if policy.Empty() {
		policy = retentionFromViper(s.v)
	}
	if policy.Empty() {
		return store.RetentionPolicy{}, errors.New(
			"no retention policy to apply: pass --max-age or --keep, or configure one with " +
				"--cache-retain / --cache-retain-age (env AUDITOR_CACHE_RETAIN / AUDITOR_CACHE_RETAIN_AGE)")
	}
	return policy, nil
}

// humanBytes renders a byte count at a glance. Binary units, because that is
// what the filesystem tools the user will cross-check with report.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func cacheClearCmd(s *cliState) *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Delete all cached audit snapshots (the secrets vault is untouched)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore(s)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			n, err := st.ClearAudits(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Cleared %d audit snapshot(s).\n", n)
			return nil
		},
	}
}

// parseAuditID parses the ID positional argument shared by show and rm.
func parseAuditID(arg string) (int64, error) {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid audit id %q (expected a number from `auditor cache list`)", arg)
	}
	return id, nil
}
