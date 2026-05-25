package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/version"
)

func newVersionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build version information.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := version.Get()
			asJSON, _ := cmd.Flags().GetBool("json")
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetEscapeHTML(false)
				return enc.Encode(info)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"auditor %s (commit %s, built %s, %s)\n",
				info.Version, info.Commit, info.Date, info.GoVersion)
			return err
		},
	}
	cmd.Flags().Bool("json", false, "emit JSON instead of human-readable text")
	return cmd
}
