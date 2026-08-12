package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/store"
)

// newSecretsCmd is the `auditor secrets` group: an encrypted, SQLite-backed
// vault for provider credentials. Secrets are keyed by the ENV VAR NAME the
// provider reads (e.g. NETBIRD_API_TOKEN), so a stored secret is loaded into
// the environment at startup and picked up transparently — no env export
// needed on each run. Encryption is AES-256-GCM with a scrypt-derived key; the
// passphrase comes from --passphrase or $AUDITOR_SECRETS_PASSPHRASE.
func newSecretsCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage provider credentials in the encrypted local vault",
		Long: `Store provider credentials encrypted at rest in the SQLite database
(--db), so you don't re-export them every run.

Secrets are keyed by the environment variable the provider reads, e.g.:

  export AUDITOR_SECRETS_PASSPHRASE=...           # the vault passphrase
  auditor secrets set NETBIRD_API_TOKEN nbp_xxx   # or omit the value to be prompted
  auditor audit --provider netbird                # token loaded from the vault

Encryption is AES-256-GCM with a scrypt-derived key. The passphrase is never
stored; an explicit environment variable always overrides a vaulted value.`,
	}
	cmd.PersistentFlags().String("passphrase", "",
		"vault passphrase (default: $AUDITOR_SECRETS_PASSPHRASE)")
	cmd.AddCommand(secretsSetCmd(s), secretsGetCmd(s), secretsListCmd(s), secretsRmCmd(s))
	return cmd
}

func secretsSetCmd(s *cliState) *cobra.Command {
	return &cobra.Command{
		Use:   "set NAME [VALUE]",
		Short: "Store or replace a secret (reads VALUE from stdin if omitted)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			pass := passphraseFor(cmd)
			if pass == "" {
				return store.ErrNoPassphrase
			}
			var value string
			if len(args) == 2 {
				value = args[1]
			} else {
				v, err := readSecretValue(name)
				if err != nil {
					return err
				}
				value = v
			}
			if value == "" {
				return fmt.Errorf("secrets set: empty value for %q", name)
			}
			st, err := openStore(s)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			if err := st.SetSecret(cmd.Context(), name, value, pass); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stored secret %q\n", name)
			return nil
		},
	}
}

func secretsGetCmd(s *cliState) *cobra.Command {
	return &cobra.Command{
		Use:   "get NAME",
		Short: "Print a decrypted secret to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pass := passphraseFor(cmd)
			if pass == "" {
				return store.ErrNoPassphrase
			}
			st, err := openStore(s)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			val, err := st.GetSecret(cmd.Context(), args[0], pass)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), val)
			return nil
		},
	}
}

func secretsListCmd(s *cliState) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List stored secret names (never the values)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore(s)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			names, err := st.ListSecretNames(cmd.Context())
			if err != nil {
				return err
			}
			for _, n := range names {
				fmt.Fprintln(cmd.OutOrStdout(), n)
			}
			return nil
		},
	}
}

func secretsRmCmd(s *cliState) *cobra.Command {
	return &cobra.Command{
		Use:     "rm NAME",
		Aliases: []string{"remove", "delete"},
		Short:   "Delete a stored secret",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore(s)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			found, err := st.DeleteSecret(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if !found {
				return store.ErrSecretNotFound
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted secret %q\n", args[0])
			return nil
		},
	}
}

// openStore opens the store at the resolved --db path.
func openStore(s *cliState) (*store.Store, error) {
	return store.Open(s.v.GetString("db"))
}

// passphraseFor resolves the vault passphrase: --passphrase wins, else the
// AUDITOR_SECRETS_PASSPHRASE env var.
func passphraseFor(cmd *cobra.Command) string {
	if p, _ := cmd.Flags().GetString("passphrase"); p != "" {
		return p
	}
	return os.Getenv("AUDITOR_SECRETS_PASSPHRASE")
}

// readSecretValue reads a secret value: a no-echo prompt on a terminal, or the
// whole of stdin (trailing newline trimmed) when piped.
func readSecretValue(name string) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(os.Stderr, "Enter value for %s: ", name)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}
