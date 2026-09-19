// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package nativeruntime is a cluster.Client that runs a workload as a plain
// host process instead of a Kubernetes pod — the execution backend for
// Teepin Inference on a Mac Mini, where MLX needs direct Metal access and
// therefore cannot run inside the Lima Linux VM the node's ordinary agent
// lives in. teepin-agent selects it with TEEPIN_RUNTIME=native (see
// cmd/teepin-agent); everything else about that agent — enrollment,
// reconnect, heartbeat, status reporting, the tunnel — is reused
// unchanged, which is the whole reason this is a cluster.Client and not a
// separate daemon with its own wire protocol.
//
// It is deliberately NOT a general container runtime. It runs exactly one
// kind of thing: a Command (plus Args/Env) as a supervised child process.
// A spec that names an Image without a Command, or asks for an init
// container, is rejected loudly rather than half-honoured.
package nativeruntime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// PortPlaceholder in a spec's Command/Args/Env values is replaced with the
// host port the runtime allocated for that instance. The runtime, not the
// caller, owns port allocation: two mounts on one Mac must never collide on
// a hard-coded port, and only the host knows what is free right now.
const PortPlaceholder = "${TEEPIN_PORT}"

// hiddenLabel matches pkg/cluster's private labelKumbhaAgent — see the same
// note in pkg/inferencereconciler on why it is redeclared, not imported.
const hiddenLabel = "teepin.io/kumbha-agent"

const (
	statusPending    = "pending"
	statusRunning    = "running"
	statusFailed     = "failed"
	statusTerminated = "terminated"

	readyPollInterval = time.Second
	defaultStopGrace  = 10 * time.Second
	logTailLimitBytes = 256 * 1024
)

// Config configures a Runtime.
type Config struct {
	// StateDir holds per-instance logs and the pid registry, e.g.
	// ~/.teepin/modeld. Created if absent.
	StateDir string
	// NodeName is reported as InstanceStatus.NodeName.
	NodeName string
	// StopGrace is how long a process gets after SIGTERM before SIGKILL.
	// Zero means defaultStopGrace.
	StopGrace time.Duration
}

// Runtime implements cluster.Client over host processes.
type Runtime struct {
	cfg Config

	mu        sync.Mutex
	instances map[string]*instance

	// goneAtStartup lists instances a previous run of this agent had recorded
	// that are no longer running (reaped as orphans, or already dead after a
	// reboot). The control plane still believes they are running; see
	// InstancesGoneAtStartup.
	goneAtStartup []string
}

var _ cluster.Client = (*Runtime)(nil)

type instance struct {
	spec    cluster.InstanceSpec
	cmd     *exec.Cmd
	port    int
	logPath string
	started time.Time

	// Guarded by Runtime.mu.
	state   string
	message string
	exited  chan struct{}
}

// New builds a Runtime and reaps any process a previous run of this agent
// left behind — a 36B model held in unified memory by an orphan is not
// something to leave running just because its supervisor restarted.
func New(cfg Config) (*Runtime, error) {
	if cfg.StateDir == "" {
		return nil, errors.New("nativeruntime: StateDir is required")
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "logs"), 0o700); err != nil {
		return nil, fmt.Errorf("nativeruntime: create state dir: %w", err)
	}
	r := &Runtime{cfg: cfg, instances: make(map[string]*instance)}
	r.reapStale()
	return r, nil
}

// InstancesGoneAtStartup returns the IDs of instances the previous run left
// recorded but that did not survive to this one. The agent reports them
// terminated once after connecting: its own record of what it had reported is
// per-connection, so without this the control plane keeps a stale "running"
// entry forever and never remounts a model an agent update just stopped.
func (r *Runtime) InstancesGoneAtStartup() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.goneAtStartup...)
}

func (r *Runtime) stopGrace() time.Duration {
	if r.cfg.StopGrace > 0 {
		return r.cfg.StopGrace
	}
	return defaultStopGrace
}

