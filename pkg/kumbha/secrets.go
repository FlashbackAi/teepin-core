// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/envname"
)

// A secret is something the customer types into a secure field in the
// console (an API key, a database URL) for the app the agent is building.
// The build agent NEVER receives one: it can ask for a secret by name, and
// the control plane injects the stored value into the deployed app's
// environment at create/redeploy time. That is why nothing here returns a
// value except SecretEnv, whose only caller builds an instance spec.

var (
	// ErrSecretsNotConfigured means the deployment has no encryption key, so
	// secrets cannot be stored. Without one they must not be stored at all.
	ErrSecretsNotConfigured = errors.New("secret storage is not available on this deployment")
	// ErrInvalidSecret is a bad name or value; its message is safe to show
	// the customer.
	ErrInvalidSecret = errors.New("invalid secret")
	// ErrTooManySecrets means the session already holds MaxSecretsPerSession.
	ErrTooManySecrets = errors.New("this build already has the maximum number of saved secrets")
)

const (
	// MaxSecretValueBytes bounds one value. Generous for an API key, a
	// connection string or a PEM certificate, small enough that a session
	// cannot be used as general storage.
	MaxSecretValueBytes = 8 * 1024
	// MaxSecretsPerSession bounds how many distinct names one build holds.
	MaxSecretsPerSession = 50
)

// SecretVault seals secret values for storage: AES-256-GCM under a key
// derived from the platform encryption key with its own purpose label (the
// same raw key seals registry credentials and instance launch specs;
// deriving per purpose keeps the uses independent).
type SecretVault struct {
	aead cipher.AEAD
}

// NewSecretVault builds a vault from the platform encryption key. An empty
// key is an error.
func NewSecretVault(encryptionKey string) (*SecretVault, error) {
	if encryptionKey == "" {
		return nil, errors.New("an encryption key is required to store secrets")
	}
	sum := sha256.Sum256([]byte("teepin/kumbha-session-secret/v1\x00" + encryptionKey))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SecretVault{aead: aead}, nil
}

// aad binds a sealed value to its session and name, so a stored value cannot
// be copied onto another session's row or another variable's name.
func secretAAD(sessionID uuid.UUID, name string) []byte {
	return []byte(sessionID.String() + "\x00" + name)
}

// Seal encrypts value for (sessionID, name).
func (v *SecretVault) Seal(sessionID uuid.UUID, name, value string) ([]byte, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return v.aead.Seal(nonce, nonce, []byte(value), secretAAD(sessionID, name)), nil
}

// Open decrypts a value sealed for (sessionID, name). A wrong key, corrupted
// data or a value belonging to another row all fail.
func (v *SecretVault) Open(sessionID uuid.UUID, name string, sealed []byte) (string, error) {
	n := v.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("stored secret is corrupted")
	}
	plain, err := v.aead.Open(nil, sealed[:n], sealed[n:], secretAAD(sessionID, name))
	if err != nil {
		return "", fmt.Errorf("stored secret %q cannot be opened: %w", name, err)
	}
	return string(plain), nil
}

// WithSecretVault enables the secrets capability. Returns the same *Store for
// chaining, so existing NewStore call sites compile unchanged.
func (s *Store) WithSecretVault(v *SecretVault) *Store {
	s.vault = v
	return s
}

// SecretInfo is what may be shown about a saved secret: never its value.
type SecretInfo struct {
	Name      string
	UpdatedAt time.Time
}

// SaveSecret stores (or replaces) one secret for a session the account owns.
func (s *Store) SaveSecret(ctx context.Context, sessionID, accountID uuid.UUID, name, value string) error {
	if s.vault == nil {
		return ErrSecretsNotConfigured
	}
	if err := envname.Validate(name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSecret, err)
	}
	if value == "" {
		return fmt.Errorf("%w: the value is empty", ErrInvalidSecret)
	}
	if len(value) > MaxSecretValueBytes {
		return fmt.Errorf("%w: the value is longer than %d bytes", ErrInvalidSecret, MaxSecretValueBytes)
	}
	sealed, err := s.vault.Seal(sessionID, name, value)
	if err != nil {
		return fmt.Errorf("failed to seal secret: %w", err)
	}

	// One statement so the ownership check, the per-session cap and the
	// upsert cannot be separated by a concurrent request: it inserts only if
	// the session belongs to the account AND (the name already exists OR the
	// session is under the cap).
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO billing.kumbha_session_secrets (session_id, name, sealed)
		SELECT $1, $2, $3
		WHERE EXISTS (SELECT 1 FROM billing.inference_sessions WHERE id = $1 AND account_id = $4)
		  AND (
		        EXISTS (SELECT 1 FROM billing.kumbha_session_secrets WHERE session_id = $1 AND name = $2)
		     OR (SELECT COUNT(*) FROM billing.kumbha_session_secrets WHERE session_id = $1) < $5
		  )
		ON CONFLICT (session_id, name)
		DO UPDATE SET sealed = EXCLUDED.sealed, updated_at = NOW()
	`, sessionID, name, sealed, accountID, MaxSecretsPerSession)
	if err != nil {
		return fmt.Errorf("failed to save secret: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}

	// Nothing written: either the session is not this account's, or it is at
	// the cap. Tell them apart without leaking which sessions exist.
	var owned bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM billing.inference_sessions WHERE id = $1 AND account_id = $2)
	`, sessionID, accountID).Scan(&owned); err != nil {
		return fmt.Errorf("failed to save secret: %w", err)
	}
	if !owned {
		return ErrSessionNotFound
	}
	return ErrTooManySecrets
}

