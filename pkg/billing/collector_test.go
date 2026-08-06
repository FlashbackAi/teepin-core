// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newMockCollector(t *testing.T) (*UsageCollector, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewUsageCollector(db, NewService(db)), mock
}

func billableInstanceRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_id", "project_id", "instance_type_id", "gpu_vram_gb", "created_at", "terminated_at",
	})
}

func expectPricingRead(mock sqlmock.Sqlmock, rate float64) {
	mock.ExpectQuery(`SELECT vram_price_per_gb_hour FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"vram_price_per_gb_hour"}).AddRow(rate))
}

func TestCollectUsage_BillsCustomSizeLinearly(t *testing.T) {
	collector, mock := newMockCollector(t)
	accountID, projectID := uuid.New(), uuid.New()
	createdAt := time.Now().Add(-2 * time.Hour)

	// A 25GB custom instance running for ~2 hours. The old rate-table
	// logic priced unknown types at $0 — this must now bill $0.10/GB-hr.
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WillReturnRows(billableInstanceRows().
			AddRow("inst-25gb0001", accountID, projectID, "gpu.h100.custom-25gb", 25, createdAt, nil))

	// No previous collection.
	mock.ExpectQuery(`SELECT MAX\(end_time\)`).
		WithArgs("inst-25gb0001").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))

	// The live rate is read from billing.pricing before metering.
	expectPricingRead(mock, 0.10)

	// Usage record: unit price must be exactly 25 * $0.10 = $2.50/hr.
	mock.ExpectQuery(`INSERT INTO billing\.usage_records`).
		WithArgs(accountID, projectID, "inst-25gb0001", "gpu.h100.custom-25gb",
			sqlmock.AnyArg(), // hours (wall-clock dependent)
			"hours",
			2.50,             // unit price — the regression this test guards
			sqlmock.AnyArg(), // total cost
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow(uuid.New(), time.Now()))

	if err := collector.collectUsage(context.Background()); err != nil {
		t.Fatalf("collectUsage failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestCollectUsage_UsesAdminConfiguredRate(t *testing.T) {
	collector, mock := newMockCollector(t)
	accountID, projectID := uuid.New(), uuid.New()
	createdAt := time.Now().Add(-1 * time.Hour)

	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WillReturnRows(billableInstanceRows().
			AddRow("inst-20gb0001", accountID, projectID, "gpu.a100.2g.20gb", 20, createdAt, nil))

	mock.ExpectQuery(`SELECT MAX\(end_time\)`).
		WithArgs("inst-20gb0001").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))

	// Admin doubled the rate: 20GB must bill at 20 * $0.20 = $4.00/hr.
	expectPricingRead(mock, 0.20)

	mock.ExpectQuery(`INSERT INTO billing\.usage_records`).
		WithArgs(accountID, projectID, "inst-20gb0001", "gpu.a100.2g.20gb",
			sqlmock.AnyArg(), "hours",
			4.00, // 20GB at the admin-configured $0.20/GB-hr
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow(uuid.New(), time.Now()))

	if err := collector.collectUsage(context.Background()); err != nil {
		t.Fatalf("collectUsage failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestCollectUsage_BillsTerminatedTail(t *testing.T) {
	collector, mock := newMockCollector(t)
	accountID, projectID := uuid.New(), uuid.New()
	createdAt := time.Now().Add(-50 * time.Minute)
	terminatedAt := time.Now().Add(-10 * time.Minute)

	// An instance that lived 40 minutes and was deleted before any
	// hourly tick. The old collector only looked at running instances,
	// so this workload was never billed at all.
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WillReturnRows(billableInstanceRows().
			AddRow("inst-short001", accountID, projectID, "gpu.a100.2g.20gb", 20, createdAt, terminatedAt))

	mock.ExpectQuery(`SELECT MAX\(end_time\)`).
		WithArgs("inst-short001").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))

	expectPricingRead(mock, 0.10)

	// The record must end exactly at terminated_at — not at "now".
	mock.ExpectQuery(`INSERT INTO billing\.usage_records`).
		WithArgs(accountID, projectID, "inst-short001", "gpu.a100.2g.20gb",
			sqlmock.AnyArg(), "hours",
			2.00,             // 20GB * $0.10
			sqlmock.AnyArg(), // total cost
			createdAt,        // start: creation (no prior records)
			terminatedAt).    // end: termination, the billing boundary
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow(uuid.New(), time.Now()))

	if err := collector.collectUsage(context.Background()); err != nil {
		t.Fatalf("collectUsage failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestCollectUsage_SkipsCPUOnlyInstances(t *testing.T) {
	collector, mock := newMockCollector(t)

	// CPU-only instance (no VRAM): no usage record must be written.
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WillReturnRows(billableInstanceRows().
			AddRow("inst-cpu00001", uuid.New(), uuid.New(), "cpu.small", 0, time.Now().Add(-3*time.Hour), nil))

	if err := collector.collectUsage(context.Background()); err != nil {
		t.Fatalf("collectUsage failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestCollectUsage_SkipsRecentlyCollected(t *testing.T) {
	collector, mock := newMockCollector(t)

	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WillReturnRows(billableInstanceRows().
			AddRow("inst-20gb0001", uuid.New(), uuid.New(), "gpu.h100.2g.20gb", 20, time.Now().Add(-5*time.Hour), nil))

	// Last collection was 30 seconds ago → below the 1-minute floor,
	// no new record (and no pricing read either).
	mock.ExpectQuery(`SELECT MAX\(end_time\)`).
		WithArgs("inst-20gb0001").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).
			AddRow(time.Now().Add(-30 * time.Second)))

	if err := collector.collectUsage(context.Background()); err != nil {
		t.Fatalf("collectUsage failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
