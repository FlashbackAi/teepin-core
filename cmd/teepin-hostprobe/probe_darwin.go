// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build darwin

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// probeHost detects P-core/E-core counts via the same sysctl names Apple's
// own tools (powermetrics, Activity Monitor) key off: hw.perflevel0 is the
// highest-performance cluster (P-cores on Apple Silicon), hw.perflevel1 the
// efficiency cluster (E-cores). An Intel Mac, or an Apple Silicon chip with
// only one cluster, has no perflevel1 at all — treated as "no split
// detected" (0, 0), never a guess.
func probeHost() (probeResult, error) {
	pCores, pErr := sysctlUint32("hw.perflevel0.physicalcpu")
	eCores, eErr := sysctlUint32("hw.perflevel1.physicalcpu")
	if pErr != nil {
		// No perflevel0 at all means this Mac reports no heterogeneous-core
		// info (a homogeneous Intel Mac) -- not a failure, just "no split".
		pCores, eCores = 0, 0
	} else if eErr != nil {
		// A P-core reading with no E-core reading is still a real,
		// single-cluster machine -- 0 E-cores is correct here, not unknown.
		eCores = 0
	}

	memGB := 0
	if memBytes, err := sysctlUint64("hw.memsize"); err == nil {
		const gib = 1024 * 1024 * 1024
		memGB = int((memBytes + gib/2) / gib)
	}

	return probeResult{PCores: int(pCores), ECores: int(eCores), TotalMemoryGB: memGB}, nil
}

func sysctlUint32(name string) (uint32, error) {
	v, err := unix.SysctlUint32(name)
	if err != nil {
		return 0, fmt.Errorf("sysctl %s: %w", name, err)
	}
	return v, nil
}

func sysctlUint64(name string) (uint64, error) {
	v, err := unix.SysctlUint64(name)
	if err != nil {
		return 0, fmt.Errorf("sysctl %s: %w", name, err)
	}
	return v, nil
}
