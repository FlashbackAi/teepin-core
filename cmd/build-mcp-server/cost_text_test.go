// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"strings"
	"testing"
)

func TestHourlyCostText(t *testing.T) {
	if got := hourlyCostText(0.0123); got != "Cost: $0.0123/hour while running." {
		t.Errorf("got %q", got)
	}
	for _, zero := range []float64{0, -1} {
		got := hourlyCostText(zero)
		if strings.Contains(got, "$0.0000") || !strings.Contains(got, "No hourly compute charge") {
			t.Errorf("a missing price must not read as $0.0000/hour: %q", got)
		}
	}
}
