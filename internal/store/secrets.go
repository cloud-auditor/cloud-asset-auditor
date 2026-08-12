package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/crypto/scrypt"
)

// Secret-vault sentinels.
var (
	ErrNoPassphrase    = errors.New("store: a secrets passphrase is required (set AUDITOR_SECRETS_PASSPHRASE or pass --passphrase)")
	ErrSecretNotFound  = errors.New("store: secret not found")
	ErrWrongPassphrase = errors.New("store: could not decrypt secret — wrong passphrase, or the secret was written under a different one")
)

// scrypt cost parameters. N=2^15 is the interactive-login recommendation and
// is plenty for a CLI that decrypts a handful of secrets per run.
const (
	scryptN = 1 << 15
	scryptR = 8
	scryptP = 1
	keyLen  = 32 // AES-256
	saltLen = 16
)

// deriveKey turns a passphrase + per-secret salt into an AES-256 key. A fresh
// random salt per secret means two secrets with the same passphrase get
// independent keys.
func deriveKey(passphrase string, salt []byte) ([]byte, error) {
	return scrypt.Key([]byte(passphrase), salt, scryptN, scryptR, scryptP, keyLen)
}

// SetSecret stores (or replaces) an encrypted secret under name. The plaintext
// is sealed with AES-256-GCM; the secret's name is the GCM additional data, so
// a ciphertext can't be silently moved to a different name. The passphrase is
// never stored — only a random salt, nonce, and the ciphertext.
func (s *Store) SetSecret(ctx context.Context, name, value, passphrase string) error {
	if passphrase == "" {
		return ErrNoPassphrase
	}
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return err
	}
	gcm, err := newGCM(passphrase, salt)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	ciphertext := gcm.Seal(nil, nonce, []byte(value), []byte(name))

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO secrets(name,ciphertext,nonce,salt,updated_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET
		   ciphertext=excluded.ciphertext, nonce=excluded.nonce,
		   salt=excluded.salt, updated_at=excluded.updated_at`,
		name, ciphertext, nonce, salt, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: write secret: %w", err)
	}
	return nil
}

// GetSecret decrypts and returns the named secret. A passphrase that can't
// authenticate the ciphertext yields ErrWrongPassphrase (GCM open fails).
func (s *Store) GetSecret(ctx context.Context, name, passphrase string) (string, error) {
	if passphrase == "" {
		return "", ErrNoPassphrase
	}
	var ciphertext, nonce, salt []byte
	row := s.db.QueryRowContext(ctx, `SELECT ciphertext,nonce,salt FROM secrets WHERE name=?`, name)
	switch err := row.Scan(&ciphertext, &nonce, &salt); {
	case err == sql.ErrNoRows:
		return "", ErrSecretNotFound
	case err != nil:
		return "", err
	}
	gcm, err := newGCM(passphrase, salt)
	if err != nil {
		return "", err
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(name))
	if err != nil {
		return "", ErrWrongPassphrase
	}
	return string(plaintext), nil
}

// ListSecretNames returns the stored secret names (never the values).
func (s *Store) ListSecretNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM secrets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// DeleteSecret removes a secret; found reports whether a row existed.
func (s *Store) DeleteSecret(ctx context.Context, name string) (found bool, err error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE name=?`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// LoadSecretsIntoEnv decrypts every stored secret and exports it as an
// environment variable IF that variable isn't already set in the process. This
// is what lets the provider factories (which read os.Getenv) transparently
// pick up stored credentials. An explicit env var always wins over the vault.
// Returns the names that were loaded.
func (s *Store) LoadSecretsIntoEnv(ctx context.Context, passphrase string) ([]string, error) {
	names, err := s.ListSecretNames(ctx)
	if err != nil {
		return nil, err
	}
	var loaded []string
	for _, name := range names {
		if _, set := os.LookupEnv(name); set {
			continue // an explicit env var takes precedence over the vault
		}
		val, err := s.GetSecret(ctx, name, passphrase)
		if err != nil {
			return loaded, err
		}
		if err := os.Setenv(name, val); err != nil {
			return loaded, err
		}
		loaded = append(loaded, name)
	}
	return loaded, nil
}

// HasSecrets reports whether the vault holds any secrets — used to skip the
// passphrase requirement entirely when there's nothing to load.
func (s *Store) HasSecrets(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM secrets`).Scan(&n)
	return n > 0, err
}

func newGCM(passphrase string, salt []byte) (cipher.AEAD, error) {
	key, err := deriveKey(passphrase, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
