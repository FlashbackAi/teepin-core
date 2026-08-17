// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Throwaway archivability drill for migration 016. Run explicitly against a
// disposable Postgres; it is behind a build tag so it never runs in CI or the
// normal `go test ./...`.
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55433/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestArchivabilityDrill ./migrations/ -v
package migrations

import (
	"database/sql"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/lib/pq"
)

func migrator(t *testing.T, db *sql.DB) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(FS, ".")
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	drv, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		t.Fatalf("init driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		t.Fatalf("init migrator: %v", err)
	}
	return m
}

func tableExists(t *testing.T, db *sql.DB, schema, table string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM information_schema.tables
		               WHERE table_schema = $1 AND table_name = $2)
	`, schema, table).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %s.%s: %v", schema, table, err)
	}
	return exists
}

func columnExists(t *testing.T, db *sql.DB, schema, table, col string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = $1 AND table_name = $2 AND column_name = $3)
	`, schema, table, col).Scan(&exists)
	if err != nil {
		t.Fatalf("check column %s.%s.%s: %v", schema, table, col, err)
	}
	return exists
}

// TestArchivabilityDrill proves the home-compute pilot removes cleanly:
// full up → nodes objects exist → down one → they are gone AND the rest of
// the schema is intact → up again → they are back. This is the "flag off +
// revert 016 leaves the platform as before" guarantee, exercised for real.
func TestArchivabilityDrill(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// 1. Full up.
	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !tableExists(t, db, "compute", "nodes") {
		t.Fatal("after up: compute.nodes missing")
	}
	if !tableExists(t, db, "compute", "node_enrollment_tokens") {
		t.Fatal("after up: compute.node_enrollment_tokens missing")
	}
	if !columnExists(t, db, "compute", "instances", "node_id") {
		t.Fatal("after up: compute.instances.node_id missing")
	}

	// 2. Revert down to version 15 (the state AFTER 015, BEFORE 016). Using
	//    an absolute target rather than relative Steps keeps this robust as
	//    later migrations (017, 018, ...) are added on top.
	if err := migrator(t, db).Migrate(15); err != nil {
		t.Fatalf("migrate down to 015: %v", err)
	}
	if tableExists(t, db, "compute", "nodes") {
		t.Error("after down: compute.nodes still present")
	}
	if tableExists(t, db, "compute", "node_enrollment_tokens") {
		t.Error("after down: compute.node_enrollment_tokens still present")
	}
	if columnExists(t, db, "compute", "instances", "node_id") {
		t.Error("after down: compute.instances.node_id still present")
	}
	// The rest of the platform must be intact — spot-check core tables the
	// pilot must never have disturbed.
	for _, tbl := range [][2]string{
		{"compute", "instances"}, {"billing", "invoices"},
		{"billing", "payment_methods"}, {"auth", "accounts"},
	} {
		if !tableExists(t, db, tbl[0], tbl[1]) {
			t.Errorf("after down: %s.%s was wrongly removed", tbl[0], tbl[1])
		}
	}

	// 3. Up again — restorable.
	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	if !tableExists(t, db, "compute", "nodes") {
		t.Fatal("after re-up: compute.nodes missing")
	}
	t.Log("archivability drill passed: 016 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill017 proves Stage 2 (migration 017) is equally
// reversible: its columns exist after up, are gone after reverting one step,
// and the instances table and pricing row survive.
func TestArchivabilityDrill017(t *testing.T) {
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
	if !columnExists(t, db, "compute", "instances", "provider_id") {
		t.Fatal("after up: compute.instances.provider_id missing")
	}
	if !columnExists(t, db, "billing", "pricing", "cpu_price_per_core_hour") {
		t.Fatal("after up: billing.pricing.cpu_price_per_core_hour missing")
	}

	// Revert to version 16 (state after 016, before 017), absolute so later
	// migrations do not shift it.
	if err := migrator(t, db).Migrate(16); err != nil {
		t.Fatalf("migrate down to 016: %v", err)
	}
	if columnExists(t, db, "compute", "instances", "provider_id") {
		t.Error("after down: compute.instances.provider_id still present")
	}
	if columnExists(t, db, "billing", "pricing", "cpu_price_per_core_hour") {
		t.Error("after down: cpu_price_per_core_hour still present")
	}
	// The instances table and the pricing row must be intact — 017 must not
	// have removed anything it did not add.
	if !tableExists(t, db, "compute", "instances") {
		t.Error("after down: compute.instances was wrongly removed")
	}
	if !columnExists(t, db, "billing", "pricing", "vram_price_per_gb_hour") {
		t.Error("after down: the VRAM rate (predates 017) was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 017 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill018 proves Stage 2.5 (migration 018) is reversible:
// the rentable columns exist after up, are gone after reverting one step, and
// the node's detected specs survive.
func TestArchivabilityDrill018(t *testing.T) {
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
	if !columnExists(t, db, "compute", "nodes", "rentable_cpu_cores") {
		t.Fatal("after up: compute.nodes.rentable_cpu_cores missing")
	}

	// Revert to version 17 (state after 017, before 018), absolute.
	if err := migrator(t, db).Migrate(17); err != nil {
		t.Fatalf("migrate down to 017: %v", err)
	}
	if columnExists(t, db, "compute", "nodes", "rentable_cpu_cores") {
		t.Error("after down: rentable_cpu_cores still present")
	}
	if columnExists(t, db, "compute", "nodes", "rentable_memory_gb") {
		t.Error("after down: rentable_memory_gb still present")
	}
	// Detected specs (migration 016) must survive.
	if !columnExists(t, db, "compute", "nodes", "cpu_cores") {
		t.Error("after down: detected cpu_cores was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 018 applies, reverts cleanly, and re-applies")
}
