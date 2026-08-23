// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql/driver"
	"math"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// captureArg matches any float64 and stores it — used when a later
// argument's expected value depends on this one (wall-clock hours, which
// this test suite otherwise treats as unverifiable via sqlmock.AnyArg()).
type captureArg struct{ into *float64 }

func (c captureArg) Match(v driver.Value) bool {
	f, ok := v.(float64)
	if !ok {
		return false
	}
	*c.into = f
	return true
}

// funcArg matches via an arbitrary predicate, for a value that must be
// checked against a formula rather than a literal.
type funcArg func(driver.Value) bool

func (f funcArg) Match(v driver.Value) bool { return f(v) }

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
		"id", "account_id", "project_id", "instance_type_id", "gpu_vram_gb",
		"cpu_units", "memory_gb", "storage_gb", "created_at", "terminated_at",
	})
}

// expectPricingRead expects the four rate reads the collector now performs
// per run (VRAM, then CPU, then memory, then storage). cpuRate/memRate/
// storageRate default to 0 for the GPU-only tests, matching the migration
// default.
func expectPricingRead(mock sqlmock.Sqlmock, rate float64) {
	expectPricingReadFull(mock, rate, 0, 0, 0)
}

func expectPricingReadFull(mock sqlmock.Sqlmock, vram, cpu, mem, storage float64) {
	mock.ExpectQuery(`SELECT vram_price_per_gb_hour FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"vram_price_per_gb_hour"}).AddRow(vram))
	mock.ExpectQuery(`SELECT cpu_price_per_core_hour FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"cpu_price_per_core_hour"}).AddRow(cpu))
	mock.ExpectQuery(`SELECT memory_price_per_gb_hour FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"memory_price_per_gb_hour"}).AddRow(mem))
	mock.ExpectQuery(`SELECT storage_price_per_gb_month FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"storage_price_per_gb_month"}).AddRow(storage))
}

