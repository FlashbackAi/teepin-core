// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"math"
	"testing"
)

func TestCalculateVRAMCost(t *testing.T) {
	s := NewService(nil)

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
		got := s.CalculateVRAMCost(tc.vramGB, tc.hours)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("CalculateVRAMCost(%d, %.1f) = %.4f, want %.4f",
				tc.vramGB, tc.hours, got, tc.want)
		}
	}
}

func TestVRAMUnitPrice(t *testing.T) {
	s := NewService(nil)
	if got := s.VRAMUnitPrice(25); math.Abs(got-2.50) > 1e-9 {
		t.Errorf("VRAMUnitPrice(25) = %.4f, want 2.50", got)
	}
}
