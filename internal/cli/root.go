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
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/config"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/logging"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/telemetry"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/version"
)

const telemetryShutdownGrace = 5 * time.Second

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

			// Bind --tracing too so AUDITOR_TRACING / config-file keys
			// take effect, then install the OTel TracerProvider. Off
			// mode is the default and pays zero overhead.
			if err := v.BindPFlag("tracing", cmd.Root().PersistentFlags().Lookup("tracing")); err != nil {
				return fmt.Errorf("bind tracing: %w", err)
			}
			if err := telemetry.Setup(cmd.Context(), telemetry.Options{
				Mode:           v.GetString("tracing"),
				ServiceVersion: version.Get().Version,
			}); err != nil {
				return err
			}
			return nil
		},
	}
	cmd.PersistentFlags().StringVar(&state.cfgFile, "config", "", "path to config file")
	cmd.PersistentFlags().String("log-level", "info", "log level: debug|info|warn|error")
	cmd.PersistentFlags().String("log-format", "text", "log format: text|json")
	cmd.PersistentFlags().String("tracing", "off", "tracing mode: off|stdout|otlp (honors OTEL_EXPORTER_OTLP_* env vars)")

	cmd.AddCommand(newAuditCmd(state))
	cmd.AddCommand(newServeCmd(state))
	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newProvidersCmd(state))
	cmd.AddCommand(newTopologyCmd(state))
	cmd.AddCommand(newDiffCmd())
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

	// Flush any pending OTel spans before the process exits. No-op when
	// --tracing=off; bounded so a wedged exporter can't hang the binary.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), telemetryShutdownGrace)
		defer cancel()
		if err := telemetry.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintln(os.Stderr, "warning: telemetry shutdown:", err)
		}
	}()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		if errors.Is(err, ErrPartial) {
			return 2
		}
		return 1
	}
	return 0
}
