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

// PlaceCPU picks a home node when one is online and (if requested) arch-matched.
func TestPlaceCPU_Selects(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM compute\.nodes\s+WHERE class = 'home' AND status = 'online'`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`SELECT n\.node_name, n\.provider_id.*FROM compute\.nodes n`).
		WithArgs("amd64", true).
		WillReturnRows(sqlmock.NewRows([]string{"node_name", "provider_id", "arch"}).
			AddRow("mac-mini", "home-sreek", "amd64"))

	p, err := s.PlaceCPU(context.Background(), PlacementReq{Arch: "amd64"})
	if err != nil {
		t.Fatalf("PlaceCPU: %v", err)
	}
	if p.NodeName != "mac-mini" || p.ProviderID != "home-sreek" {
		t.Errorf("placed on %+v, want mac-mini/home-sreek", p)
	}
}

// No online home node at all → ErrNoHomeCapacity (retryable).
func TestPlaceCPU_NoCapacity(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM compute\.nodes`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))

	if _, err := s.PlaceCPU(context.Background(), PlacementReq{}); !errors.Is(err, ErrNoHomeCapacity) {
		t.Fatalf("err = %v, want ErrNoHomeCapacity", err)
	}
}

// Home nodes online, but none matches the requested arch → ErrArchUnavailable
// (a request problem, distinct from no-capacity).
func TestPlaceCPU_ArchMismatch(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM compute\.nodes`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(2))
	// Arch-filtered query returns nothing.
	mock.ExpectQuery(`SELECT n\.node_name, n\.provider_id.*FROM compute\.nodes n`).
		WithArgs("arm64", true).
		WillReturnError(sql.ErrNoRows)

	if _, err := s.PlaceCPU(context.Background(), PlacementReq{Arch: "arm64"}); !errors.Is(err, ErrArchUnavailable) {
		t.Fatalf("err = %v, want ErrArchUnavailable", err)
	}
}

// No arch requested: the filter is disabled ($2 = false) and any online home
// node is eligible.
func TestPlaceCPU_NoArchPreference(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM compute\.nodes`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`SELECT n\.node_name, n\.provider_id.*FROM compute\.nodes n`).
		WithArgs("", false).
		WillReturnRows(sqlmock.NewRows([]string{"node_name", "provider_id", "arch"}).
			AddRow("box", "prov", "amd64"))

	if _, err := s.PlaceCPU(context.Background(), PlacementReq{}); err != nil {
		t.Fatalf("PlaceCPU: %v", err)
	}
}
