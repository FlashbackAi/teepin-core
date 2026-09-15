// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nodes

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// countRows builds the (online, arch_matched) result the placement counter
// expects.
func countRows(online, archMatched int) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"count", "count_filtered"}).AddRow(online, archMatched)
}

// PlaceCPU picks an arch-matched node with room for the requested size.
func TestPlaceCPU_Selects(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT\s+COUNT\(\*\),\s+COUNT\(\*\) FILTER`).
		WithArgs("amd64", true).
		WillReturnRows(countRows(1, 1))
	mock.ExpectQuery(`SELECT n\.node_name, n\.provider_id.*rentable_cpu_cores - COALESCE`).
		WithArgs("amd64", true, 4, 8, 0, 0, false).
		WillReturnRows(sqlmock.NewRows([]string{"node_name", "provider_id", "arch", "p_cores", "e_cores", "used_p", "used_e"}).
			AddRow("mac-mini", "home-sreek", "amd64", 0, 0, 0, 0))

	p, err := s.PlaceCPU(context.Background(), PlacementReq{Arch: "amd64", CPUUnits: 4, MemoryGB: 8})
	if err != nil {
		t.Fatalf("PlaceCPU: %v", err)
	}
	if p.NodeName != "mac-mini" || p.ProviderID != "home-sreek" {
		t.Errorf("placed on %+v, want mac-mini/home-sreek", p)
	}
}

// No online home node at all -> ErrNoHomeCapacity.
func TestPlaceCPU_NoCapacity(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT\s+COUNT\(\*\)`).
		WithArgs("", false).
		WillReturnRows(countRows(0, 0))

	if _, err := s.PlaceCPU(context.Background(), PlacementReq{}); !errors.Is(err, ErrNoHomeCapacity) {
		t.Fatalf("err = %v, want ErrNoHomeCapacity", err)
	}
}

// Home nodes online, but none matches the requested arch -> ErrArchUnavailable.
func TestPlaceCPU_ArchMismatch(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	// 2 online, 0 arch-matched.
	mock.ExpectQuery(`SELECT\s+COUNT\(\*\)`).
		WithArgs("arm64", true).
		WillReturnRows(countRows(2, 0))

	if _, err := s.PlaceCPU(context.Background(), PlacementReq{Arch: "arm64"}); !errors.Is(err, ErrArchUnavailable) {
		t.Fatalf("err = %v, want ErrArchUnavailable", err)
	}
}

// Arch-matched nodes exist but none has room -> ErrInsufficientCapacity.
func TestPlaceCPU_InsufficientCapacity(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT\s+COUNT\(\*\)`).
		WithArgs("amd64", true).
		WillReturnRows(countRows(1, 1))
	// The fit query finds no node with enough free capacity.
	mock.ExpectQuery(`SELECT n\.node_name.*rentable_cpu_cores`).
		WithArgs("amd64", true, 8, 16, 0, 0, false).
		WillReturnError(sql.ErrNoRows)

	_, err := s.PlaceCPU(context.Background(), PlacementReq{Arch: "amd64", CPUUnits: 8, MemoryGB: 16})
	if !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("err = %v, want ErrInsufficientCapacity", err)
	}
}

// Both PlaceCPU queries filter on k8s_ready = TRUE, not just status =
// 'online' — a node whose agent is connected but whose local Kubernetes is
// unreachable must be excluded from placement the same as an offline node.
// This asserts the predicate is actually present in both emitted queries
// (sqlmock's query-regexp matching only succeeds if the SQL text matches),
// which is what proves the code emits the new gate rather than merely that
// some row-count arithmetic happens to work out.
func TestPlaceCPU_QueriesGateOnK8sReady(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT\s+COUNT\(\*\).*WHERE class = 'home' AND status = 'online' AND k8s_ready = TRUE`).
		WithArgs("amd64", true).
		WillReturnRows(countRows(1, 1))
	mock.ExpectQuery(`(?s)SELECT n\.node_name.*WHERE n\.class = 'home' AND n\.status = 'online' AND n\.k8s_ready = TRUE`).
		WithArgs("amd64", true, 4, 8, 0, 0, false).
		WillReturnRows(sqlmock.NewRows([]string{"node_name", "provider_id", "arch", "p_cores", "e_cores", "used_p", "used_e"}).
			AddRow("srialla", "home-sreek", "amd64", 0, 0, 0, 0))

	p, err := s.PlaceCPU(context.Background(), PlacementReq{Arch: "amd64", CPUUnits: 4, MemoryGB: 8})
	if err != nil {
		t.Fatalf("PlaceCPU: %v", err)
	}
	if p.NodeName != "srialla" {
		t.Errorf("placed on %+v, want srialla", p)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v (the k8s_ready predicate may be missing from the SQL)", err)
	}
}

