// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newSecretStore(t *testing.T) (*Store, sqlmock.Sqlmock, *SecretVault) {
	t.Helper()
	store, mock := newMockStore(t)
	vault, err := NewSecretVault("test-encryption-key")
	if err != nil {
		t.Fatalf("NewSecretVault: %v", err)
	}
	return store.WithSecretVault(vault), mock, vault
}

func TestSecretVault_RoundTrips(t *testing.T) {
	v, _ := NewSecretVault("k")
	sid := uuid.New()
	sealed, err := v.Seal(sid, "API_KEY", "s3cr3t-value")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("s3cr3t-value")) {
		t.Fatal("the sealed form contains the plaintext")
	}
	got, err := v.Open(sid, "API_KEY", sealed)
	if err != nil || got != "s3cr3t-value" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

// A stored value copied onto another session's row, or another variable's
// name, must not decrypt: that is what the additional data binds.
func TestSecretVault_IsBoundToSessionAndName(t *testing.T) {
	v, _ := NewSecretVault("k")
	sid := uuid.New()
	sealed, _ := v.Seal(sid, "API_KEY", "value")
	if _, err := v.Open(uuid.New(), "API_KEY", sealed); err == nil {
		t.Error("opened under a different session")
	}
	if _, err := v.Open(sid, "OTHER_NAME", sealed); err == nil {
		t.Error("opened under a different name")
	}
	other, _ := NewSecretVault("a-different-key")
	if _, err := other.Open(sid, "API_KEY", sealed); err == nil {
		t.Error("opened under a different key")
	}
	if _, err := v.Open(sid, "API_KEY", []byte("short")); err == nil {
		t.Error("opened a truncated value")
	}
}

func TestSecretVault_RequiresAKey(t *testing.T) {
	if _, err := NewSecretVault(""); err == nil {
		t.Error("an empty key must be refused")
	}
}

