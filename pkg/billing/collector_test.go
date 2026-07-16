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

func runningInstanceRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "project_id", "instance_type_id", "gpu_vram_gb", "created_at",
	})
}

func TestCollectUsage_BillsCustomSizeLinearly(t *testing.T) {
	collector, mock := newMockCollector(t)
	projectID := uuid.New()
	createdAt := time.Now().Add(-2 * time.Hour)

	// A 25GB custom instance running for ~2 hours. The old rate-table
	// logic priced unknown types at $0 — this must now bill $0.10/GB-hr.
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WillReturnRows(runningInstanceRows().
			AddRow("inst-25gb0001", projectID, "gpu.h100.custom-25gb", 25, createdAt))

	// No previous collection.
	mock.ExpectQuery(`SELECT MAX\(end_time\)`).
		WithArgs("inst-25gb0001").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))

	// Usage record: unit price must be exactly 25 * $0.10 = $2.50/hr.
	mock.ExpectQuery(`INSERT INTO billing\.usage_records`).
		WithArgs(projectID, "inst-25gb0001", "gpu.h100.custom-25gb",
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

func TestCollectUsage_SkipsCPUOnlyInstances(t *testing.T) {
	collector, mock := newMockCollector(t)

	// CPU-only instance (no VRAM): no usage record must be written.
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WillReturnRows(runningInstanceRows().
			AddRow("inst-cpu00001", uuid.New(), "cpu.small", 0, time.Now().Add(-3*time.Hour)))

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
		WillReturnRows(runningInstanceRows().
			AddRow("inst-20gb0001", uuid.New(), "gpu.h100.2g.20gb", 20, time.Now().Add(-5*time.Hour)))

	// Last collection was 30 seconds ago → below the 1-minute floor,
	// no new record.
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