// A node that is online but not k8s_ready (its agent is connected but its
// local Kubernetes is unreachable) must not be selectable. Because the
// gating predicate lives in the SQL itself (proven above), the DB-level
// effect of excluding such a node is: it contributes nothing to either
// query's result set — indistinguishable, from PlaceCPU's point of view,
// from a fully offline node. This locks in that PlaceCPU then reports the
// SAME error a genuinely offline node would (ErrNoHomeCapacity /
// ErrInsufficientCapacity), not a different, misleading one.
func TestPlaceCPU_NotK8sReady_TreatedAsNoCapacity(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT\s+COUNT\(\*\)`).
		WithArgs("", false).
		WillReturnRows(countRows(0, 0))

	if _, err := s.PlaceCPU(context.Background(), PlacementReq{}); !errors.Is(err, ErrNoHomeCapacity) {
		t.Fatalf("err = %v, want ErrNoHomeCapacity (a not-ready node must not count as online capacity)", err)
	}
}

// The fit query gates on k8s_ready too — a node online, arch-matched, AND
// with room, but not ready, must still be excluded from the fit result, not
// just the initial count.
func TestPlaceCPU_NotK8sReady_ExcludedFromFitQuery(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT\s+COUNT\(\*\)`).
		WithArgs("amd64", true).
		WillReturnRows(countRows(1, 1))
	mock.ExpectQuery(`SELECT n\.node_name.*rentable_cpu_cores`).
		WithArgs("amd64", true, 4, 8, 0, 0, false).
		WillReturnError(sql.ErrNoRows)

	_, err := s.PlaceCPU(context.Background(), PlacementReq{Arch: "amd64", CPUUnits: 4, MemoryGB: 8})
	if !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("err = %v, want ErrInsufficientCapacity", err)
	}
}

// No arch preference: the filter is disabled ($2 = false).
func TestPlaceCPU_NoArchPreference(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT\s+COUNT\(\*\)`).
		WithArgs("", false).
		WillReturnRows(countRows(1, 1))
	mock.ExpectQuery(`SELECT n\.node_name`).
		WithArgs("", false, 2, 4, 0, 0, false).
		WillReturnRows(sqlmock.NewRows([]string{"node_name", "provider_id", "arch", "p_cores", "e_cores", "used_p", "used_e"}).
			AddRow("box", "prov", "amd64", 0, 0, 0, 0))

	if _, err := s.PlaceCPU(context.Background(), PlacementReq{CPUUnits: 2, MemoryGB: 4}); err != nil {
		t.Fatalf("PlaceCPU: %v", err)
	}
}

// A node with NO detected P/E split (p_cores/e_cores both 0) is placed
// exactly as before this feature — PCoresUsed/ECoresUsed stay nil, even
// when the node has a real split column that just reads 0/0 (undetected).
func TestPlaceCPU_NoDetectedSplit_LeavesPCoresUsedNil(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT\s+COUNT\(\*\)`).
		WithArgs("", false).
		WillReturnRows(countRows(1, 1))
	mock.ExpectQuery(`SELECT n\.node_name`).
		WithArgs("", false, 4, 8, 0, 0, false).
		WillReturnRows(sqlmock.NewRows([]string{"node_name", "provider_id", "arch", "p_cores", "e_cores", "used_p", "used_e"}).
			AddRow("plain-box", "prov", "amd64", 0, 0, 0, 0))

	p, err := s.PlaceCPU(context.Background(), PlacementReq{CPUUnits: 4, MemoryGB: 8})
	if err != nil {
		t.Fatalf("PlaceCPU: %v", err)
	}
	if p.PCoresUsed != nil || p.ECoresUsed != nil {
		t.Errorf("PCoresUsed/ECoresUsed = %v/%v, want nil/nil for a node with no detected split", p.PCoresUsed, p.ECoresUsed)
	}
}

