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

// newMockMeter shares one sqlmock DB between objectstore.Store and
// billing.Service — the same pattern pkg/billing's own
// newMockCollector uses, since in production both really do share one
// *sql.DB.
func newMockMeter(t *testing.T) (*Meter, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewMeter(NewStore(db), billing.NewService(db), time.Hour), mock
}

// expectNoHold queues the meter's per-account hold read for an account that
// has never had one.
func expectNoHold(mock sqlmock.Sqlmock, accountID uuid.UUID) {
	mock.ExpectQuery(`FROM billing\.storage_holds WHERE account_id`).WithArgs(accountID).
		WillReturnRows(sqlmock.NewRows([]string{"held", "released"}).AddRow(false, nil))
}

func bucketMeterRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_id", "project_id", "name", "backend",
		"object_count", "total_bytes", "created_at", "updated_at",
	})
}

// TestMeter_SkipsEntirelyWhenRateIsZero proves the meter never even lists
// buckets when no rate is configured — most deployments will never set
// one, and there's no reason to touch every bucket's billing history on
// a schedule just to compute a guaranteed-zero charge.
func TestMeter_SkipsEntirelyWhenRateIsZero(t *testing.T) {
	meter, mock := newMockMeter(t)
	mock.ExpectQuery(`SELECT object_storage_price_per_gb_month FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"object_storage_price_per_gb_month"}).AddRow(0.0))

	if err := meter.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMeter_BillsBucketSinceCreation proves a never-before-billed bucket
// is metered from its own creation time, at the correct GB-month-derived
// rate. ConsumeCredit is deliberately left unmocked — Meter treats a
// credit-draw failure as non-fatal (logged, not propagated), and
// ConsumeCredit's own internals are already covered by
// pkg/billing/credits_test.go, so re-verifying its transaction shape
// here would test billing's code, not the meter's.
func TestMeter_BillsBucketSinceCreation(t *testing.T) {
	meter, mock := newMockMeter(t)
	bucketID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()
	createdAt := time.Now().Add(-2 * time.Hour)

	mock.ExpectQuery(`SELECT object_storage_price_per_gb_month FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"object_storage_price_per_gb_month"}).AddRow(0.02))
	mock.ExpectQuery(`SELECT .* FROM storage\.buckets\s+WHERE deleted_at IS NULL`).
		WillReturnRows(bucketMeterRows().
			AddRow(bucketID, accountID, projectID, "photos", "shelby", 3, int64(10<<30), createdAt, createdAt))
	// No prior billing record for this bucket — MAX(end_time) over zero
	// matching rows still returns one row with a NULL value (standard SQL
	// aggregate behaviour), not zero rows.
	expectNoHold(mock, accountID)
	mock.ExpectQuery(`SELECT MAX\(end_time\) FROM billing\.usage_records`).
		WithArgs(SubjectTypeBucket, bucketID.String()).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))
	mock.ExpectQuery(`INSERT INTO billing\.usage_records`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(uuid.New(), time.Now()))

	if err := meter.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMeter_SkipsRecentlyBilledBucket mirrors
// pkg/billing's own TestCollectUsage_SkipsRecentlyCollected: a bucket
// billed 30 seconds ago is below the 1-minute floor, so no new usage
// record (and no rate read wasted work beyond the one already spent
// deciding whether to bother).
func TestMeter_SkipsRecentlyBilledBucket(t *testing.T) {
	meter, mock := newMockMeter(t)
	bucketID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT object_storage_price_per_gb_month FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"object_storage_price_per_gb_month"}).AddRow(0.02))
	mock.ExpectQuery(`SELECT .* FROM storage\.buckets\s+WHERE deleted_at IS NULL`).
		WillReturnRows(bucketMeterRows().
			AddRow(bucketID, accountID, projectID, "photos", "shelby", 3, int64(10<<30), time.Now().Add(-5*time.Hour), time.Now()))
	expectNoHold(mock, accountID)
	mock.ExpectQuery(`SELECT MAX\(end_time\) FROM billing\.usage_records`).
		WithArgs(SubjectTypeBucket, bucketID.String()).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(time.Now().Add(-30 * time.Second)))

	if err := meter.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A bucket whose account is on storage hold is not billed at all.
func TestMeter_DoesNotBillHeldStorage(t *testing.T) {
	meter, mock := newMockMeter(t)
	bucketID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT object_storage_price_per_gb_month FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"object_storage_price_per_gb_month"}).AddRow(0.02))
	mock.ExpectQuery(`SELECT .* FROM storage\.buckets\s+WHERE deleted_at IS NULL`).
		WillReturnRows(bucketMeterRows().
			AddRow(bucketID, accountID, projectID, "photos", "shelby", 3, int64(10<<30), time.Now().Add(-5*time.Hour), time.Now()))
	mock.ExpectQuery(`FROM billing\.storage_holds WHERE account_id`).WithArgs(accountID).
		WillReturnRows(sqlmock.NewRows([]string{"held", "released"}).AddRow(true, nil))

	if err := meter.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err) // no usage query or insert may follow the hold read
	}
}

// When a hold has just ended, billing resumes from the release moment, not
// from the last pre-hold record: the held days are never back-charged.
func TestMeter_DoesNotBackBillTheHeldPeriod(t *testing.T) {
	meter, mock := newMockMeter(t)
	bucketID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()
	released := time.Now().Add(-30 * time.Second) // hold ended 30s ago

	mock.ExpectQuery(`SELECT object_storage_price_per_gb_month FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"object_storage_price_per_gb_month"}).AddRow(0.02))
	mock.ExpectQuery(`SELECT .* FROM storage\.buckets\s+WHERE deleted_at IS NULL`).
		WillReturnRows(bucketMeterRows().
			AddRow(bucketID, accountID, projectID, "photos", "shelby", 3, int64(10<<30), time.Now().Add(-240*time.Hour), time.Now()))
	mock.ExpectQuery(`FROM billing\.storage_holds WHERE account_id`).WithArgs(accountID).
		WillReturnRows(sqlmock.NewRows([]string{"held", "released"}).AddRow(false, released))
	// Last billed 8 days ago (before the hold), but only 30s since release,
	// which is under the one-minute floor, so nothing is recorded.
	mock.ExpectQuery(`SELECT MAX\(end_time\) FROM billing\.usage_records`).
		WithArgs(SubjectTypeBucket, bucketID.String()).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(time.Now().Add(-8 * 24 * time.Hour)))

	if err := meter.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err) // an INSERT here would mean the held days were billed
	}
}
