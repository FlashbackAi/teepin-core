// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Drill for migration 073 (P-core/E-core split removed): the columns go, the
// rest of each table is untouched, and the migration reverses cleanly.
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55433/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestDropPECoresDrill ./migrations/ -v
package migrations

import (
	"database/sql"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

func TestDropPECoresDrill(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Migrate(72); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 072: %v", err)
	}
	mustExec(t, db, `UPDATE billing.pricing SET cpu_price_per_core_hour = 0.002, p_core_price_per_hour = 0.003 WHERE id = 1`)

	if err := migrator(t, db).Migrate(73); err != nil {
		t.Fatalf("migrate to 073: %v", err)
	}
	for _, c := range [][3]string{{"billing", "pricing", "p_core_price_per_hour"}, {"billing", "pricing", "e_core_price_per_hour"},
		{"compute", "instances", "p_cores_used"}, {"compute", "instances", "e_cores_used"},
		{"compute", "nodes", "p_cores"}, {"compute", "nodes", "e_cores"}} {
		if columnExists(t, db, c[0], c[1], c[2]) {
			t.Errorf("%s.%s.%s still exists after 073", c[0], c[1], c[2])
		}
	}
	if got := queryFloat(t, db, `SELECT cpu_price_per_core_hour FROM billing.pricing WHERE id = 1`); !near(got, 0.002) {
		t.Errorf("cpu price = %v, want it untouched", got)
	}

	if err := migrator(t, db).Migrate(72); err != nil {
		t.Fatalf("migrate down to 072: %v", err)
	}
	if !columnExists(t, db, "compute", "nodes", "p_cores") || !columnExists(t, db, "billing", "pricing", "e_core_price_per_hour") {
		t.Error("the rollback did not restore the columns")
	}
	if err := migrator(t, db).Migrate(73); err != nil {
		t.Fatalf("migrate up to 073 again: %v", err)
	}
}
