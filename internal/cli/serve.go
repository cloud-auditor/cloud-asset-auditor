package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/server"
)

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
			}

			srv, err := server.New(cfg)
			if err != nil {
				return err
			}

			fmt.Fprintf(cmd.ErrOrStderr(),
				"auditor serve: listening on %s (auth=%s)\n", cfg.Addr, cfg.AuthMode)

			return srv.Run(cmd.Context())
		},
	}
	cmd.Flags().String("addr", ":8080", "address to listen on")
	cmd.Flags().String("auth", "none", "auth mode: none|basic|token")
	cmd.Flags().Int("max-concurrency", 5, "per-provider parallelism (mirrors `audit --max-concurrency`)")
	cmd.Flags().Bool("include-raw", false, "include full provider payload in Asset.Raw for both SSE and export")
	return cmd
}
