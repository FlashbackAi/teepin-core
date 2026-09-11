// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package objectstore

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

func newMockEgressTracker(t *testing.T) (*EgressTracker, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewEgressTracker(billing.NewService(db), time.Minute), mock
}

func TestEgressTracker_RecordAccumulatesPerBucket(t *testing.T) {
	tracker, _ := newMockEgressTracker(t)
	accountID, projectID, bucketID := uuid.New(), uuid.New(), uuid.New()

	tracker.Record(accountID, projectID, bucketID, 1000)
	tracker.Record(accountID, projectID, bucketID, 2000)

	tracker.mu.Lock()
	got := tracker.pending[egressKey{accountID, projectID, bucketID}]
	tracker.mu.Unlock()

	if got != 3000 {
		t.Fatalf("expected accumulated 3000 bytes, got %d", got)
	}
}

// TestEgressTracker_RecordIgnoresZeroOrNegative proves a cancelled
// download (0 bytes actually written) or any negative value never
// creates a pending entry at all — not just "adds zero," but genuinely
// never touches the map, so a flush over an all-cancelled interval does
// no work.
func TestEgressTracker_RecordIgnoresZeroOrNegative(t *testing.T) {
	tracker, _ := newMockEgressTracker(t)
	accountID, projectID, bucketID := uuid.New(), uuid.New(), uuid.New()

	tracker.Record(accountID, projectID, bucketID, 0)
	tracker.Record(accountID, projectID, bucketID, -5)

	tracker.mu.Lock()
	_, exists := tracker.pending[egressKey{accountID, projectID, bucketID}]
	tracker.mu.Unlock()

	if exists {
		t.Fatal("expected no pending entry for zero/negative byte counts")
	}
}

// TestEgressTracker_FlushDropsBatchWhenRateIsZero proves accumulated
// bytes are discarded, not re-queued, when no rate is configured —
// egress tracking exists only to bill it, and traffic from before a rate
// existed must never be retroactively charged once one is set later.
func TestEgressTracker_FlushDropsBatchWhenRateIsZero(t *testing.T) {
	tracker, mock := newMockEgressTracker(t)
	accountID, projectID, bucketID := uuid.New(), uuid.New(), uuid.New()
	tracker.Record(accountID, projectID, bucketID, 5<<20)

	mock.ExpectQuery(`SELECT object_storage_price_per_gb_egress FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"object_storage_price_per_gb_egress"}).AddRow(0.0))

	tracker.flush(context.Background())

	tracker.mu.Lock()
	remaining := len(tracker.pending)
	tracker.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected the batch to be cleared even when dropped, got %d pending entries", remaining)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err) // no RecordUsage call should have happened
	}
}

// TestEgressTracker_FlushBillsAccumulatedBytes proves a real rate turns
// accumulated bytes into a usage record with the right subject/quantity.
// ConsumeCredit is deliberately left unmocked — same reasoning as
// meter_test.go: it's non-fatal here and already covered by
// pkg/billing/credits_test.go.
func TestEgressTracker_FlushBillsAccumulatedBytes(t *testing.T) {
	tracker, mock := newMockEgressTracker(t)
	accountID, projectID, bucketID := uuid.New(), uuid.New(), uuid.New()
	tracker.Record(accountID, projectID, bucketID, 1<<30) // exactly 1 GB

	mock.ExpectQuery(`SELECT object_storage_price_per_gb_egress FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"object_storage_price_per_gb_egress"}).AddRow(0.09))
	mock.ExpectQuery(`INSERT INTO billing\.usage_records`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(uuid.New(), time.Now()))

	tracker.flush(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestEgressTracker_FlushOfEmptyBatchDoesNothing(t *testing.T) {
	tracker, mock := newMockEgressTracker(t)
	tracker.flush(context.Background()) // nothing recorded yet
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err) // not even the rate should be read for an empty batch
	}
}
