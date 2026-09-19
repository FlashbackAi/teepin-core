// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nativeruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// TestHelperProcess is not a test: it is the workload the tests below
// launch, by re-executing this very test binary — the standard way to get
// a real, portable child process without depending on any external tool.
// Modes: "serve <port>" listens until killed; "die" prints and exits 3.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("NATIVERUNTIME_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a != "--" {
			continue
		}
		mode := args[i+1]
		switch mode {
		case "serve":
			fmt.Println("helper: starting up")
			l, err := net.Listen("tcp", "127.0.0.1:"+args[i+2])
			if err != nil {
				fmt.Println("helper: listen failed:", err)
				os.Exit(2)
			}
			fmt.Println("helper: listening on", l.Addr())
			for {
				c, err := l.Accept()
				if err != nil {
					os.Exit(0)
				}
				_ = c.Close()
			}
		case "die":
			fmt.Println("helper: fatal: model not found")
			os.Exit(3)
		}
	}
	os.Exit(0)
}

func helperSpec(id, mode string, extra ...string) cluster.InstanceSpec {
	cmd := append([]string{os.Args[0], "-test.run=TestHelperProcess", "--", mode}, extra...)
	return cluster.InstanceSpec{
		InstanceID: id,
		Command:    cmd,
		Env:        map[string]string{"NATIVERUNTIME_HELPER": "1"},
		Labels:     map[string]string{hiddenLabel: "true"},
	}
}

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	rt, err := New(Config{StateDir: t.TempDir(), NodeName: "test-mac", StopGrace: 2 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		rt.mu.Lock()
		ids := make([]string, 0, len(rt.instances))
		for id := range rt.instances {
			ids = append(ids, id)
		}
		rt.mu.Unlock()
		for _, id := range ids {
			_ = rt.DeleteInstance(context.Background(), cluster.AllTenants(), id)
		}
	})
	return rt
}