func TestStore_SaveSecret_StoresOnlyTheSealedValue(t *testing.T) {
	store, mock, vault := newSecretStore(t)
	sid, aid := uuid.New(), uuid.New()

	mock.ExpectExec(`INSERT INTO billing\.kumbha_session_secrets`).
		WithArgs(sid, "API_KEY", sqlmock.AnyArg(), aid, MaxSecretsPerSession).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.SaveSecret(context.Background(), sid, aid, "API_KEY", "plain-value"); err != nil {
		t.Fatalf("SaveSecret: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
	_ = vault
}

// The plaintext must never be a query argument, only the sealed bytes.
type notPlaintext struct{ plain string }

func (n notPlaintext) Match(v driver.Value) bool {
	b, ok := v.([]byte)
	return ok && !bytes.Contains(b, []byte(n.plain)) && len(b) > len(n.plain)
}

func TestStore_SaveSecret_NeverSendsPlaintextToTheDatabase(t *testing.T) {
	store, mock, _ := newSecretStore(t)
	sid, aid := uuid.New(), uuid.New()
	mock.ExpectExec(`INSERT INTO billing\.kumbha_session_secrets`).
		WithArgs(sid, "API_KEY", notPlaintext{"plain-value"}, aid, MaxSecretsPerSession).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.SaveSecret(context.Background(), sid, aid, "API_KEY", "plain-value"); err != nil {
		t.Fatalf("SaveSecret: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the value reached the database unsealed: %v", err)
	}
}

func TestStore_SaveSecret_Validation(t *testing.T) {
	store, _, _ := newSecretStore(t)
	sid, aid := uuid.New(), uuid.New()
	cases := []struct{ name, key, val string }{
		{"lower-case name", "api_key", "v"},
		{"reserved prefix", "TEEPIN_SESSION_TOKEN", "v"},
		{"reserved name", "LD_PRELOAD", "v"},
		{"empty value", "API_KEY", ""},
		{"oversized value", "API_KEY", strings.Repeat("x", MaxSecretValueBytes+1)},
	}
	for _, tc := range cases {
		err := store.SaveSecret(context.Background(), sid, aid, tc.key, tc.val)
		if !errors.Is(err, ErrInvalidSecret) {
			t.Errorf("%s: err = %v, want ErrInvalidSecret", tc.name, err)
		}
	}
}

func TestStore_SaveSecret_NotConfigured(t *testing.T) {
	store, _ := newMockStore(t) // no vault
	err := store.SaveSecret(context.Background(), uuid.New(), uuid.New(), "API_KEY", "v")
	if !errors.Is(err, ErrSecretsNotConfigured) {
		t.Errorf("err = %v, want ErrSecretsNotConfigured", err)
	}
}

// Nothing written because the session belongs to someone else must look the
// same as a session that does not exist.
func TestStore_SaveSecret_OtherAccountsSessionIsNotFound(t *testing.T) {
	store, mock, _ := newSecretStore(t)
	sid, aid := uuid.New(), uuid.New()
	mock.ExpectExec(`INSERT INTO billing\.kumbha_session_secrets`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT EXISTS`).WithArgs(sid, aid).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	if err := store.SaveSecret(context.Background(), sid, aid, "API_KEY", "v"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestStore_SaveSecret_AtTheCapIsRefused(t *testing.T) {
	store, mock, _ := newSecretStore(t)
	sid, aid := uuid.New(), uuid.New()
	mock.ExpectExec(`INSERT INTO billing\.kumbha_session_secrets`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT EXISTS`).WithArgs(sid, aid).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	if err := store.SaveSecret(context.Background(), sid, aid, "API_KEY", "v"); !errors.Is(err, ErrTooManySecrets) {
		t.Errorf("err = %v, want ErrTooManySecrets", err)
	}
}

func TestStore_ListSecrets_ReturnsNamesOnly(t *testing.T) {
	store, mock, _ := newSecretStore(t)
	sid, aid := uuid.New(), uuid.New()
	now := time.Now()
	mock.ExpectQuery(`SELECT k\.name, k\.updated_at`).WithArgs(sid, aid).
		WillReturnRows(sqlmock.NewRows([]string{"name", "updated_at"}).AddRow("A_KEY", now).AddRow("B_KEY", now))
	got, err := store.ListSecrets(context.Background(), sid, aid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "A_KEY" || got[1].Name != "B_KEY" {
		t.Errorf("got %+v", got)
	}
}

func TestStore_SecretEnv_DecryptsForInjection(t *testing.T) {
	store, mock, vault := newSecretStore(t)
	sid, aid := uuid.New(), uuid.New()
	sealedA, _ := vault.Seal(sid, "A_KEY", "value-a")
	sealedB, _ := vault.Seal(sid, "B_KEY", "value-b")
	mock.ExpectQuery(`SELECT k\.name, k\.sealed`).WithArgs(sid, aid).
		WillReturnRows(sqlmock.NewRows([]string{"name", "sealed"}).AddRow("A_KEY", sealedA).AddRow("B_KEY", sealedB))
	env, err := store.SecretEnv(context.Background(), sid, aid)
	if err != nil {
		t.Fatal(err)
	}
	if env["A_KEY"] != "value-a" || env["B_KEY"] != "value-b" || len(env) != 2 {
		t.Errorf("env = %v", env)
	}
}

// A stored value that will not open must fail the deploy, not vanish.
func TestStore_SecretEnv_UnreadableSecretIsAnError(t *testing.T) {
	store, mock, vault := newSecretStore(t)
	sid, aid := uuid.New(), uuid.New()
	sealedForOtherSession, _ := vault.Seal(uuid.New(), "A_KEY", "value")
	mock.ExpectQuery(`SELECT k\.name, k\.sealed`).WithArgs(sid, aid).
		WillReturnRows(sqlmock.NewRows([]string{"name", "sealed"}).AddRow("A_KEY", sealedForOtherSession))
	if _, err := store.SecretEnv(context.Background(), sid, aid); err == nil {
		t.Error("an unreadable secret was silently skipped")
	}
}

func TestStore_SecretEnv_NoVaultMeansNoSecrets(t *testing.T) {
	store, _ := newMockStore(t)
	env, err := store.SecretEnv(context.Background(), uuid.New(), uuid.New())
	if err != nil || len(env) != 0 {
		t.Errorf("env = %v, err = %v", env, err)
	}
}
