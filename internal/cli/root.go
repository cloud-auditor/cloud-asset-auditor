// Package cli wires the cobra command tree for the auditor binary.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/config"
)

// cliState carries cross-cutting state between cobra commands. It's
// populated by PersistentPreRunE on the root command and read inside each
// subcommand's RunE.
type cliState struct {
	cfgFile string
	v       *viper.Viper
}

// ErrPartial signals that one or more providers failed but the run
// continued. main() maps this to a distinct exit code (2) per init-plan.md §6.
var ErrPartial = errors.New("partial provider failure")

func newRootCmd() *cobra.Command {
	state := &cliState{}

	cmd := &cobra.Command{
		Use:           "auditor",
		Short:         "Inventory cloud assets across providers (OCI, Cloudflare, Kubernetes).",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			v, err := config.Init(state.cfgFile)
			if err != nil {
				return err
			}
			state.v = v
			return nil
		},
	}
	cmd.PersistentFlags().StringVar(&state.cfgFile, "config", "", "path to config file")

	cmd.AddCommand(newAuditCmd(state))
	cmd.AddCommand(newServeCmd(state))
	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newProvidersCmd(state))
	return cmd
}

// Execute runs the CLI and returns a process exit code.
//   - 0: success
//   - 1: any error
//   - 2: partial provider failure (some providers produced results, others
//     errored) — see init-plan.md §6 invariant 5.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		if errors.Is(err, ErrPartial) {
			return 2
		}
		return 1
	}
	return 0
}
