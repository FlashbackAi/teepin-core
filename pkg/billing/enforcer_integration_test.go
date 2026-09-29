// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs the credit enforcer's SQL against a real Postgres. See
// credits_integration_test.go for how to run it.
package billing

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// itInstance seeds a running GPU instance for account. Foreign keys to
// users and projects are bypassed for this disposable database (a real
// instance needs both behind it) using a dedicated connection.
func itInstance(t *testing.T, ctx context.Context, s *Service, account uuid.UUID, id string, vramGB, storageGB int) {
	t.Helper()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatalf("disable fk triggers: %v", err)
	}
	defer conn.ExecContext(ctx, `SET session_replication_role = DEFAULT`)
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO compute.instances
			(id, project_id, user_id, account_id, name, image, status, gpu_vram_gb, cpu_units, memory_gb, storage_gb, created_at)
		VALUES ($1, $2, $3, $4, 'it', 'img', 'running', $5, 1, 1, $6, NOW() - INTERVAL '30 minutes')
	`, id, uuid.New(), uuid.New(), account, vramGB, storageGB); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
}

func TestCreditEnforcerIntegration(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()

	if _, err := db.Exec(`UPDATE billing.pricing SET vram_price_per_gb_hour = 0.10`); err != nil {
		t.Fatalf("set rate: %v", err)
	}

	broke := itAccount(t, db)   // 20GB = $2/hour, ran 30 min => $1 accrued; only $1.02 credit
	funded := itAccount(t, db)  // same instance, $100 credit
	diskful := itAccount(t, db) // out of credit but has a persistent disk
	itLot(t, db, broke, "purchase", 1.02, 0, nil)
	itLot(t, db, funded, "purchase", 100, 0, nil)
	itInstance(t, ctx, s, broke, "it-broke", 20, 0)
	itInstance(t, ctx, s, funded, "it-funded", 20, 0)
	itInstance(t, ctx, s, diskful, "it-disk", 20, 50)

	stopper := &fakeStopper{}
	e := NewCreditEnforcer(db, s, stopper, nil, EnforceOn)
	e.Tick(ctx)

	stopped := map[string]bool{}
	for _, call := range stopper.calls {
		for _, id := range call {
			stopped[id] = true
		}
	}
	if !stopped["it-broke"] {
		t.Error("the account whose credit is nearly gone was not stopped")
	}
	if stopped["it-funded"] {
		t.Error("an account with $100 credit was stopped")
	}
	if stopped["it-disk"] {
		t.Error("an instance with a persistent disk was stopped (data loss)")
	}
}
