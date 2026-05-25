package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func newServeCmd(_ *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the web UI (Phase 5 — not yet implemented).",
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New("serve: not yet implemented (Phase 5)")
		},
	}
	cmd.Flags().String("addr", ":8080", "address to listen on")
	cmd.Flags().String("auth", "none", "auth mode: none|basic|token")
	return cmd
}