// CreateInstance starts the workload and returns as soon as the process is
// launched — NOT when the model is loaded. Loading a large model can take
// minutes (and the agent's create round-trip has a much shorter budget), so
// readiness is reported later through GetInstanceStatus: "pending" until the
// process accepts connections, then "running".
func (r *Runtime) CreateInstance(_ context.Context, spec cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	if len(spec.Command) == 0 {
		return nil, errors.New("nativeruntime: a native workload needs a Command; container images cannot run here")
	}
	if spec.InitContainer != nil {
		return nil, errors.New("nativeruntime: init containers are not supported")
	}
	if spec.InstanceID == "" {
		return nil, errors.New("nativeruntime: InstanceID is required")
	}

	r.mu.Lock()
	if existing, ok := r.instances[spec.InstanceID]; ok {
		if existing.state == statusPending || existing.state == statusRunning {
			result := resultFor(existing)
			r.mu.Unlock()
			return result, nil // redelivered create: already ours, do not start a second copy
		}
		delete(r.instances, spec.InstanceID) // a dead one under the same ID is replaced
	}
	r.mu.Unlock()

	inst, err := r.start(spec)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.instances[spec.InstanceID] = inst
	r.mu.Unlock()
	r.persistPIDs()

	go r.supervise(inst)
	go r.watchReady(inst)

	return resultFor(inst), nil
}

func resultFor(i *instance) *cluster.InstanceResult {
	return &cluster.InstanceResult{PodName: podName(i)}
}

func podName(i *instance) string {
	if i.cmd != nil && i.cmd.Process != nil {
		return "native-" + strconv.Itoa(i.cmd.Process.Pid)
	}
	return "native-unstarted"
}

func (r *Runtime) start(spec cluster.InstanceSpec) (*instance, error) {
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("nativeruntime: allocate port: %w", err)
	}
	portStr := strconv.Itoa(port)
	sub := func(s string) string { return strings.ReplaceAll(s, PortPlaceholder, portStr) }

	argv := make([]string, 0, len(spec.Command)+len(spec.Args))
	for _, a := range spec.Command {
		argv = append(argv, sub(a))
	}
	for _, a := range spec.Args {
		argv = append(argv, sub(a))
	}

	logPath := filepath.Join(r.cfg.StateDir, "logs", spec.InstanceID+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("nativeruntime: open log: %w", err)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(), "TEEPIN_PORT="+portStr)
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+sub(v))
	}
	configureProcess(cmd)

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("nativeruntime: start %q: %w", argv[0], err)
	}

	return &instance{
		spec:    spec,
		cmd:     cmd,
		port:    port,
		logPath: logPath,
		started: time.Now(),
		state:   statusPending,
		exited:  make(chan struct{}),
	}, nil
}

// supervise waits for the process to exit and records why. A process we
// asked to stop is "terminated"; one that died on its own is "failed", with
// the tail of its log as the reason — that tail is what an operator
// actually needs ("out of memory", "model not found"), not an exit code.
func (r *Runtime) supervise(inst *instance) {
	err := inst.cmd.Wait()
	if f, ok := inst.cmd.Stdout.(*os.File); ok {
		_ = f.Close()
	}

	r.mu.Lock()
	if inst.state != statusTerminated {
		inst.state = statusFailed
		reason := "process exited"
		if err != nil {
			reason = err.Error()
		}
		if tail := lastLines(inst.logPath, 3); tail != "" {
			reason += ": " + tail
		}
		inst.message = reason
	}
	close(inst.exited)
	r.mu.Unlock()
	r.persistPIDs()
}

// watchReady flips pending -> running once the process accepts TCP
// connections. A TCP dial rather than an HTTP probe on purpose: the runtime
// must not assume what the workload speaks, and a listening socket is
// exactly the signal that the port is now safe to route to.
func (r *Runtime) watchReady(inst *instance) {
	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(inst.port))
	for {
		select {
		case <-inst.exited:
			return
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
			if err != nil {
				continue
			}
			_ = conn.Close()
			r.mu.Lock()
			if inst.state == statusPending {
				inst.state = statusRunning
			}
			r.mu.Unlock()
			return
		}
	}
}

// UpdateInstance replaces an instance: stop, then start fresh under the
// same ID.
func (r *Runtime) UpdateInstance(ctx context.Context, scope cluster.Scope, spec cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	if err := r.DeleteInstance(ctx, scope, spec.InstanceID); err != nil {
		return nil, err
	}
	return r.CreateInstance(ctx, spec)
}

