// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Drill for migration 070 (inference.models.kumbha_image_reader), against a
// disposable Postgres; behind the build tag so it never runs in CI:
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55455/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestKumbhaImageReaderDrill ./migrations/ -v
package migrations

import (
	"database/sql"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/lib/pq"
)

func TestKumbhaImageReaderDrill(t *testing.T) {
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
	if !columnExists(t, db, "inference", "models", "kumbha_image_reader") {
		t.Fatal("after up: inference.models.kumbha_image_reader missing")
	}
	// Existing models are not readers until someone says so.
	var defaulted int
	if err := db.QueryRow(`SELECT COUNT(*) FROM inference.models WHERE kumbha_image_reader`).Scan(&defaulted); err != nil {
		t.Fatal(err)
	}
	if defaulted != 0 {
		t.Errorf("%d existing models became image readers by default", defaulted)
	}

	if err := migrator(t, db).Migrate(69); err != nil {
		t.Fatalf("migrate down to 069: %v", err)
	}
	if columnExists(t, db, "inference", "models", "kumbha_image_reader") {
		t.Error("after down: the column is still present")
	}
	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	if !columnExists(t, db, "inference", "models", "kumbha_image_reader") {
		t.Error("after re-up: the column is missing")
	}
	t.Log("drill passed: 070 applies, reverts cleanly, and re-applies")
}
