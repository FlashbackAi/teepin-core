// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs the secret store's SQL against a real Postgres, because sqlmock checks
// the shape of a query but not that the SQL works: this one statement carries
// the ownership check, the per-session cap and the upsert. Behind the same
// build tag as the migration drills so it never runs in the normal
// `go test ./...`:
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55444/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestSecretsIntegration ./pkg/kumbha/ -v
package kumbha

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	_ "github.com/lib/pq"

	"github.com/FlashbackAi/teepin-core/migrations"
)

func secretsIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the secrets integration test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	drv, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("up: %v", err)
	}
	return db
}

// seedSession creates an account, user, project and session, returning the
// account and session ids.
func seedSession(t *testing.T, db *sql.DB, tag string) (accountID, sessionID uuid.UUID) {
	t.Helper()
	var user, project string
	if err := db.QueryRow(`
		INSERT INTO auth.accounts (account_number, alias, type, display_name)
		VALUES ($1, $1, 'organization', $1) RETURNING id
	`, "sec-"+tag).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := db.QueryRow(`
		INSERT INTO auth.users (email, password_hash, account_id) VALUES ($1, 'x', $2) RETURNING id
	`, tag+"@example.com", accountID).Scan(&user); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.QueryRow(`
		INSERT INTO auth.projects (owner_id, name, slug, account_id) VALUES ($1, $2, $2, $3) RETURNING id
	`, user, "p-"+tag, accountID).Scan(&project); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := db.QueryRow(`
		INSERT INTO billing.inference_sessions (account_id, project_id, budget) VALUES ($1, $2, 5) RETURNING id
	`, accountID, project).Scan(&sessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return accountID, sessionID
}

func TestSecretsIntegration(t *testing.T) {
	db := secretsIntegrationDB(t)
	ctx := context.Background()
	vault, err := NewSecretVault("integration-key")
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db).WithSecretVault(vault)

	acct, sess := seedSession(t, db, uuid.NewString()[:8])
	otherAcct, otherSess := seedSession(t, db, uuid.NewString()[:8])

	// Save, then replace: still one row, latest value wins.
	if err := store.SaveSecret(ctx, sess, acct, "API_KEY", "first"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.SaveSecret(ctx, sess, acct, "API_KEY", "second"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := store.SaveSecret(ctx, sess, acct, "DB_URL", "postgres://u:p@h/db"); err != nil {
		t.Fatalf("second name: %v", err)
	}

	// What is stored is sealed, not the plaintext.
	var raw []byte
	if err := db.QueryRow(`SELECT sealed FROM billing.kumbha_session_secrets WHERE session_id=$1 AND name='API_KEY'`, sess).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if string(raw) == "second" || len(raw) <= len("second") {
		t.Errorf("the stored value is not sealed: %q", raw)
	}

	infos, err := store.ListSecrets(ctx, sess, acct)
	if err != nil || len(infos) != 2 || infos[0].Name != "API_KEY" || infos[1].Name != "DB_URL" {
		t.Fatalf("list = %+v, %v", infos, err)
	}

	env, err := store.SecretEnv(ctx, sess, acct)
	if err != nil || env["API_KEY"] != "second" || env["DB_URL"] != "postgres://u:p@h/db" || len(env) != 2 {
		t.Fatalf("env = %v, %v", env, err)
	}

	// Another account can neither write to, read from, nor delete from this
	// session, and learns nothing: not-found, an empty list, no values.
	if err := store.SaveSecret(ctx, sess, otherAcct, "STOLEN", "x"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("another account's save: err = %v, want ErrSessionNotFound", err)
	}
	if got, _ := store.ListSecrets(ctx, sess, otherAcct); len(got) != 0 {
		t.Errorf("another account listed %v", got)
	}
	if got, _ := store.SecretEnv(ctx, sess, otherAcct); len(got) != 0 {
		t.Errorf("another account read %v", got)
	}
	if err := store.DeleteSecret(ctx, sess, otherAcct, "API_KEY"); err != nil {
		t.Fatalf("another account's delete should be a silent no-op: %v", err)
	}
	if got, _ := store.ListSecrets(ctx, sess, acct); len(got) != 2 {
		t.Errorf("another account's delete removed a secret: %v", got)
	}
	// The other session is unaffected and separate.
	if err := store.SaveSecret(ctx, otherSess, otherAcct, "API_KEY", "theirs"); err != nil {
		t.Fatalf("other session save: %v", err)
	}
	if env, _ := store.SecretEnv(ctx, sess, acct); env["API_KEY"] != "second" {
		t.Errorf("secrets leaked across sessions: %v", env)
	}

	// A value copied onto another session's row must not decrypt.
	if _, err := db.Exec(`
		UPDATE billing.kumbha_session_secrets
		SET sealed = (SELECT sealed FROM billing.kumbha_session_secrets WHERE session_id = $1 AND name = 'API_KEY')
		WHERE session_id = $2 AND name = 'API_KEY'`, sess, otherSess); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SecretEnv(ctx, otherSess, otherAcct); err == nil {
		t.Error("a value copied onto another session's row was accepted")
	}

	// The cap: fill to the limit, the next new name is refused, replacing an
	// existing name still works.
	capAcct, capSess := seedSession(t, db, uuid.NewString()[:8])
	for i := 0; i < MaxSecretsPerSession; i++ {
		if err := store.SaveSecret(ctx, capSess, capAcct, fmt.Sprintf("KEY_%d", i), "v"); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	if err := store.SaveSecret(ctx, capSess, capAcct, "ONE_TOO_MANY", "v"); !errors.Is(err, ErrTooManySecrets) {
		t.Errorf("over the cap: err = %v, want ErrTooManySecrets", err)
	}
	if err := store.SaveSecret(ctx, capSess, capAcct, "KEY_0", "replaced"); err != nil {
		t.Errorf("replacing an existing name at the cap must still work: %v", err)
	}

	// Delete, and deleting again is fine.
	if err := store.DeleteSecret(ctx, sess, acct, "API_KEY"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSecret(ctx, sess, acct, "API_KEY"); err != nil {
		t.Errorf("second delete: %v", err)
	}
	if got, _ := store.ListSecrets(ctx, sess, acct); len(got) != 1 || got[0].Name != "DB_URL" {
		t.Errorf("after delete: %v", got)
	}
}
