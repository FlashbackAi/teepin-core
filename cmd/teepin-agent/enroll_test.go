// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPeCoresFromEnv(t *testing.T) {
	cases := []struct {
		name       string
		pEnv, eEnv string
		wantP      int
		wantE      int
		wantOK     bool
	}{
		{"both set", "8", "16", 8, 16, true},
		{"e-cores zero is still valid (single-cluster host)", "8", "0", 8, 0, true},
		{"p-cores missing", "", "16", 0, 0, false},
		{"e-cores missing", "8", "", 0, 0, false},
		{"neither set", "", "", 0, 0, false},
		{"p-cores non-numeric", "eight", "16", 0, 0, false},
		{"p-cores zero is not a real split", "0", "16", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEEPIN_PCORES", tc.pEnv)
			t.Setenv("TEEPIN_ECORES", tc.eEnv)
			p, e, ok := peCoresFromEnv()
			if p != tc.wantP || e != tc.wantE || ok != tc.wantOK {
				t.Fatalf("peCoresFromEnv() = (%d, %d, %v), want (%d, %d, %v)",
					p, e, ok, tc.wantP, tc.wantE, tc.wantOK)
			}
		})
	}
}

// withFakeSysCPU builds a fixture /sys/devices/system/cpu-shaped directory
// with a cpu_capacity file for each entry in capacities (nil skips writing
// the file for that core, simulating a kernel that does not expose it),
// swaps sysCPUDir to point at it, and restores the original on cleanup.
func withFakeSysCPU(t *testing.T, capacities map[string]*int) {
	t.Helper()
	dir := t.TempDir()
	for cpu, capacity := range capacities {
		cpuDir := filepath.Join(dir, cpu)
		if err := os.MkdirAll(cpuDir, 0o755); err != nil {
			t.Fatalf("failed to create fixture dir: %v", err)
		}
		if capacity == nil {
			continue // no cpu_capacity file for this core
		}
		content := []byte(strconv.Itoa(*capacity) + "\n")
		if err := os.WriteFile(filepath.Join(cpuDir, "cpu_capacity"), content, 0o644); err != nil {
			t.Fatalf("failed to write fixture file: %v", err)
		}
	}
	original := sysCPUDir
	sysCPUDir = dir
	t.Cleanup(func() { sysCPUDir = original })
}

func TestPeCoresFromLinuxTopology(t *testing.T) {
	p1024, p1024b, p1024c, p1024d := 1024, 1024, 1024, 1024
	e512, e512b, e512c, e512d := 512, 512, 512, 512

	t.Run("two distinct capacities -> a real P/E split", func(t *testing.T) {
		withFakeSysCPU(t, map[string]*int{
			"cpu0": &p1024, "cpu1": &p1024b, "cpu2": &e512, "cpu3": &e512b, "cpu4": &e512c,
		})
		p, e := peCoresFromLinuxTopology()
		if p != 2 || e != 3 {
			t.Fatalf("peCoresFromLinuxTopology() = (%d, %d), want (2, 3)", p, e)
		}
	})

	t.Run("homogeneous capacity -> no split detected", func(t *testing.T) {
		withFakeSysCPU(t, map[string]*int{"cpu0": &p1024c, "cpu1": &p1024d})
		p, e := peCoresFromLinuxTopology()
		if p != 0 || e != 0 {
			t.Fatalf("peCoresFromLinuxTopology() = (%d, %d), want (0, 0) for a homogeneous CPU", p, e)
		}
	})

	t.Run("three distinct capacities -> not confident, no split reported", func(t *testing.T) {
		lo, mid, hi := 256, 512, 1024
		withFakeSysCPU(t, map[string]*int{"cpu0": &lo, "cpu1": &mid, "cpu2": &hi})
		p, e := peCoresFromLinuxTopology()
		if p != 0 || e != 0 {
			t.Fatalf("peCoresFromLinuxTopology() = (%d, %d), want (0, 0) for 3+ clusters", p, e)
		}
	})

	t.Run("one core missing cpu_capacity -> not confident, no split reported", func(t *testing.T) {
		withFakeSysCPU(t, map[string]*int{"cpu0": &p1024, "cpu1": nil, "cpu2": &e512d})
		p, e := peCoresFromLinuxTopology()
		if p != 0 || e != 0 {
			t.Fatalf("peCoresFromLinuxTopology() = (%d, %d), want (0, 0) when any core lacks the file", p, e)
		}
	})

	t.Run("no /sys directory at all -> no split detected", func(t *testing.T) {
		original := sysCPUDir
		sysCPUDir = filepath.Join(t.TempDir(), "does-not-exist")
		t.Cleanup(func() { sysCPUDir = original })
		p, e := peCoresFromLinuxTopology()
		if p != 0 || e != 0 {
			t.Fatalf("peCoresFromLinuxTopology() = (%d, %d), want (0, 0)", p, e)
		}
	})
}
