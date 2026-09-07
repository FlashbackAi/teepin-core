// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nodes

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// SetReservation accepts a reservation within the node's detected specs.
func TestSetReservation_WithinSpecs(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()
	id := uuid.New()

	mock.ExpectQuery(`SELECT COALESCE\(cpu_cores,0\), COALESCE\(memory_gb,0\)\s+FROM compute\.nodes WHERE id`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"cpu_cores", "memory_gb"}).AddRow(32, 64))
	mock.ExpectExec(`UPDATE compute\.nodes\s+SET rentable_cpu_cores = \$1, rentable_memory_gb = \$2`).
		WithArgs(24, 48, id).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.SetReservation(context.Background(), id, 24, 48); err != nil {
		t.Fatalf("SetReservation: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// Offering more than the node has is rejected with ErrOverCommit, before any
// UPDATE runs.
func TestSetReservation_OverCommit(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()
	id := uuid.New()

	mock.ExpectQuery(`SELECT COALESCE\(cpu_cores,0\)`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"cpu_cores", "memory_gb"}).AddRow(8, 16))

	// 16 vCPU > 8 detected -> over-commit, no UPDATE expected.
	if err := s.SetReservation(context.Background(), id, 16, 8); !errors.Is(err, ErrOverCommit) {
		t.Fatalf("err = %v, want ErrOverCommit", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A missing node is ErrNotFound.
func TestSetReservation_NotFound(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()
	id := uuid.New()

	mock.ExpectQuery(`SELECT COALESCE\(cpu_cores,0\)`).
		WithArgs(id).
		WillReturnError(sql.ErrNoRows)

	if err := s.SetReservation(context.Background(), id, 4, 8); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ListNodeCapacity derives free = rentable - used, clamped at 0.
func TestListNodeCapacity_DerivesFree(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()
	id := uuid.New()

	// 24 rentable CPU, two instances holding 4+8 = 12 used -> 12 free.
	mock.ExpectQuery(`SELECT n\.id, n\.node_name.*FROM compute\.nodes n\s+LEFT JOIN`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "node_name", "class", "status",
			"cpu_cores", "memory_gb", "rentable_cpu_cores", "rentable_memory_gb",
			"used_cpu", "used_mem",
		}).AddRow(id, "srialla", "home", "online", 32, 64, 24, 48, 12, 24))

	caps, err := s.ListNodeCapacity(context.Background())
	if err != nil {
		t.Fatalf("ListNodeCapacity: %v", err)
	}
	if len(caps) != 1 {
		t.Fatalf("got %d rows, want 1", len(caps))
	}
	c := caps[0]
	if c.FreeCPU != 12 || c.FreeMemGB != 24 {
		t.Errorf("free = %d cpu / %d mem, want 12 / 24", c.FreeCPU, c.FreeMemGB)
	}
}

// fakeHiddenCounter is a canned HiddenWorkloadCounter for tests.
type fakeHiddenCounter struct {
	usage map[string]HiddenUsage
	err   error
}

func (f *fakeHiddenCounter) HiddenUsageByNode(context.Context) (map[string]HiddenUsage, error) {
	return f.usage, f.err
}

// The whole point of HiddenWorkloadCounter: a node running one of
// Teepin's own hidden pods (a Kaniko build, say) must have that pod's
// footprint counted as used, on TOP of whatever compute.instances already
// holds — not instead of it, and not ignored.
func TestListNodeCapacity_AddsHiddenWorkloadUsage(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()
	id := uuid.New()

	// Same DB shape as TestListNodeCapacity_DerivesFree: 24 rentable CPU,
	// 12 already used by real compute.instances -> 12 free before any
	// hidden-workload addition.
	mock.ExpectQuery(`SELECT n\.id, n\.node_name.*FROM compute\.nodes n\s+LEFT JOIN`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "node_name", "class", "status",
			"cpu_cores", "memory_gb", "rentable_cpu_cores", "rentable_memory_gb",
			"used_cpu", "used_mem",
		}).AddRow(id, "srialla", "home", "online", 32, 64, 24, 48, 12, 24))

	s.WithHiddenWorkloadCounter(&fakeHiddenCounter{
		usage: map[string]HiddenUsage{"srialla": {CPUCores: 2, MemoryGB: 4}},
	})

	caps, err := s.ListNodeCapacity(context.Background())
	if err != nil {
		t.Fatalf("ListNodeCapacity: %v", err)
	}
	c := caps[0]
	if c.UsedCPU != 14 || c.UsedMemGB != 28 {
		t.Errorf("used = %d cpu / %d mem, want 14 / 28 (12 DB + 2/4 hidden)", c.UsedCPU, c.UsedMemGB)
	}
	if c.FreeCPU != 10 || c.FreeMemGB != 20 {
		t.Errorf("free = %d cpu / %d mem, want 10 / 20", c.FreeCPU, c.FreeMemGB)
	}
}

