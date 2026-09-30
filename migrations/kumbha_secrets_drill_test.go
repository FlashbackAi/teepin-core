// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Drill for migration 064 (billing.kumbha_session_secrets), against a
// disposable Postgres; behind the build tag so it never runs in CI or a normal
// `go test ./...`:
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55444/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestKumbhaSecretsDrill ./migrations/ -v
package migrations

import (
	"database/sql"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/lib/pq"
)

func TestKumbhaSecretsDrill(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !tableExists(t, db, "billing", "kumbha_session_secrets") {
		t.Fatal("after up: billing.kumbha_session_secrets missing")
	}

	// A real session to hang secrets on: account, user, project, session.
	var accountID, userID, projectID, sessionID string
	if err := db.QueryRow(`
		INSERT INTO auth.accounts (account_number, alias, type, display_name)
		VALUES ('drill-064', 'drill-064', 'organization', 'drill') RETURNING id
	`).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := db.QueryRow(`
		INSERT INTO auth.users (email, password_hash, account_id)
		VALUES ('drill064@example.com', 'x', $1) RETURNING id
	`, accountID).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.QueryRow(`
		INSERT INTO auth.projects (owner_id, name, slug, account_id)
		VALUES ($1, 'drill', 'drill-064', $2) RETURNING id
	`, userID, accountID).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := db.QueryRow(`
		INSERT INTO billing.inference_sessions (account_id, project_id, budget)
		VALUES ($1, $2, 5) RETURNING id
	`, accountID, projectID).Scan(&sessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// One row per (session, name); saving again replaces, it does not add.
	upsert := `
		INSERT INTO billing.kumbha_session_secrets (session_id, name, sealed)
		VALUES ($1, $2, $3)
		ON CONFLICT (session_id, name) DO UPDATE SET sealed = EXCLUDED.sealed, updated_at = NOW()`
	for _, sealed := range [][]byte{[]byte("first"), []byte("second")} {
		if _, err := db.Exec(upsert, sessionID, "API_KEY", sealed); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	if _, err := db.Exec(upsert, sessionID, "OTHER_KEY", []byte("x")); err != nil {
		t.Fatalf("second name: %v", err)
	}
	var n int
	var sealed []byte
	if err := db.QueryRow(`SELECT COUNT(*) FROM billing.kumbha_session_secrets WHERE session_id = $1`, sessionID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows = %d (err %v), want 2: an upsert must replace, not add", n, err)
	}
	if err := db.QueryRow(`SELECT sealed FROM billing.kumbha_session_secrets WHERE session_id = $1 AND name = 'API_KEY'`, sessionID).Scan(&sealed); err != nil || string(sealed) != "second" {
		t.Fatalf("sealed = %q (err %v), want the replacement value", sealed, err)
	}

	// A secret cannot exist without its session.
	if _, err := db.Exec(upsert, "00000000-0000-0000-0000-000000000000", "API_KEY", []byte("x")); err == nil {
		t.Error("a secret was stored for a session that does not exist")
	}

	// Deleting the session deletes its secrets.
	if _, err := db.Exec(`DELETE FROM billing.inference_sessions WHERE id = $1`, sessionID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM billing.kumbha_session_secrets WHERE session_id = $1`, sessionID).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows after session delete = %d (err %v), want 0: secrets must not outlive their session", n, err)
	}

	// Down one: the table goes, the rest of the schema stays.
	if err := migrator(t, db).Migrate(63); err != nil {
		t.Fatalf("migrate down to 063: %v", err)
	}
	if tableExists(t, db, "billing", "kumbha_session_secrets") {
		t.Error("after down: billing.kumbha_session_secrets still present")
	}
	if !tableExists(t, db, "billing", "inference_sessions") {
		t.Error("after down: billing.inference_sessions was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	if !tableExists(t, db, "billing", "kumbha_session_secrets") {
		t.Error("after re-up: billing.kumbha_session_secrets missing")
	}
	t.Log("drill passed: 064 applies, behaves as designed, reverts cleanly, and re-applies")
}
