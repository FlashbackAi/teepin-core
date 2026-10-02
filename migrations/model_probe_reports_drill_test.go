// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Drill for migration 069 (inference.model_probe_reports), against a
// disposable Postgres; behind the build tag so it never runs in CI or a normal
// `go test ./...`:
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55444/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestModelProbeReportsDrill ./migrations/ -v
package migrations

import (
	"database/sql"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/lib/pq"
)

func TestModelProbeReportsDrill(t *testing.T) {
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
	if !tableExists(t, db, "inference", "model_probe_reports") {
		t.Fatal("after up: inference.model_probe_reports missing")
	}
	if _, err := db.Exec(`INSERT INTO inference.model_probe_reports (model_route, ran_at, report) VALUES ('nope/none', NOW(), '{}')`); err == nil {
		t.Error("a report was stored for a model that does not exist")
	}

	if err := migrator(t, db).Migrate(68); err != nil {
		t.Fatalf("migrate down to 068: %v", err)
	}
	if tableExists(t, db, "inference", "model_probe_reports") {
		t.Error("after down: inference.model_probe_reports still present")
	}
	if !tableExists(t, db, "inference", "models") {
		t.Error("after down: inference.models was wrongly removed")
	}
	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	if !tableExists(t, db, "inference", "model_probe_reports") {
		t.Error("after re-up: table missing")
	}
	t.Log("drill passed: 069 applies, reverts cleanly, and re-applies")
}