func TestCollectUsage_BillsCustomSizeLinearly(t *testing.T) {
	collector, mock := newMockCollector(t)
	accountID, projectID := uuid.New(), uuid.New()
	createdAt := time.Now().Add(-2 * time.Hour)

	// A 25GB custom instance running for ~2 hours. The old rate-table
	// logic priced unknown types at $0 — this must now bill $0.10/GB-hr.
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WillReturnRows(billableInstanceRows().
			AddRow("inst-25gb0001", accountID, projectID, "gpu.h100.custom-25gb", 25, 8, 32, 0, createdAt, nil))

	// No previous collection.
	mock.ExpectQuery(`SELECT MAX\(end_time\)`).
		WithArgs("inst-25gb0001").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))

	// The live rate is read from billing.pricing before metering.
	expectPricingRead(mock, 0.10)

	// Usage record: unit price must be exactly 25 * $0.10 = $2.50/hr.
	mock.ExpectQuery(`INSERT INTO billing\.usage_records`).
		WithArgs(accountID, projectID, "inst-25gb0001", "instance", "inst-25gb0001",
			0.0, nil, "gpu.h100.custom-25gb",
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
			AddRow("inst-20gb0001", accountID, projectID, "gpu.a100.2g.20gb", 20, 8, 32, 0, createdAt, nil))

	mock.ExpectQuery(`SELECT MAX\(end_time\)`).
		WithArgs("inst-20gb0001").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))

	// Admin doubled the rate: 20GB must bill at 20 * $0.20 = $4.00/hr.
	expectPricingRead(mock, 0.20)

	mock.ExpectQuery(`INSERT INTO billing\.usage_records`).
		WithArgs(accountID, projectID, "inst-20gb0001", "instance", "inst-20gb0001",
			0.0, nil, "gpu.a100.2g.20gb",
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
			AddRow("inst-short001", accountID, projectID, "gpu.a100.2g.20gb", 20, 8, 32, 0, createdAt, terminatedAt))

	mock.ExpectQuery(`SELECT MAX\(end_time\)`).
		WithArgs("inst-short001").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))

	expectPricingRead(mock, 0.10)

	// The record must end exactly at terminated_at — not at "now".
	mock.ExpectQuery(`INSERT INTO billing\.usage_records`).
		WithArgs(accountID, projectID, "inst-short001", "instance", "inst-short001",
			0.0, nil, "gpu.a100.2g.20gb",
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

// A CPU-only instance is now METERED (home compute), not skipped. At a
// non-zero rate it bills cores*cpuRate + gb*memRate; the record is written
// with the computed cost.
func TestCollectUsage_MetersCPUInstance(t *testing.T) {
	collector, mock := newMockCollector(t)
	accountID, projectID := uuid.New(), uuid.New()
	createdAt := time.Now().Add(-1 * time.Hour)

	// 4 vCPU / 8 GB CPU instance, no VRAM.
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WillReturnRows(billableInstanceRows().
			AddRow("inst-cpu00001", accountID, projectID, "cpu.home", 0, 4, 8, 0, createdAt, nil))

	mock.ExpectQuery(`SELECT MAX\(end_time\)`).
		WithArgs("inst-cpu00001").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))

	// VRAM 0 (unused for CPU), CPU $1.25/core-hr, mem $0.60/GB-hr.
	// unit price = 4*1.25 + 8*0.60 = 5.00 + 4.80 = 9.80/hr.
	expectPricingReadFull(mock, 0.10, 1.25, 0.60, 0)

	mock.ExpectQuery(`INSERT INTO billing\.usage_records`).
		WithArgs(accountID, projectID, "inst-cpu00001", "instance", "inst-cpu00001",
			0.0, nil, "cpu.home",
			sqlmock.AnyArg(), "hours",
			9.80, // the CPU cost formula — the regression this guards
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

// A CPU instance at the default rate of 0 still gets a usage record, but for
// $0 — the "metering on, billing off until a rate is set" guarantee.
func TestCollectUsage_CPUAtZeroRateRecordsZero(t *testing.T) {
	collector, mock := newMockCollector(t)
	accountID, projectID := uuid.New(), uuid.New()
	createdAt := time.Now().Add(-1 * time.Hour)

	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WillReturnRows(billableInstanceRows().
			AddRow("inst-cpu00002", accountID, projectID, "cpu.home", 0, 4, 8, 0, createdAt, nil))
	mock.ExpectQuery(`SELECT MAX\(end_time\)`).
		WithArgs("inst-cpu00002").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))
	// All rates 0 (migration default).
	expectPricingReadFull(mock, 0.10, 0, 0, 0)

	mock.ExpectQuery(`INSERT INTO billing\.usage_records`).
		WithArgs(accountID, projectID, "inst-cpu00002", "instance", "inst-cpu00002",
			0.0, nil, "cpu.home",
			sqlmock.AnyArg(), "hours",
			0.0, // unit price 0 at default rates
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow(uuid.New(), time.Now()))
	// ConsumeCredit is a no-op at zero cost (returns before any query).

	if err := collector.collectUsage(context.Background()); err != nil {
		t.Fatalf("collectUsage failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestCollectUsage_MetersStorage exercises the storage cost path
// specifically — the GB-month rate converted to the collector's hourly
// tick via hoursPerMonth (730). CPU/memory rates are left at 0 so
// TotalCost is driven by storage alone, isolating the conversion; UnitPrice
// stays exactly 0 since storage cost is added directly to TotalCost, not
// folded into the per-hour unit price (see collector.go's comment on why).
func TestCollectUsage_MetersStorage(t *testing.T) {
	collector, mock := newMockCollector(t)
	accountID, projectID := uuid.New(), uuid.New()
	createdAt := time.Now().Add(-1 * time.Hour)

	// 100GB volume on an otherwise free CPU instance.
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WillReturnRows(billableInstanceRows().
			AddRow("inst-vol00001", accountID, projectID, "cpu.home", 0, 0, 0, 100, createdAt, nil))
	mock.ExpectQuery(`SELECT MAX\(end_time\)`).
		WithArgs("inst-vol00001").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))
	// $0.10/GB-month storage rate; CPU/memory both 0.
	expectPricingReadFull(mock, 0.10, 0, 0, 0.10)

	var observedHours float64
	wantCost := funcArg(func(v driver.Value) bool {
		got, ok := v.(float64)
		if !ok {
			return false
		}
		// The exact formula from collector.go: GB * rate / hoursPerMonth * hours.
		want := 100 * 0.10 / hoursPerMonth * observedHours
		return math.Abs(got-want) < 1e-9
	})

	mock.ExpectQuery(`INSERT INTO billing\.usage_records`).
		WithArgs(accountID, projectID, "inst-vol00001", "instance", "inst-vol00001",
			0.0, nil, "cpu.home",
			captureArg{&observedHours}, "hours",
			0.0, // unit price — storage never touches it, only TotalCost
			wantCost,
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow(uuid.New(), time.Now()))

	if err := collector.collectUsage(context.Background()); err != nil {
		t.Fatalf("collectUsage failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
	if observedHours <= 0 {
		t.Fatal("captureArg never observed a value — the hours argument shape changed")
	}
}

func TestCollectUsage_SkipsRecentlyCollected(t *testing.T) {
	collector, mock := newMockCollector(t)

	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WillReturnRows(billableInstanceRows().
			AddRow("inst-20gb0001", uuid.New(), uuid.New(), "gpu.h100.2g.20gb", 20, 8, 32, 0, time.Now().Add(-5*time.Hour), nil))

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
