// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"math"
	"testing"
)

// The quote a customer is shown is computed by the same formula the collector
// charges with, cores + memory + disk (disk per GB-month spread over 730 hours).
func TestComputeHourlyPrice_IsTheBilledFormula(t *testing.T) {
	collector, mock := newMockCollector(t)
	svc := collector.billingService
	expectPricingReadFull(mock, 0.10, 0.002, 0.001, 0.073)

	got := svc.ComputeHourlyPrice(context.Background(), 0, 4, 8, 100)
	want := 4*0.002 + 8*0.001 + 100*0.073/730
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("ComputeHourlyPrice = %v, want %v", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A GPU instance is priced on its VRAM, whatever its CPU.
func TestComputeHourlyPrice_GPUUsesVRAM(t *testing.T) {
	collector, mock := newMockCollector(t)
	expectPricingReadFull(mock, 0.10, 0.002, 0.001, 0)
	got := collector.billingService.ComputeHourlyPrice(context.Background(), 20, 8, 32, 0)
	if math.Abs(got-2.0) > 1e-12 {
		t.Errorf("GPU price = %v, want 2.0 (20GB at $0.10)", got)
	}
}
