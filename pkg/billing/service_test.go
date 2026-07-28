// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"math"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestCalculateVRAMCost(t *testing.T) {
	// nil DB → compiled-in default rate ($0.10/GB-hour).
	s := NewService(nil)
	ctx := context.Background()

	cases := []struct {
		vramGB int
		hours  float64
		want   float64
	}{
		{10, 1, 1.00},
		{20, 1, 2.00},
		{25, 1, 2.50}, // custom size — the old rate table returned $0 here
		{25, 2, 5.00},
		{80, 0.5, 4.00},
		{0, 10, 0.00}, // CPU-only: no GPU cost
	}
	for _, tc := range cases {
		got := s.CalculateVRAMCost(ctx, tc.vramGB, tc.hours)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("CalculateVRAMCost(%d, %.1f) = %.4f, want %.4f",
				tc.vramGB, tc.hours, got, tc.want)
		}
	}
}

func TestVRAMUnitPrice(t *testing.T) {
	s := NewService(nil)
	if got := s.VRAMUnitPrice(context.Background(), 25); math.Abs(got-2.50) > 1e-9 {
		t.Errorf("VRAMUnitPrice(25) = %.4f, want 2.50", got)
	}
}

func TestVRAMPricePerGBHour_ReadsLiveRate(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := NewService(db)

	// Admin raised the rate to $0.25/GB-hour — allocations must see it.
	mock.ExpectQuery(`SELECT vram_price_per_gb_hour FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"vram_price_per_gb_hour"}).AddRow(0.25))

	if got := s.VRAMPricePerGBHour(context.Background()); math.Abs(got-0.25) > 1e-9 {
		t.Errorf("VRAMPricePerGBHour() = %.4f, want 0.25", got)
	}

	// A second call must hit the database again (no caching).
	mock.ExpectQuery(`SELECT vram_price_per_gb_hour FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"vram_price_per_gb_hour"}).AddRow(0.30))

	if got := s.VRAMPricePerGBHour(context.Background()); math.Abs(got-0.30) > 1e-9 {
		t.Errorf("VRAMPricePerGBHour() second read = %.4f, want 0.30", got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestVRAMPricePerGBHour_FallsBackOnError(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := NewService(db)

	// Table missing / query failure must never break allocation —
	// fall back to the compiled-in default.
	mock.ExpectQuery(`SELECT vram_price_per_gb_hour FROM billing\.pricing`).
		WillReturnError(context.DeadlineExceeded)

	if got := s.VRAMPricePerGBHour(context.Background()); math.Abs(got-0.10) > 1e-9 {
		t.Errorf("VRAMPricePerGBHour() on error = %.4f, want default 0.10", got)
	}
}

func TestSetVRAMPricePerGBHour_RejectsNonPositive(t *testing.T) {
	s := NewService(nil)
	for _, price := range []float64{0, -0.10} {
		if err := s.SetVRAMPricePerGBHour(context.Background(), price, "test"); err == nil {
			t.Errorf("SetVRAMPricePerGBHour(%.2f) accepted, want error", price)
		}
	}
}