func waitForStatus(t *testing.T, rt *Runtime, id, want string) *cluster.InstanceStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last *cluster.InstanceStatus
	for time.Now().Before(deadline) {
		st, err := rt.GetInstanceStatus(context.Background(), cluster.AllTenants(), id)
		if err == nil {
			last = st
			if st.Status == want {
				return st
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("instance %s never reached %q (last: %+v)", id, want, last)
	return nil
}

// TestCreate_SubstitutesAllocatedPortAndBecomesRunning is the core
// contract: the runtime, not the caller, picks the port; the placeholder
// reaches the child; "pending" becomes "running" only once the child is
// really listening; and the resolved address is where it listens.
func TestCreate_SubstitutesAllocatedPortAndBecomesRunning(t *testing.T) {
	rt := newTestRuntime(t)

	if _, err := rt.CreateInstance(context.Background(), helperSpec("infsvc-a", "serve", PortPlaceholder)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	waitForStatus(t, rt, "infsvc-a", statusRunning)

	addr, err := rt.ResolveInstanceAddress(context.Background(), "infsvc-a", 8000)
	if err != nil {
		t.Fatalf("ResolveInstanceAddress: %v", err)
	}
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("resolved address %s is not actually listening: %v", addr, err)
	}
	_ = conn.Close()
}

func TestCreate_IsIdempotentWhileAlive(t *testing.T) {
	rt := newTestRuntime(t)
	spec := helperSpec("infsvc-b", "serve", PortPlaceholder)

	first, err := rt.CreateInstance(context.Background(), spec)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := rt.CreateInstance(context.Background(), spec)
	if err != nil {
		t.Fatalf("redelivered create: %v", err)
	}
	if first.PodName != second.PodName {
		t.Errorf("a redelivered create started a second process: %q vs %q", first.PodName, second.PodName)
	}
}

func TestCreate_RejectsWhatItCannotRun(t *testing.T) {
	rt := newTestRuntime(t)

	if _, err := rt.CreateInstance(context.Background(), cluster.InstanceSpec{InstanceID: "x", Image: "nginx"}); err == nil {
		t.Error("an image-only spec (no Command) was accepted")
	}
	spec := helperSpec("y", "serve", PortPlaceholder)
	spec.InitContainer = &cluster.InitContainerSpec{Image: "curl"}
	if _, err := rt.CreateInstance(context.Background(), spec); err == nil {
		t.Error("an init container was accepted")
	}
}

// TestFailedProcessReportsLogTail proves the failure reason is what the
// process actually said, not just an exit code — "model not found" is what
// an operator needs to see in Control Centre.
func TestFailedProcessReportsLogTail(t *testing.T) {
	rt := newTestRuntime(t)

	if _, err := rt.CreateInstance(context.Background(), helperSpec("infsvc-c", "die")); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	st := waitForStatus(t, rt, "infsvc-c", statusFailed)
	if !strings.Contains(st.Message, "model not found") {
		t.Errorf("failure message %q does not carry the process's own last output", st.Message)
	}
}

func TestDelete_StopsProcessAndIsIdempotent(t *testing.T) {
	rt := newTestRuntime(t)

	if _, err := rt.CreateInstance(context.Background(), helperSpec("infsvc-d", "serve", PortPlaceholder)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	waitForStatus(t, rt, "infsvc-d", statusRunning)
	addr, _ := rt.ResolveInstanceAddress(context.Background(), "infsvc-d", 0)

	if err := rt.DeleteInstance(context.Background(), cluster.AllTenants(), "infsvc-d"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if _, err := rt.GetInstanceStatus(context.Background(), cluster.AllTenants(), "infsvc-d"); !errors.Is(err, cluster.ErrNotFound) {
		t.Errorf("deleted instance still visible: %v", err)
	}
	if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Error("process is still listening after DeleteInstance")
	}
	if err := rt.DeleteInstance(context.Background(), cluster.AllTenants(), "infsvc-d"); err != nil {
		t.Errorf("deleting an already-deleted instance must succeed, got %v", err)
	}
}

// TestList_HidesTeepinOwnWorkloadsUnlessAsked mirrors DirectClient's
// listing rule: a model server is Teepin's own workload and must not
// appear in a customer-facing list, but the agent's own status sweep
// (IncludeHidden) must still see it or the control plane never learns its
// state.
func TestList_HidesTeepinOwnWorkloadsUnlessAsked(t *testing.T) {
	rt := newTestRuntime(t)
	if _, err := rt.CreateInstance(context.Background(), helperSpec("infsvc-e", "serve", PortPlaceholder)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	visible, _ := rt.ListInstanceStatuses(context.Background(), cluster.AllTenants())
	if len(visible) != 0 {
		t.Errorf("a hidden workload appeared in the default listing: %+v", visible)
	}
	all, _ := rt.ListInstanceStatuses(context.Background(), cluster.AllTenantsIncludingHidden())
	if len(all) != 1 || !all[0].Hidden {
		t.Errorf("IncludeHidden listing = %+v, want the one hidden instance", all)
	}
}

func TestStreamLogs_ReturnsProcessOutput(t *testing.T) {
	rt := newTestRuntime(t)
	if _, err := rt.CreateInstance(context.Background(), helperSpec("infsvc-f", "serve", PortPlaceholder)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	waitForStatus(t, rt, "infsvc-f", statusRunning)

	var buf bytes.Buffer
	if err := rt.StreamLogs(context.Background(), cluster.AllTenants(), "infsvc-f", cluster.LogOptions{TailLines: 10}, &buf); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if !strings.Contains(buf.String(), "helper: starting up") {
		t.Errorf("log stream %q does not contain the process's output", buf.String())
	}
}

// TestHealthyIsFalseSoCustomerPlacementNeverPicksThisNode guards the one
// deliberately surprising value in this package.
func TestHealthyIsFalseSoCustomerPlacementNeverPicksThisNode(t *testing.T) {
	if newTestRuntime(t).Healthy(context.Background()) {
		t.Error("Healthy() = true: this node would look schedulable for customer containers it cannot run")
	}
}

func TestTailOf_ReturnsLastNLines(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.WriteString("one\ntwo\nthree\nfour\n")

	if got := tailOf(f, 2); got != "three\nfour\n" {
		t.Errorf("tailOf(2) = %q, want the last two lines", got)
	}
}

// TestReapStale_OnlySignalsProcessesThatStillMatch is the pid-reuse guard:
// a recorded pid whose live command line no longer contains our recorded
// executable must be left alone.
func TestReapStale_OnlySignalsProcessesThatStillMatch(t *testing.T) {
	dir := t.TempDir()
	orig := processCommand
	t.Cleanup(func() { processCommand = orig })
	processCommand = func(pid int) string {
		if pid == 111 {
			return "/opt/homebrew/bin/mlx_lm.server --model x"
		}
		return "/usr/bin/some-unrelated-thing" // pid reused by something else
	}

	var signalled []int
	origStop := stopPID
	t.Cleanup(func() { stopPID = origStop })
	stopPID = func(pid int) { signalled = append(signalled, pid) }

	rt := &Runtime{cfg: Config{StateDir: dir}, instances: map[string]*instance{}}
	if err := os.WriteFile(rt.pidFile(),
		[]byte(`[{"pid":111,"exe":"/opt/homebrew/bin/mlx_lm.server","instance_id":"a"},`+
			`{"pid":222,"exe":"/opt/homebrew/bin/mlx_lm.server","instance_id":"b"}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	rt.reapStale()

	if len(signalled) != 1 || signalled[0] != 111 {
		t.Errorf("signalled pids = %v, want only 111 (222 was reused by an unrelated process)", signalled)
	}
	if _, err := os.Stat(rt.pidFile()); !os.IsNotExist(err) {
		t.Error("pid registry was not cleared after reaping")
	}
}

// Every instance a previous run recorded is reported as gone-at-startup —
// whether it was reaped or was already dead — so the control plane can drop
// its stale "running" entry.
func TestReapStale_RecordsEveryRecordedInstanceAsGone(t *testing.T) {
	orig := processCommand
	t.Cleanup(func() { processCommand = orig })
	processCommand = func(pid int) string {
		if pid == 111 {
			return "/opt/homebrew/bin/mlx_lm.server --model x"
		}
		return "" // 222 is already dead (e.g. after a reboot)
	}
	origStop := stopPID
	t.Cleanup(func() { stopPID = origStop })
	stopPID = func(int) {}

	rt := &Runtime{cfg: Config{StateDir: t.TempDir()}, instances: map[string]*instance{}}
	if err := os.WriteFile(rt.pidFile(),
		[]byte(`[{"pid":111,"exe":"/opt/homebrew/bin/mlx_lm.server","instance_id":"a"},`+
			`{"pid":222,"exe":"/opt/homebrew/bin/mlx_lm.server","instance_id":"b"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	rt.reapStale()

	got := rt.InstancesGoneAtStartup()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("InstancesGoneAtStartup = %v, want [a b]", got)
	}
}