// DeleteInstance stops the process (SIGTERM, then SIGKILL after the grace
// period) and forgets it. Deleting a missing instance is success — commands
// are redelivered after reconnects.
func (r *Runtime) DeleteInstance(_ context.Context, _ cluster.Scope, instanceID string) error {
	r.mu.Lock()
	inst, ok := r.instances[instanceID]
	if !ok {
		r.mu.Unlock()
		return nil
	}
	inst.state = statusTerminated
	delete(r.instances, instanceID)
	r.mu.Unlock()

	terminate(inst.cmd)
	select {
	case <-inst.exited:
	case <-time.After(r.stopGrace()):
		forceKill(inst.cmd)
		<-inst.exited
	}
	r.persistPIDs()
	return nil
}

func (r *Runtime) statusOf(inst *instance) cluster.InstanceStatus {
	return cluster.InstanceStatus{
		InstanceID: inst.spec.InstanceID,
		Status:     inst.state,
		PodName:    podName(inst),
		NodeName:   r.cfg.NodeName,
		Message:    inst.message,
		ObservedAt: time.Now().UTC(),
		AccountID:  inst.spec.AccountID,
		ProjectID:  inst.spec.ProjectID,
		Hidden:     inst.spec.Labels[hiddenLabel] == "true",
	}
}

func (r *Runtime) GetInstanceStatus(_ context.Context, _ cluster.Scope, instanceID string) (*cluster.InstanceStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[instanceID]
	if !ok {
		return nil, cluster.ErrNotFound
	}
	st := r.statusOf(inst)
	return &st, nil
}

// ListInstanceStatuses mirrors DirectClient's listing semantics: Teepin's
// own hidden workloads are excluded unless the scope asks for them — the
// agent's own status-reporting sweep does, a customer-facing list never.
func (r *Runtime) ListInstanceStatuses(_ context.Context, scope cluster.Scope) ([]cluster.InstanceStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]cluster.InstanceStatus, 0, len(r.instances))
	for _, inst := range r.instances {
		st := r.statusOf(inst)
		if st.Hidden && !scope.IncludeHidden {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

// StreamLogs writes the instance's log to w: the last opts.TailLines lines
// (all of it, capped, when zero), then — with Follow — new output until ctx
// ends or the process has exited and the file is drained.
func (r *Runtime) StreamLogs(ctx context.Context, _ cluster.Scope, instanceID string, opts cluster.LogOptions, w io.Writer) error {
	r.mu.Lock()
	inst, ok := r.instances[instanceID]
	r.mu.Unlock()
	if !ok {
		return cluster.ErrNotFound
	}

	f, err := os.Open(inst.logPath)
	if err != nil {
		return fmt.Errorf("nativeruntime: open log: %w", err)
	}
	defer f.Close()

	if _, err := io.WriteString(w, tailOf(f, opts.TailLines)); err != nil {
		return err
	}
	if !opts.Follow {
		return nil
	}

	reader := bufio.NewReader(f)
	for {
		chunk, _ := reader.ReadString('\n')
		if chunk != "" {
			if _, err := io.WriteString(w, chunk); err != nil {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-inst.exited:
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Inventory: a native host runs no GPU-scheduler workloads — an empty
// success, the same "genuinely nothing here" posture as cluster.CPUOnly.
func (r *Runtime) Inventory(context.Context) ([]cluster.NodeInventory, error) {
	return []cluster.NodeInventory{}, nil
}

// InstanceMetrics: not measured yet. Absent rather than zero-filled, per
// InstanceMetric's own contract.
func (r *Runtime) InstanceMetrics(context.Context) ([]cluster.InstanceMetric, error) {
	return nil, nil
}

// Healthy reports false ON PURPOSE. That value becomes the node's
// k8s_ready, and pkg/nodes.PlaceCPU only places customer containers on
// k8s_ready nodes — a native host cannot run a customer's container image,
// so it must never look schedulable for one. Model mounts are dispatched
// by ProviderID, which does not consult readiness.
func (r *Runtime) Healthy(context.Context) bool { return false }

// ResolveInstanceAddress returns the loopback address the process is
// listening on. The requested port is ignored: a native instance has one
// runtime-allocated port, not a set of container ports to choose between.
func (r *Runtime) ResolveInstanceAddress(_ context.Context, instanceID string, _ int32) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[instanceID]
	if !ok || (inst.state != statusPending && inst.state != statusRunning) {
		return "", cluster.ErrNotFound
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(inst.port)), nil
}

// freePort asks the OS for an unused loopback port. There is an inherent
// race between releasing it here and the child binding it; for a
// single-supervisor host that is acceptable, and a lost race surfaces as a
// "failed" instance the reconciler simply retries.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
