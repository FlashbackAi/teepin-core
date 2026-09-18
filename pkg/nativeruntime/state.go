// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nativeruntime

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// pidRecord is what survives an agent restart: enough to recognise our own
// orphan and nothing else.
type pidRecord struct {
	PID  int    `json:"pid"`
	Exe  string `json:"exe"` // argv[0], to guard against pid reuse
	Name string `json:"instance_id"`
}

func (r *Runtime) pidFile() string { return filepath.Join(r.cfg.StateDir, "instances.json") }

// persistPIDs rewrites the registry of live child processes. Best-effort:
// failing to write it costs the ability to reap an orphan after a crash,
// not the ability to run, so it logs rather than failing the caller.
func (r *Runtime) persistPIDs() {
	r.mu.Lock()
	records := make([]pidRecord, 0, len(r.instances))
	for id, inst := range r.instances {
		if inst.cmd == nil || inst.cmd.Process == nil {
			continue
		}
		if inst.state != statusPending && inst.state != statusRunning {
			continue
		}
		records = append(records, pidRecord{PID: inst.cmd.Process.Pid, Exe: inst.cmd.Path, Name: id})
	}
	r.mu.Unlock()

	data, err := json.Marshal(records)
	if err != nil {
		return
	}
	if err := os.WriteFile(r.pidFile(), data, 0o600); err != nil {
		log.Printf("nativeruntime: could not persist pid registry: %v", err)
	}
}

// processCommand returns the command line of a live pid, or "" if it is
// gone. A variable so tests can stand in for `ps`.
var processCommand = func(pid int) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// stopPID is a variable so tests can observe which pids would be signalled
// without ever signalling a real process.
var stopPID = terminatePID

// reapStale stops processes a previous run of this agent started and never
// got to stop. A pid is only killed if its live command line still contains
// the executable we recorded — pids are reused, and an unrelated process
// that happens to hold an old number must never be signalled.
func (r *Runtime) reapStale() {
	data, err := os.ReadFile(r.pidFile())
	if err != nil {
		return
	}
	var records []pidRecord
	if json.Unmarshal(data, &records) != nil {
		return
	}
	for _, rec := range records {
		if rec.PID <= 0 || rec.Exe == "" {
			continue
		}
		cmdline := processCommand(rec.PID)
		if cmdline == "" || !strings.Contains(cmdline, filepath.Base(rec.Exe)) {
			continue
		}
		log.Printf("nativeruntime: stopping orphaned instance %s (pid %d) left by a previous run", rec.Name, rec.PID)
		stopPID(rec.PID)
	}
	_ = os.Remove(r.pidFile())
}
