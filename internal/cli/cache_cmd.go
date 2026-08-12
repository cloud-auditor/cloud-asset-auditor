package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/output"
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

Examples:
  auditor cache list                       # every snapshot, newest first
  auditor cache show 3 > snapshot.json     # re-emit a snapshot as audit JSON
  auditor diff <(auditor cache show 3) <(auditor cache show 5)
  auditor cache rm 3                       # delete one snapshot
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
				return tw.Flush()
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
		Short: "Delete cached snapshots older than --max-age",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			maxAge, _ := cmd.Flags().GetDuration("max-age")
			if maxAge <= 0 {
				return errors.New("--max-age must be a positive duration (e.g. 24h)")
			}
			st, err := openStore(s)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			n, err := st.PruneAudits(cmd.Context(), time.Now().Add(-maxAge))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pruned %d audit snapshot(s).\n", n)
			return nil
		},
	}
	cmd.Flags().Duration("max-age", 0, "delete snapshots older than this (e.g. 24h, 168h)")
	_ = cmd.MarkFlagRequired("max-age")
	return cmd
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
