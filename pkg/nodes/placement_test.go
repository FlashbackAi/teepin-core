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
