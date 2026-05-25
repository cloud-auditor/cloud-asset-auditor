package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func newProvidersCmd(_ *cliState) *cobra.Command {
	return &cobra.Command{
		Use:   "providers",
		Short: "List registered providers.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			names := core.Registered()
			out := cmd.OutOrStdout()
			if len(names) == 0 {
				_, err := fmt.Fprintln(out, "(no providers registered)")
				return err
			}
			for _, n := range names {
				if _, err := fmt.Fprintln(out, n); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