// ListSecrets returns the names (and when each was last saved) of a session's
// secrets, never their values.
func (s *Store) ListSecrets(ctx context.Context, sessionID, accountID uuid.UUID) ([]SecretInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT k.name, k.updated_at
		FROM billing.kumbha_session_secrets k
		JOIN billing.inference_sessions s ON s.id = k.session_id
		WHERE k.session_id = $1 AND s.account_id = $2
		ORDER BY k.name
	`, sessionID, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to list secrets: %w", err)
	}
	defer rows.Close()
	out := []SecretInfo{}
	for rows.Next() {
		var info SecretInfo
		if err := rows.Scan(&info.Name, &info.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to read secret: %w", err)
		}
		out = append(out, info)
	}
	return out, rows.Err()
}

// DeleteSecret removes one secret. Deleting a name that is not saved is not
// an error.
func (s *Store) DeleteSecret(ctx context.Context, sessionID, accountID uuid.UUID, name string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM billing.kumbha_session_secrets k
		USING billing.inference_sessions s
		WHERE k.session_id = s.id AND k.session_id = $1 AND s.account_id = $2 AND k.name = $3
	`, sessionID, accountID, name)
	if err != nil {
		return fmt.Errorf("failed to delete secret: %w", err)
	}
	return nil
}

// SecretEnv decrypts every secret of a session into an environment map for
// the app being deployed. It is the ONLY path a value leaves the store, and
// its one caller builds an instance spec: the values go to the customer's own
// container and nowhere else — never to the build agent, an API response or a
// log line.
//
// A secret that cannot be opened is an error rather than a silent omission:
// deploying an app without a credential the customer supplied would fail
// confusingly later, far from the cause.
func (s *Store) SecretEnv(ctx context.Context, sessionID, accountID uuid.UUID) (map[string]string, error) {
	if s.vault == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT k.name, k.sealed
		FROM billing.kumbha_session_secrets k
		JOIN billing.inference_sessions s ON s.id = k.session_id
		WHERE k.session_id = $1 AND s.account_id = $2
	`, sessionID, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to load secrets: %w", err)
	}
	defer rows.Close()
	env := map[string]string{}
	for rows.Next() {
		var name string
		var sealed []byte
		if err := rows.Scan(&name, &sealed); err != nil {
			return nil, fmt.Errorf("failed to read secret: %w", err)
		}
		value, err := s.vault.Open(sessionID, name, sealed)
		if err != nil {
			return nil, err
		}
		env[name] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return env, nil
}

// --- Gateway wrappers ---
//
// pkg/api talks to the Gateway, not the Store, everywhere else in this
// package, so the secrets methods follow suit.

func (g *Gateway) SaveSecret(ctx context.Context, sessionID, accountID uuid.UUID, name, value string) error {
	return g.store.SaveSecret(ctx, sessionID, accountID, name, value)
}

func (g *Gateway) ListSecrets(ctx context.Context, sessionID, accountID uuid.UUID) ([]SecretInfo, error) {
	return g.store.ListSecrets(ctx, sessionID, accountID)
}

func (g *Gateway) DeleteSecret(ctx context.Context, sessionID, accountID uuid.UUID, name string) error {
	return g.store.DeleteSecret(ctx, sessionID, accountID, name)
}

// SecretEnv returns the session's decrypted secrets for injection into a
// deployed app's environment. Empty (not an error) when the session has none
// or secret storage is not configured.
func (g *Gateway) SecretEnv(ctx context.Context, sessionID, accountID uuid.UUID) (map[string]string, error) {
	return g.store.SecretEnv(ctx, sessionID, accountID)
}
