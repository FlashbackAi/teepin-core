// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs the storage-hold lifecycle against a real Postgres. See
// credits_integration_test.go for how to run it.
package billing

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStored is a HeldStorage whose contents the test controls.
type fakeStored struct {
	accounts  map[uuid.UUID]bool
	purged    []uuid.UUID
	purgeFail error
}

func (f *fakeStored) Name() string { return "fake" }
func (f *fakeStored) AccountsWithStorage(context.Context) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for id, has := range f.accounts {
		if has {
			out = append(out, id)
		}
	}
	return out, nil
}
func (f *fakeStored) PurgeAccount(_ context.Context, id uuid.UUID) error {
	if f.purgeFail != nil {
		return f.purgeFail
	}
	f.purged = append(f.purged, id)
	f.accounts[id] = false
	return nil
}

func holdCount(t *testing.T, db *sql.DB, account uuid.UUID, where string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM billing.storage_holds WHERE account_id = $1 AND `+where, account).Scan(&n); err != nil {
		t.Fatalf("count holds: %v", err)
	}
	return n
}

func expireHold(t *testing.T, db *sql.DB, account uuid.UUID) {
	t.Helper()
	if _, err := db.Exec(`UPDATE billing.storage_holds SET delete_after = NOW() - INTERVAL '1 minute'
		WHERE account_id = $1 AND released_at IS NULL AND purged_at IS NULL`, account); err != nil {
		t.Fatalf("expire hold: %v", err)
	}
}

func TestStorageHoldIntegration_Lifecycle(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()

	broke := itAccount(t, db)  // has data, no credit
	funded := itAccount(t, db) // has data and credit
	itLot(t, db, funded, "purchase", 50, 0, nil)
	store := &fakeStored{accounts: map[uuid.UUID]bool{broke: true, funded: true}}
	sw := NewStorageHoldSweeper(s, EnforceOn, 0, store)

	// 1. Only the account with no credit is held; sweeping again does not
	// create a second hold.
	sw.Sweep(ctx)
	sw.Sweep(ctx)
	if n := holdCount(t, db, broke, "released_at IS NULL AND purged_at IS NULL"); n != 1 {
		t.Fatalf("broke account has %d active holds, want exactly 1", n)
	}
	if n := holdCount(t, db, funded, "TRUE"); n != 0 {
		t.Errorf("an account with credit was held")
	}
	hold, err := s.ActiveStorageHold(ctx, broke)
	if err != nil || hold == nil {
		t.Fatalf("ActiveStorageHold: %v %v", hold, err)
	}
	if d := hold.DeleteAfter.Sub(hold.HeldSince); d < 6*24*time.Hour+23*time.Hour || d > 7*24*time.Hour+time.Hour {
		t.Errorf("hold lasts %s, want 7 days", d)
	}

	// 2. While held nothing is billable.
	if held, _, err := s.StorageBillingFloor(ctx, broke); err != nil || !held {
		t.Errorf("StorageBillingFloor held=%v err=%v, want held", held, err)
	}

	// 3. Credit returns: the hold is released, the data stays, and the
	// billing floor moves to the release moment.
	itLot(t, db, broke, "purchase", 20, 0, nil)
	sw.Sweep(ctx)
	if n := holdCount(t, db, broke, "released_at IS NOT NULL"); n != 1 {
		t.Fatalf("hold not released after top-up")
	}
	if len(store.purged) != 0 {
		t.Fatal("data purged though credit returned")
	}
	held, floor, err := s.StorageBillingFloor(ctx, broke)
	if err != nil || held || time.Since(floor) > time.Minute || floor.IsZero() {
		t.Errorf("after release: held=%v floor=%v err=%v; want not held and floor about now", held, floor, err)
	}

	// 4. Spend it all again: a fresh 7 days starts (the old hold does not
	// carry over).
	itConsume(t, s, broke, 20)
	sw.Sweep(ctx)
	if n := holdCount(t, db, broke, "released_at IS NULL AND purged_at IS NULL"); n != 1 {
		t.Fatalf("second hold not started")
	}

	// 5. A top-up that lands after the hold expired but before the sweep
	// wins: nothing is deleted.
	expireHold(t, db, broke)
	itLot(t, db, broke, "purchase", 20, 0, nil)
	sw.Sweep(ctx)
	if len(store.purged) != 0 {
		t.Fatal("data deleted though the account had topped up")
	}

	// 6. Out of credit again and expired: a failed purge is recorded and
	// retried, then a successful one marks the hold purged.
	itConsume(t, s, broke, 20)
	sw.Sweep(ctx) // starts a new hold
	expireHold(t, db, broke)
	store.purgeFail = errors.New("backend unavailable")
	sw.Sweep(ctx)
	var lastErr sql.NullString
	if err := db.QueryRow(`SELECT last_error FROM billing.storage_holds
		WHERE account_id = $1 AND released_at IS NULL AND purged_at IS NULL`, broke).Scan(&lastErr); err != nil || !lastErr.Valid {
		t.Fatalf("purge failure not recorded on the hold: %v %v", lastErr, err)
	}
	store.purgeFail = nil
	sw.Sweep(ctx)
	if len(store.purged) != 1 || store.purged[0] != broke {
		t.Fatalf("purged = %v, want the broke account once", store.purged)
	}
	if n := holdCount(t, db, broke, "purged_at IS NOT NULL"); n != 1 {
		t.Errorf("hold not marked purged")
	}
	if n := holdCount(t, db, funded, "TRUE"); n != 0 {
		t.Errorf("the funded account was touched")
	}
}

// A customer who deletes everything themselves ends their own hold.
func TestStorageHoldIntegration_NothingLeftReleasesTheHold(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	acct := itAccount(t, db)
	store := &fakeStored{accounts: map[uuid.UUID]bool{acct: true}}
	sw := NewStorageHoldSweeper(s, EnforceOn, 0, store)

	sw.Sweep(ctx)
	if n := holdCount(t, db, acct, "released_at IS NULL AND purged_at IS NULL"); n != 1 {
		t.Fatalf("hold not started")
	}
	store.accounts[acct] = false
	sw.Sweep(ctx)
	if n := holdCount(t, db, acct, "released_at IS NOT NULL"); n != 1 {
		t.Errorf("hold still active though nothing is stored")
	}
}

// Dry-run never writes or deletes.
func TestStorageHoldIntegration_DryRunChangesNothing(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	acct := itAccount(t, db)
	store := &fakeStored{accounts: map[uuid.UUID]bool{acct: true}}

	NewStorageHoldSweeper(s, EnforceDryRun, 0, store).Sweep(ctx)
	if n := holdCount(t, db, acct, "TRUE"); n != 0 {
		t.Errorf("dry-run created a hold")
	}
}
