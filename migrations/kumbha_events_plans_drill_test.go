// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Drill for migration 068 (kumbha_events, kumbha_plans, agent_launch_seq,
// approved_plan_id), against a disposable Postgres; behind the build tag so it
// never runs in CI or a normal `go test ./...`:
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55444/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestKumbhaEventsPlansDrill ./migrations/ -v
package migrations

import (
	"database/sql"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/lib/pq"
)

func TestKumbhaEventsPlansDrill(t *testing.T) {
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
	for _, table := range []string{"kumbha_events", "kumbha_plans"} {
		if !tableExists(t, db, "billing", table) {
			t.Fatalf("after up: billing.%s missing", table)
		}
	}
	for _, col := range []string{"agent_launch_seq", "approved_plan_id"} {
		if !columnExists(t, db, "billing", "inference_sessions", col) {
			t.Fatalf("after up: inference_sessions.%s missing", col)
		}
	}

	// A session with a plan and an event.
	var accountID, userID, projectID, sessionID, planID string
	if err := db.QueryRow(`INSERT INTO auth.accounts (account_number, alias, type, display_name)
		VALUES ('drill-068', 'drill-068', 'organization', 'drill') RETURNING id`).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := db.QueryRow(`INSERT INTO auth.users (email, password_hash, account_id)
		VALUES ('drill068@example.com', 'x', $1) RETURNING id`, accountID).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.QueryRow(`INSERT INTO auth.projects (owner_id, name, slug, account_id)
		VALUES ($1, 'drill', 'drill-068', $2) RETURNING id`, userID, accountID).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := db.QueryRow(`INSERT INTO billing.inference_sessions (account_id, project_id, budget)
		VALUES ($1, $2, 5) RETURNING id`, accountID, projectID).Scan(&sessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	var seq int
	if err := db.QueryRow(`SELECT agent_launch_seq FROM billing.inference_sessions WHERE id = $1`, sessionID).Scan(&seq); err != nil || seq != 0 {
		t.Fatalf("a new session's agent_launch_seq = %d (err %v), want 0", seq, err)
	}
	if err := db.QueryRow(`INSERT INTO billing.kumbha_plans (session_id, resources)
		VALUES ($1, '[{"name":"app","cpu_units":1,"memory_gb":1,"storage_gb":0}]') RETURNING id`, sessionID).Scan(&planID); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if _, err := db.Exec(`UPDATE billing.inference_sessions SET approved_plan_id = $2 WHERE id = $1`, sessionID, planID); err != nil {
		t.Fatalf("bind plan: %v", err)
	}

	// The same (session, launch, line) cannot be stored twice.
	ins := `INSERT INTO billing.kumbha_events (session_id, launch_seq, line_no, payload) VALUES ($1, 1, 1, '{"type":"idle"}')`
	if _, err := db.Exec(ins, sessionID); err != nil {
		t.Fatalf("first event: %v", err)
	}
	if _, err := db.Exec(ins, sessionID); err == nil {
		t.Error("the same event line was stored twice")
	}

	// A plan cannot be bound to a session that does not exist, and deleting the
	// plan unbinds it rather than blocking.
	if _, err := db.Exec(`DELETE FROM billing.kumbha_plans WHERE id = $1`, planID); err != nil {
		t.Fatalf("delete plan: %v", err)
	}
	var bound sql.NullString
	if err := db.QueryRow(`SELECT approved_plan_id FROM billing.inference_sessions WHERE id = $1`, sessionID).Scan(&bound); err != nil || bound.Valid {
		t.Errorf("approved_plan_id after the plan was deleted = %v (err %v), want NULL", bound, err)
	}

	// Deleting the session removes its events and plans.
	if _, err := db.Exec(`DELETE FROM billing.inference_sessions WHERE id = $1`, sessionID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM billing.kumbha_events WHERE session_id = $1`, sessionID).Scan(&n); err != nil || n != 0 {
		t.Errorf("events after session delete = %d (err %v), want 0", n, err)
	}

	// Down one: the new tables and columns go, the rest stays.
	if err := migrator(t, db).Migrate(67); err != nil {
		t.Fatalf("migrate down to 067: %v", err)
	}
	for _, table := range []string{"kumbha_events", "kumbha_plans"} {
		if tableExists(t, db, "billing", table) {
			t.Errorf("after down: billing.%s still present", table)
		}
	}
	for _, col := range []string{"agent_launch_seq", "approved_plan_id"} {
		if columnExists(t, db, "billing", "inference_sessions", col) {
			t.Errorf("after down: inference_sessions.%s still present", col)
		}
	}
	if !tableExists(t, db, "billing", "inference_sessions") {
		t.Error("after down: billing.inference_sessions was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	if !tableExists(t, db, "billing", "kumbha_events") || !columnExists(t, db, "billing", "inference_sessions", "approved_plan_id") {
		t.Error("after re-up: 068 objects missing")
	}
	t.Log("drill passed: 068 applies, behaves as designed, reverts cleanly, and re-applies")
}
