// Package cli wires the cobra command tree for the auditor binary.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/config"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/logging"
)

// cliState carries cross-cutting state between cobra commands. It's
// populated by PersistentPreRunE on the root command and read inside each
// subcommand's RunE.
type cliState struct {
	cfgFile string
	v       *viper.Viper
	logger  *slog.Logger
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
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			v, err := config.Init(state.cfgFile)
			if err != nil {
				return err
			}
			state.v = v

			// Bind the persistent log flags to viper *before* reading
			// them so AUDITOR_LOG_LEVEL / AUDITOR_LOG_FORMAT env vars
			// and config-file keys take effect.
			if err := v.BindPFlag("log-level", cmd.Root().PersistentFlags().Lookup("log-level")); err != nil {
				return fmt.Errorf("bind log-level: %w", err)
			}
			if err := v.BindPFlag("log-format", cmd.Root().PersistentFlags().Lookup("log-format")); err != nil {
				return fmt.Errorf("bind log-format: %w", err)
			}

			logger, err := logging.New(logging.Options{
				Level:  v.GetString("log-level"),
				Format: v.GetString("log-format"),
			})
			if err != nil {
				return err
			}
			state.logger = logger
			logging.SetDefault(logger)
			return nil
		},
	}
	cmd.PersistentFlags().StringVar(&state.cfgFile, "config", "", "path to config file")
	cmd.PersistentFlags().String("log-level", "info", "log level: debug|info|warn|error")
	cmd.PersistentFlags().String("log-format", "text", "log format: text|json")

	cmd.AddCommand(newAuditCmd(state))
	cmd.AddCommand(newServeCmd(state))
	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newProvidersCmd(state))
	cmd.AddCommand(newTopologyCmd(state))
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
