// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs PurgeAccount against a real Postgres, because the purge is
// irreversible and sqlmock cannot show the SQL is right:
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55433/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestPurgeIntegration ./pkg/objectstore/ -v
package objectstore

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	_ "github.com/lib/pq"

	"github.com/FlashbackAi/teepin-core/migrations"
)

func purgeDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the purge integration test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("migration source: %v", err)
	}
	drv, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		t.Fatalf("migration driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}
	// Seeded accounts and projects are not real rows; drop the foreign keys
	// to auth.* on this disposable database only.
	rows, err := db.Query(`
		SELECT conrelid::regclass::text, conname FROM pg_constraint
		WHERE contype = 'f' AND conrelid::regclass::text LIKE 'storage.%'
		  AND confrelid::regclass::text LIKE 'auth.%'`)
	if err != nil {
		t.Fatalf("list fks: %v", err)
	}
	type fk struct{ table, name string }
	var fks []fk
	for rows.Next() {
		var f fk
		if err := rows.Scan(&f.table, &f.name); err != nil {
			t.Fatal(err)
		}
		fks = append(fks, f)
	}
	rows.Close()
	for _, f := range fks {
		if _, err := db.Exec(`ALTER TABLE ` + f.table + ` DROP CONSTRAINT ` + f.name); err != nil {
			t.Fatalf("drop fk: %v", err)
		}
	}
	return db
}

func TestPurgeIntegration(t *testing.T) {
	db := purgeDB(t)
	backend := newFakeBackend()
	svc := NewService(NewStore(db), backend, 1<<30)
	ctx := context.Background()

	victim, bystander := uuid.New(), uuid.New()
	p1, p2, p3 := uuid.New(), uuid.New(), uuid.New()

	put := func(acct, proj uuid.UUID, bucket, key string) {
		t.Helper()
		body := []byte("data-" + key)
		if _, err := svc.PutObject(ctx, acct, proj, bucket, key, bytes.NewReader(body), int64(len(body)), "text/plain", nil); err != nil {
			t.Fatalf("put %s/%s: %v", bucket, key, err)
		}
	}
	for _, b := range []struct {
		acct, proj uuid.UUID
		name       string
	}{{victim, p1, "one"}, {victim, p2, "two"}, {bystander, p3, "keep"}} {
		if _, err := svc.CreateBucket(ctx, b.acct, b.proj, b.name); err != nil {
			t.Fatalf("create bucket %s: %v", b.name, err)
		}
	}
	put(victim, p1, "one", "a.txt")
	put(victim, p1, "one", "dir/b.txt")
	put(victim, p2, "two", "c.txt")
	put(bystander, p3, "keep", "precious.txt")

	ids, err := svc.AccountsWithStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	has := map[uuid.UUID]bool{}
	for _, id := range ids {
		has[id] = true
	}
	if !has[victim] || !has[bystander] {
		t.Fatalf("AccountsWithStorage = %v, want both accounts", ids)
	}

	if err := svc.PurgeAccount(ctx, victim); err != nil {
		t.Fatalf("PurgeAccount: %v", err)
	}
	// Running it again is harmless.
	if err := svc.PurgeAccount(ctx, victim); err != nil {
		t.Fatalf("second PurgeAccount: %v", err)
	}

	if buckets, _ := svc.store.ListBucketsForAccount(ctx, victim); len(buckets) != 0 {
		t.Errorf("victim still has %d buckets", len(buckets))
	}
	// Only the bystander's single blob remains on the backend.
	if len(backend.data) != 1 {
		t.Errorf("backend holds %d blobs, want exactly the bystander's 1", len(backend.data))
	}
	if buckets, _ := svc.store.ListBucketsForAccount(ctx, bystander); len(buckets) != 1 {
		t.Errorf("bystander lost its bucket")
	}
	if objs, _ := svc.ListObjects(ctx, bystander, p3, "keep", "", "", 10); len(objs) != 1 {
		t.Errorf("bystander lost its object")
	}
	ids, _ = svc.AccountsWithStorage(ctx)
	for _, id := range ids {
		if id == victim {
			t.Error("victim still listed as having storage")
		}
	}
}
