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
		WithArgs("amd64", true, 4, 8).
		WillReturnRows(sqlmock.NewRows([]string{"node_name", "provider_id", "arch"}).
			AddRow("mac-mini", "home-sreek", "amd64"))

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
		WithArgs("amd64", true, 8, 16).
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
		WithArgs("amd64", true, 4, 8).
		WillReturnRows(sqlmock.NewRows([]string{"node_name", "provider_id", "arch"}).
			AddRow("srialla", "home-sreek", "amd64"))

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
		WithArgs("amd64", true, 4, 8).
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
		WithArgs("", false, 2, 4).
		WillReturnRows(sqlmock.NewRows([]string{"node_name", "provider_id", "arch"}).
			AddRow("box", "prov", "amd64"))

	if _, err := s.PlaceCPU(context.Background(), PlacementReq{CPUUnits: 2, MemoryGB: 4}); err != nil {
		t.Fatalf("PlaceCPU: %v", err)
	}
}
