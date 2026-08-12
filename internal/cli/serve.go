package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/server"
)

// nb: the listen-address banner uses slog at INFO via the cliState
// logger so it flows through the operator's log pipeline (JSON in prod,
// text on laptops) instead of bypassing it via Fprintf.

func newServeCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the web UI + JSON/SSE API.",
		Long: `Starts the audit web UI on --addr.

Auth modes:
  none   (default) - everything open; put behind a real reverse proxy in prod
  basic  - requires AUDITOR_BASIC_USER and AUDITOR_BASIC_PASS env vars
  token  - requires AUDITOR_API_TOKEN env var; client sends ` + "`Authorization: Bearer <token>`" + `

Provider credentials are read from the operator's environment at server
startup (CLOUDFLARE_API_TOKEN, ~/.oci/config, ~/.kube/config, etc.) — the
browser never sees them. The frontend can choose which registered
providers to run via the API but cannot supply new credentials.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := s.v.BindPFlags(cmd.Flags()); err != nil {
				return fmt.Errorf("bind flags: %w", err)
			}
			v := s.v

			cfg := server.Config{
				Addr:           v.GetString("addr"),
				AuthMode:       v.GetString("auth"),
				BasicUser:      os.Getenv("AUDITOR_BASIC_USER"),
				BasicPass:      os.Getenv("AUDITOR_BASIC_PASS"),
				APIToken:       os.Getenv("AUDITOR_API_TOKEN"),
				MaxConcurrency: v.GetInt("max-concurrency"),
				IncludeRaw:     v.GetBool("include-raw"),
				Providers:      serveProviders(v.GetStringSlice("provider")),
			}

			srv, err := server.New(cfg)
			if err != nil {
				return err
			}

			s.logger.Info("auditor serve listening",
				"addr", cfg.Addr,
				"auth", cfg.AuthMode,
				"max_concurrency", cfg.MaxConcurrency)

			return srv.Run(cmd.Context())
		},
	}
	cmd.Flags().String("addr", ":8080", "address to listen on")
	cmd.Flags().String("auth", "none", "auth mode: none|basic|token")
	cmd.Flags().StringSlice("provider", nil,
		"scope requests that name no providers to this set (default: all registered; --demo implies demo)")
	cmd.Flags().Int("max-concurrency", 5, "per-provider parallelism (mirrors `audit --max-concurrency`)")
	cmd.Flags().Bool("include-raw", false, "include full provider payload in Asset.Raw for both SSE and export")
	return cmd
}

// serveProviders decides the default provider scope for API requests that
// don't name one.
//
// The demo case is the reason this exists. `serve --demo` on a laptop that
// also has live Cloudflare or kube credentials would otherwise blend
// fabricated assets into a real inventory the moment the browser omitted the
// parameter — and nothing downstream of that point marks which assets were
// invented. An explicit --provider still wins, so `serve --demo --provider
// demo,kubernetes` remains a way to show the demo next to a real cluster.
func serveProviders(flag []string) []string {
	if len(flag) > 0 {
		return flag
	}
	if demoMode.Load() {
		return []string{demoProviderName}
	}
	return nil
}