// An explicit P/E request is passed through as PCoresUsed/ECoresUsed
// verbatim once the node has been capacity-checked against it — the WHERE
// clause is what proves the request was actually validated, not merely
// echoed back.
func TestPlaceCPU_ExplicitSplit_HonouredAndCapacityChecked(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT\s+COUNT\(\*\)`).
		WithArgs("", false).
		WillReturnRows(countRows(1, 1))
	// wantSplit=true and the explicit 4/2 values must reach the query —
	// sqlmock's WithArgs match fails this test if PlaceCPU stops actually
	// threading the request through.
	mock.ExpectQuery(`SELECT n\.node_name`).
		WithArgs("", false, 6, 8, 4, 2, true).
		WillReturnRows(sqlmock.NewRows([]string{"node_name", "provider_id", "arch", "p_cores", "e_cores", "used_p", "used_e"}).
			AddRow("hybrid-box", "prov", "amd64", 8, 16, 0, 0))

	p, err := s.PlaceCPU(context.Background(), PlacementReq{CPUUnits: 6, MemoryGB: 8, PCores: 4, ECores: 2})
	if err != nil {
		t.Fatalf("PlaceCPU: %v", err)
	}
	if p.PCoresUsed == nil || p.ECoresUsed == nil || *p.PCoresUsed != 4 || *p.ECoresUsed != 2 {
		t.Fatalf("PCoresUsed/ECoresUsed = %v/%v, want 4/2 (the caller's own explicit request)", p.PCoresUsed, p.ECoresUsed)
	}
}

// No customer preference, but the chosen node HAS a detected split: the
// default is a PROPORTIONAL split of what is actually free on that node,
// not a fixed 50/50.
func TestPlaceCPU_NoPreference_DefaultsToProportionalSplit(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT\s+COUNT\(\*\)`).
		WithArgs("", false).
		WillReturnRows(countRows(1, 1))
	// Node has 8 P-cores / 4 E-cores free (12 total, matching the 12
	// requested cpu_units) — a 50/50 split would be wrong; proportional to
	// 8:4 free gives 8 P-cores / 4 E-cores.
	mock.ExpectQuery(`SELECT n\.node_name`).
		WithArgs("", false, 12, 8, 0, 0, false).
		WillReturnRows(sqlmock.NewRows([]string{"node_name", "provider_id", "arch", "p_cores", "e_cores", "used_p", "used_e"}).
			AddRow("hybrid-box", "prov", "amd64", 8, 4, 0, 0))

	p, err := s.PlaceCPU(context.Background(), PlacementReq{CPUUnits: 12, MemoryGB: 8})
	if err != nil {
		t.Fatalf("PlaceCPU: %v", err)
	}
	if p.PCoresUsed == nil || p.ECoresUsed == nil || *p.PCoresUsed != 8 || *p.ECoresUsed != 4 {
		t.Fatalf("PCoresUsed/ECoresUsed = %v/%v, want 8/4 (proportional to 8:4 free)", p.PCoresUsed, p.ECoresUsed)
	}
}

func TestProportionalSplit(t *testing.T) {
	cases := []struct {
		name                string
		total, freeP, freeE int
		wantP, wantE        int
	}{
		{"even split", 10, 10, 10, 5, 5},
		{"more P free than E", 12, 8, 4, 8, 4},
		{"more E free than P", 12, 4, 8, 4, 8},
		{"only P free", 6, 10, 0, 6, 0},
		{"only E free", 6, 0, 10, 0, 6},
		{"rounds, never exceeds total", 5, 1, 2, 2, 3},
		{"defensive: both free zero never divides by zero", 4, 0, 0, 0, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, e := proportionalSplit(tc.total, tc.freeP, tc.freeE)
			if p != tc.wantP || e != tc.wantE {
				t.Fatalf("proportionalSplit(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tc.total, tc.freeP, tc.freeE, p, e, tc.wantP, tc.wantE)
			}
			if p+e != tc.total {
				t.Fatalf("proportionalSplit(%d, %d, %d) = (%d, %d), sum %d != total %d",
					tc.total, tc.freeP, tc.freeE, p, e, p+e, tc.total)
			}
		})
	}
}
