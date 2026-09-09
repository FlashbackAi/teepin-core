// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package objectstore

import (
	"testing"
	"time"
)

func cycle(ok bool, ago time.Duration) []ProbeResult {
	base := time.Now().Add(-ago)
	errMsg := ""
	if !ok {
		errMsg = "simulated probe failure"
	}
	return []ProbeResult{
		{Backend: "shelby", Probe: "put", OK: ok, LatencyMS: 10, Error: errMsg, CheckedAt: base},
		{Backend: "shelby", Probe: "get", OK: ok, LatencyMS: 20, Error: errMsg, CheckedAt: base.Add(time.Second)},
		{Backend: "shelby", Probe: "delete", OK: ok, LatencyMS: 5, Error: errMsg, CheckedAt: base.Add(2 * time.Second)},
	}
}

// newestFirst concatenates cycles in the order Store.RecentProbeResults
// returns them (most recent first) — CALLERS MUST PASS CYCLES NEWEST
// FIRST (e.g. newestFirst(cycle(..., 5*time.Minute), cycle(..., 15*time.Minute))).
func newestFirst(cycles ...[]ProbeResult) []ProbeResult {
	var out []ProbeResult
	for _, c := range cycles {
		// Each cycle's own put/get/delete are appended oldest-first above
		// for readability; reverse so each cycle's own three ops are
		// newest-first too.
		for i := len(c) - 1; i >= 0; i-- {
			out = append(out, c[i])
		}
	}
	return out
}

func TestDeriveHealthStatus_NoData(t *testing.T) {
	got := DeriveHealthStatus("shelby", nil)
	if got.Status != HealthStatusUnknown {
		t.Fatalf("expected unknown with no data, got %s", got.Status)
	}
}

func TestDeriveHealthStatus_AllRecentPass(t *testing.T) {
	recent := newestFirst(cycle(true, 5*time.Minute), cycle(true, 10*time.Minute), cycle(true, 15*time.Minute))
	got := DeriveHealthStatus("shelby", recent)
	if got.Status != HealthStatusHealthy {
		t.Fatalf("expected healthy, got %s", got.Status)
	}
	if got.LatencyMS["put"] == 0 {
		t.Error("expected a recorded put latency")
	}
}

// TestDeriveHealthStatus_MostRecentCycleTotalFailureIsDown proves a
// currently-failing backend (the case that actually matters — Shelby is
// down RIGHT NOW) is reported as down even if it was healthy earlier.
func TestDeriveHealthStatus_MostRecentCycleTotalFailureIsDown(t *testing.T) {
	recent := newestFirst(cycle(false, 0), cycle(true, 10*time.Minute), cycle(true, 15*time.Minute))
	got := DeriveHealthStatus("shelby", recent)
	if got.Status != HealthStatusDown {
		t.Fatalf("expected down when the most recent cycle totally failed, got %s", got.Status)
	}
	if got.LastError == "" {
		t.Error("expected LastError to be populated")
	}
}

// TestDeriveHealthStatus_OneFlakyOpIsDegradedNotDown proves a single
// failed op within the window (not a total-failure cycle) is degraded,
// not down — absorbing one flaky probe without crying wolf.
func TestDeriveHealthStatus_OneFlakyOpIsDegradedNotDown(t *testing.T) {
	mixedCycle := cycle(true, 5*time.Minute)
	mixedCycle[1].OK = false // the "get" in the most recent cycle failed
	mixedCycle[1].Error = "transient blip"

	recent := newestFirst(mixedCycle, cycle(true, 10*time.Minute), cycle(true, 15*time.Minute))
	got := DeriveHealthStatus("shelby", recent)
	if got.Status != HealthStatusDegraded {
		t.Fatalf("expected degraded for a single failed op in the most recent cycle, got %s", got.Status)
	}
}

// TestDeriveHealthStatus_OldFailureOutsideWindowIsHealthy proves a
// failure old enough to have scrolled out of the 3-cycle window no longer
// affects the current status.
func TestDeriveHealthStatus_OldFailureOutsideWindowIsHealthy(t *testing.T) {
	recent := newestFirst(
		cycle(true, 5*time.Minute),
		cycle(true, 10*time.Minute),
		cycle(true, 15*time.Minute),
		cycle(false, 40*time.Minute), // old failure, outside the 3-cycle window
	)
	got := DeriveHealthStatus("shelby", recent)
	if got.Status != HealthStatusHealthy {
		t.Fatalf("expected healthy once the old failure is outside the window, got %s", got.Status)
	}
}

func TestDeriveHealthStatus_RecentIsCappedForDisplay(t *testing.T) {
	var many []ProbeResult
	for i := 0; i < 10; i++ {
		many = append(many, cycle(true, time.Duration(i)*time.Minute)...)
	}
	got := DeriveHealthStatus("shelby", many)
	if len(got.Recent) != 20 {
		t.Fatalf("expected Recent capped at 20, got %d", len(got.Recent))
	}
}
