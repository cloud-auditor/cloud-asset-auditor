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
	"github.com/cloud-auditor/cloud-asset-auditor/internal/providers/demo"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/store"
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

			// Load any vaulted provider credentials into the environment so
			// the provider factories (which read os.Getenv) pick them up. Best
			// effort: a passphrase problem must not block commands that don't
			// need the vault, so it only warns.
			if err := v.BindPFlag("db", cmd.Root().PersistentFlags().Lookup("db")); err != nil {
				return fmt.Errorf("bind db: %w", err)
			}
			loadVaultedSecrets(cmd.Context(), v.GetString("db"))

			// Retention is a property of the database, not of one command:
			// `audit --cache` enforces it on write and `cache prune` applies
			// it on demand, so it binds here with --db rather than on audit.
			for _, name := range []string{"cache-retain", "cache-retain-age"} {
				if err := v.BindPFlag(name, cmd.Root().PersistentFlags().Lookup(name)); err != nil {
					return fmt.Errorf("bind %s: %w", name, err)
				}
			}
			setCacheRetention(retentionFromViper(v))

			// --demo installs the built-in synthetic provider. It is
			// registered here rather than from an init() so "demo" never
			// appears in core.Registered() on a normal run — fabricated
			// assets must not be one mistyped flag away from a real audit.
			if err := v.BindPFlag("demo", cmd.Root().PersistentFlags().Lookup("demo")); err != nil {
				return fmt.Errorf("bind demo: %w", err)
			}
			if v.GetBool("demo") {
				demo.Register()
				setDemoMode(true)
			}
			return nil
		},
	}
	cmd.PersistentFlags().StringVar(&state.cfgFile, "config", "", "path to config file")
	cmd.PersistentFlags().String("log-level", "info", "log level: debug|info|warn|error")
	cmd.PersistentFlags().String("log-format", "text", "log format: text|json")
	cmd.PersistentFlags().String("tracing", "off", "tracing mode: off|stdout|otlp (honors OTEL_EXPORTER_OTLP_* env vars)")
	cmd.PersistentFlags().String("db", store.DefaultPath(),
		"SQLite database for the audit cache + secrets vault (env AUDITOR_DB)")
	// Both default to 0 = keep everything. Nothing deletes snapshot history
	// unless the operator asks: the database is its only copy, and a lost
	// baseline is only discovered when someone needs it.
	cmd.PersistentFlags().Int("cache-retain", 0,
		"keep at most this many cached snapshots per provider set, pruned after each --cache write (0 = keep every snapshot; env AUDITOR_CACHE_RETAIN)")
	cmd.PersistentFlags().Duration("cache-retain-age", 0,
		"delete cached snapshots older than this after each --cache write, e.g. 2160h for 90 days (0 = keep every snapshot; env AUDITOR_CACHE_RETAIN_AGE)")
	cmd.PersistentFlags().Bool("demo", false,
		"run against a built-in synthetic multi-cloud inventory instead of real providers — needs no credentials (env AUDITOR_DEMO)")

	cmd.AddCommand(newAuditCmd(state))
	cmd.AddCommand(newServeCmd(state))
	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newProvidersCmd(state))
	cmd.AddCommand(newTopologyCmd(state))
	cmd.AddCommand(newReachCmd(state))
	cmd.AddCommand(newDiffCmd(state))
	cmd.AddCommand(newHistoryCmd(state))
	cmd.AddCommand(newSecretsCmd(state))
	cmd.AddCommand(newCacheCmd(state))
	cmd.AddCommand(newCheckCmd(state))
	cmd.AddCommand(newCostCmd(state))
	return cmd
}

// loadVaultedSecrets decrypts stored secrets into the environment if the DB
// already exists and holds any. It never creates the DB (so `auditor version`
// stays side-effect free) and never fails the command — a missing or wrong
// passphrase only warns, because plenty of commands don't need the vault.
func loadVaultedSecrets(ctx context.Context, dbPath string) {
	if dbPath == "" {
		return
	}
	if _, err := os.Stat(dbPath); err != nil {
		return // no DB yet — nothing to load, and we won't create it here
	}
	st, err := store.Open(dbPath)
	if err != nil {
		slog.Warn("secrets vault: open failed", "error", err)
		return
	}
	defer func() { _ = st.Close() }()

	switch has, err := st.HasSecrets(ctx); {
	case err != nil:
		slog.Warn("secrets vault: lookup failed", "error", err)
		return
	case !has:
		return
	}

	pass := os.Getenv("AUDITOR_SECRETS_PASSPHRASE")
	if pass == "" {
		slog.Warn("secrets vault holds credentials but AUDITOR_SECRETS_PASSPHRASE is unset; not loading them")
		return
	}
	loaded, err := st.LoadSecretsIntoEnv(ctx, pass)
	if err != nil {
		slog.Warn("secrets vault: could not load credentials", "error", err)
		return
	}
	if len(loaded) > 0 {
		slog.Debug("loaded credentials from secrets vault", "count", len(loaded))
	}
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
