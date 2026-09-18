// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build !windows

package nativeruntime

import (
	"os/exec"
	"syscall"
)

// configureProcess puts the child in its own process group so stopping it
// stops everything it spawned too — a workload started through a launcher
// script or a `uv tool` shim would otherwise leave the real server
// running after the shim itself exits.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

func forceKill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// terminatePID stops a process group left behind by a previous run of the
// agent. The pid doubles as the group id because configureProcess made
// each child its own group leader.
func terminatePID(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
}
