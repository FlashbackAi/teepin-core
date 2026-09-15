// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// teepin-hostprobe detects a home node's real host-level P-core/E-core
// split and total memory, run BEFORE the home-node bootstrap creates its
// Linux guest (WSL2 on Windows, a Lima VM on macOS).
//
// It exists because teepin-agent itself only ever runs as Linux (see its
// Dockerfile) — on Windows/macOS it always runs inside that Linux guest,
// never natively on the host OS, so it cannot call Windows/macOS-native
// detection APIs itself, and the guest's own view of CPU topology is not
// reliably the host's real one (the hypervisor may not expose true
// core-type information to the guest at all). This binary runs on the real
// host OS instead, using each platform's own native API, and its result is
// handed into the guest by the bootstrap script — see deploy/agent/
// bootstrap-windows.ps1 and bootstrap-macos.sh.
//
// A native Linux home node needs neither this binary nor a VM: the agent
// runs directly on the host there, so it can read
// /sys/devices/system/cpu/cpu*/topology/ itself (see cmd/teepin-agent's own
// hostSpecs()).
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// probeResult is printed to stdout as one JSON line. PCores/ECores are both
// 0 when no split was detected (a homogeneous CPU, or a platform/OS build
// where detection is not implemented) — never a guess.
type probeResult struct {
	PCores        int `json:"p_cores"`
	ECores        int `json:"e_cores"`
	TotalMemoryGB int `json:"total_memory_gb"`
}

// probeHost is implemented per-OS in probe_windows.go / probe_darwin.go,
// with probe_other.go as the fallback for every other GOOS — this keeps
// `go build ./...` clean on every platform this repo is built on (Linux CI,
// a Windows dev machine) without needing a build tag on this file itself.
func main() {
	result, err := probeHost()
	if err != nil {
		fmt.Fprintf(os.Stderr, "teepin-hostprobe: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "teepin-hostprobe: failed to encode result: %v\n", err)
		os.Exit(1)
	}
}