// A HiddenWorkloadCounter lookup failure must degrade to DB-only
// accounting (today's behaviour), not fail the whole capacity view — a
// live-status blip must never make the control centre's node list an
// error page.
func TestListNodeCapacity_HiddenCounterErrorFailsOpen(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()
	id := uuid.New()

	mock.ExpectQuery(`SELECT n\.id, n\.node_name.*FROM compute\.nodes n\s+LEFT JOIN`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "node_name", "class", "status",
			"cpu_cores", "memory_gb", "rentable_cpu_cores", "rentable_memory_gb",
			"used_cpu", "used_mem",
		}).AddRow(id, "srialla", "home", "online", 32, 64, 24, 48, 12, 24))

	s.WithHiddenWorkloadCounter(&fakeHiddenCounter{err: errors.New("agent unreachable")})

	caps, err := s.ListNodeCapacity(context.Background())
	if err != nil {
		t.Fatalf("ListNodeCapacity should degrade gracefully, got error: %v", err)
	}
	c := caps[0]
	if c.UsedCPU != 12 || c.FreeCPU != 12 {
		t.Errorf("used/free = %d/%d, want 12/12 (DB-only, hidden lookup failed)", c.UsedCPU, c.FreeCPU)
	}
}

// HomeCapacitySummary prices each tier from the operator's per-resource
// rates (cores*cpuRate + gb*memRate), NOT the seeded tier price — so the
// quote equals the metering formula.
func TestHomeCapacitySummary_PriceFromRates(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	// One online home node with 8 vCPU / 16 GB free.
	mock.ExpectQuery(`SELECT n\.id, n\.node_name.*FROM compute\.nodes n`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "node_name", "class", "status",
			"cpu_cores", "memory_gb", "rentable_cpu_cores", "rentable_memory_gb",
			"used_cpu", "used_mem",
		}).AddRow(uuid.New(), "n", "home", "online", 32, 64, 8, 16, 0, 0))
	// Tier table (no price column selected now — price is computed).
	mock.ExpectQuery(`SELECT id, name, cpu_units, memory_gb\s+FROM compute\.instance_types`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "cpu_units", "memory_gb"}).
			AddRow("cpu.medium", "CPU Medium", 4, 8))

	// Rates: $0.02/core-hr, $0.01/GB-hr → medium = 4*0.02 + 8*0.01 = 0.16.
	summary, err := s.HomeCapacitySummary(context.Background(),
		CPURates{CPUCoreRate: 0.02, MemoryGBRate: 0.01})
	if err != nil {
		t.Fatalf("HomeCapacitySummary: %v", err)
	}
	if len(summary.Tiers) != 1 {
		t.Fatalf("got %d tiers, want 1", len(summary.Tiers))
	}
	tier := summary.Tiers[0]
	if tier.Price != 0.16 {
		t.Errorf("price = %.4f, want 0.16 (computed from rates, not seeded)", tier.Price)
	}
	if !tier.Fits {
		t.Errorf("cpu.medium (4/8) should fit in 8/16 free")
	}
}

// Free is clamped at 0 when usage exceeds a lowered reservation.
func TestListNodeCapacity_FreeClampedAtZero(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()
	id := uuid.New()

	// Reservation lowered to 4, but 8 already in use -> free 0, not -4.
	mock.ExpectQuery(`SELECT n\.id, n\.node_name.*FROM compute\.nodes n`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "node_name", "class", "status",
			"cpu_cores", "memory_gb", "rentable_cpu_cores", "rentable_memory_gb",
			"used_cpu", "used_mem",
		}).AddRow(id, "n", "home", "online", 32, 64, 4, 8, 8, 16))

	caps, _ := s.ListNodeCapacity(context.Background())
	if caps[0].FreeCPU != 0 || caps[0].FreeMemGB != 0 {
		t.Errorf("free = %d / %d, want 0 / 0 (clamped)", caps[0].FreeCPU, caps[0].FreeMemGB)
	}
}
