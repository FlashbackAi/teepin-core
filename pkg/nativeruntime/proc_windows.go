// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build windows

package nativeruntime

import (
	"os"
	"os/exec"
)

// The native runtime targets macOS; this file exists so the package (and
// its tests) still build and run on a Windows development machine. There
// is no process-group signalling here — Kill is the only stop.

func configureProcess(*exec.Cmd) {}

func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func forceKill(cmd *exec.Cmd) { terminate(cmd) }

func terminatePID(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
