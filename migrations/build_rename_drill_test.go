// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Drill for migration 072 (Kumbha renamed to Teepin Build): every renamed
// table and column keeps its rows, recorded usage moves to the "build/" prefix,
// the attachments bucket keeps its objects, and the migration reverses cleanly.
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55433/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestBuildRenameDrill ./migrations/ -v
package migrations

import (
	"database/sql"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

func TestBuildRenameDrill(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Migrate(71); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 071: %v", err)
	}

	acct := seedAccount(t, db, "000000000072", "drill-build-rename")
	var user, project string
	if err := db.QueryRow(`INSERT INTO auth.users (email, password_hash, account_id) VALUES ('drill-072@example.com', 'x', $1) RETURNING id`, acct).Scan(&user); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.QueryRow(`INSERT INTO auth.projects (owner_id, name, slug, account_id) VALUES ($1, 'p', 'drill-072', $2) RETURNING id`, user, acct).Scan(&project); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	var session string
	if err := db.QueryRow(`INSERT INTO billing.inference_sessions (account_id, project_id, budget, label, model_route)
		VALUES ($1, $2, 10, 'drill', 'teepin/x') RETURNING id`, acct, project).Scan(&session); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	mustExec(t, db, `INSERT INTO billing.kumbha_messages (session_id, content) VALUES ($1, 'hello')`, session)
	mustExec(t, db, `INSERT INTO inference.models (model_route, display_name, cost_class, engine, kumbha_enabled, kumbha_priority)
		VALUES ('drill/m072', 'M', 'own', 'vllm', true, 3)`)
	mustExec(t, db, `INSERT INTO billing.usage_records (project_id, account_id, subject_type, subject_id, resource_type, quantity, unit, unit_price, total_cost, start_time, end_time)
		VALUES ($1, $2, 'inference_session', $3, 'kumbha/teepin/x:input', 1000, 'tokens', 2, 0.002, NOW(), NOW())`, project, acct, session)
	mustExec(t, db, `INSERT INTO billing.usage_records (project_id, account_id, subject_type, subject_id, resource_type, quantity, unit, unit_price, total_cost, start_time, end_time)
		VALUES ($1, $2, 'instance', 'i-1', 'cpu.home', 1, 'hours', 1, 1, NOW(), NOW())`, project, acct)
	mustExec(t, db, `INSERT INTO storage.buckets (id, account_id, project_id, name, backend) VALUES (gen_random_uuid(), $1, $2, 'kumbha-attachments', 'minio')`, acct, project)

	mustExec(t, db, `INSERT INTO inference.model_probe_reports (model_route, ran_at, report) VALUES ('drill/m072', NOW(),
		'{"model_route":"drill/m072","checks":[{"capability":"tools","status":"passed"},{"capability":"kumbha","status":"passed"},{"capability":"kumbha_text","status":"failed"}]}')`)

	if err := migrator(t, db).Migrate(72); err != nil {
		t.Fatalf("migrate to 072: %v", err)
	}
	var caps string
	if err := db.QueryRow(`SELECT string_agg(c->>'capability' || '=' || (c->>'status'), ',' ORDER BY ord)
		FROM inference.model_probe_reports, jsonb_array_elements(report->'checks') WITH ORDINALITY AS t(c, ord)
		WHERE model_route = 'drill/m072'`).Scan(&caps); err != nil {
		t.Fatal(err)
	}
	if caps != "tools=passed,build=passed,build_text=failed" {
		t.Errorf("probe report checks = %s", caps)
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM billing.build_messages WHERE session_id = $1`, session); got != 1 {
		t.Errorf("messages after rename = %d, want 1", got)
	}
	if got := queryInt(t, db, `SELECT build_priority FROM inference.models WHERE model_route = 'drill/m072' AND build_enabled`); got != 3 {
		t.Errorf("model build_priority = %d, want 3", got)
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM billing.usage_records WHERE resource_type = 'build/teepin/x:input'`); got != 1 {
		t.Errorf("renamed usage lines = %d, want 1", got)
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM billing.usage_records WHERE resource_type = 'cpu.home'`); got != 1 {
		t.Error("an unrelated usage line was changed")
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM storage.buckets WHERE project_id = $1 AND name = 'build-attachments'`, project); got != 1 {
		t.Errorf("attachments bucket not renamed")
	}
	if tableExists(t, db, "billing", "kumbha_messages") || columnExists(t, db, "inference", "models", "kumbha_enabled") {
		t.Error("an old name is still present after 072")
	}
	// The sequences follow: a new message gets an id without error.
	mustExec(t, db, `INSERT INTO billing.build_messages (session_id, content) VALUES ($1, 'again')`, session)

	if err := migrator(t, db).Migrate(71); err != nil {
		t.Fatalf("migrate down to 071: %v", err)
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM billing.kumbha_messages WHERE session_id = $1`, session); got != 2 {
		t.Errorf("messages after rollback = %d, want 2", got)
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM billing.usage_records WHERE resource_type = 'kumbha/teepin/x:input'`); got != 1 {
		t.Error("usage prefix not restored by the rollback")
	}
	if err := migrator(t, db).Migrate(72); err != nil {
		t.Fatalf("migrate up to 072 again: %v", err)
	}
}
