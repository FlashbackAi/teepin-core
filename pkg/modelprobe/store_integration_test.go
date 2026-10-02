// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs the report store's SQL against a real Postgres (migration 069), behind
// the migration-drill tag like the other database drills:
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55444/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestStoreIntegration ./pkg/modelprobe/ -v
package modelprobe

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/lib/pq"

	"github.com/FlashbackAi/teepin-core/migrations"
)

func integrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the report store integration test")
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

func TestStoreIntegration(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	st := NewStore(db)

	route := "teepin/probe-" + time.Now().Format("150405.000000")
	if _, err := db.Exec(`
		INSERT INTO inference.models (model_route, display_name, cost_class, engine) VALUES ($1, 'Probe test', 'frontier', 'test')
	`, route); err != nil {
		t.Fatalf("seed model: %v", err)
	}

	if _, err := st.Get(ctx, route); !errors.Is(err, ErrNoReport) {
		t.Fatalf("never-checked model: err = %v, want ErrNoReport", err)
	}

	first := &Report{ModelRoute: route, RanAt: time.Now().UTC(),
		Metadata: &MetadataReport{ContextWindow: 32768, Source: "test"},
		Checks: []Check{
			{Capability: CapTools, Status: StatusPassed, Attempts: 2, Passed: 2, CheckedAt: time.Now().UTC()},
			{Capability: CapVision, Status: StatusFailed, Attempts: 2, Detail: "wrong colours", CheckedAt: time.Now().UTC()},
		}}
	if err := st.Put(ctx, first); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := st.Get(ctx, route)
	if err != nil || got.Check(CapTools).Status != StatusPassed || got.Check(CapVision).Detail != "wrong colours" || got.Metadata.ContextWindow != 32768 {
		t.Fatalf("round trip: %+v, %v", got, err)
	}

	// A later check that could not run (the backend was down) keeps the
	// verdict, and a conclusive one replaces it; the row is updated, not added.
	second := &Report{ModelRoute: route, RanAt: time.Now().UTC(),
		Metadata: &MetadataReport{Error: "unreachable"},
		Checks: []Check{
			{Capability: CapTools, Status: StatusError, Detail: "503"},
			{Capability: CapVision, Status: StatusPassed, Attempts: 2, Passed: 2},
		}}
	if err := st.Put(ctx, second); err != nil {
		t.Fatalf("put 2: %v", err)
	}
	got, _ = st.Get(ctx, route)
	if got.Check(CapTools).Status != StatusPassed {
		t.Errorf("an outage erased a verified result: %+v", got.Check(CapTools))
	}
	if got.Check(CapVision).Status != StatusPassed {
		t.Errorf("a conclusive result did not replace the old one: %+v", got.Check(CapVision))
	}
	if got.Metadata == nil || got.Metadata.ContextWindow != 32768 {
		t.Errorf("a failed metadata lookup erased the earlier reading: %+v", got.Metadata)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM inference.model_probe_reports WHERE model_route = $1`, route).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("rows = %d (err %v), want 1", rows, err)
	}

	all, err := st.All(ctx)
	if err != nil || all[route] == nil {
		t.Errorf("All: %v %v", all, err)
	}

	// The report goes with the model; deleting the model deletes it.
	if _, err := db.Exec(`DELETE FROM inference.models WHERE model_route = $1`, route); err != nil {
		t.Fatalf("delete model: %v", err)
	}
	if _, err := st.Get(ctx, route); !errors.Is(err, ErrNoReport) {
		t.Errorf("a report outlived its model: %v", err)
	}
	// And a report cannot exist for a model that does not.
	if err := st.Put(ctx, &Report{ModelRoute: "teepin/never-registered", RanAt: time.Now().UTC()}); err == nil {
		t.Error("a report was stored for a model that is not in the catalog")
	}
}
