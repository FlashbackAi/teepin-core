// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package build

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// fakeCluster is a minimal cluster.Client — records what was created/
// deleted and returns a scripted sequence of statuses, nothing more.
// pkg/kumbha has its own equivalent fake; not shared across packages
// since each is small enough that duplicating it costs less than adding
// a cross-package test-only dependency.
type fakeCluster struct {
	createErr error
	created   []cluster.InstanceSpec
	deleted   []string

	// statusSequence is returned one entry per GetInstanceStatus call,
	// advancing each time (sticking on the last entry once exhausted) —
	// lets a test simulate "pending, then pending, then terminated".
	statusSequence []cluster.InstanceStatus
	statusIdx      int
	statusErr      error

	logs string
}

func (f *fakeCluster) CreateInstance(_ context.Context, spec cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, spec)
	return &cluster.InstanceResult{PodName: spec.InstanceID}, nil
}

func (f *fakeCluster) UpdateInstance(_ context.Context, _ cluster.Scope, spec cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, spec)
	return &cluster.InstanceResult{PodName: spec.InstanceID}, nil
}

func (f *fakeCluster) DeleteInstance(_ context.Context, _ cluster.Scope, instanceID string) error {
	f.deleted = append(f.deleted, instanceID)
	return nil
}

func (f *fakeCluster) GetInstanceStatus(_ context.Context, _ cluster.Scope, _ string) (*cluster.InstanceStatus, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if len(f.statusSequence) == 0 {
		return &cluster.InstanceStatus{Status: "pending"}, nil
	}
	idx := f.statusIdx
	if idx >= len(f.statusSequence) {
		idx = len(f.statusSequence) - 1
	}
	f.statusIdx++
	st := f.statusSequence[idx]
	return &st, nil
}

func (f *fakeCluster) ListInstanceStatuses(context.Context, cluster.Scope) ([]cluster.InstanceStatus, error) {
	return nil, nil
}

func (f *fakeCluster) StreamLogs(_ context.Context, _ cluster.Scope, _ string, _ cluster.LogOptions, w io.Writer) error {
	if f.logs != "" {
		_, _ = io.WriteString(w, f.logs)
	}
	return nil
}

func (f *fakeCluster) Inventory(context.Context) ([]cluster.NodeInventory, error) { return nil, nil }
func (f *fakeCluster) InstanceMetrics(context.Context) ([]cluster.InstanceMetric, error) {
	return nil, nil
}
func (f *fakeCluster) Healthy(context.Context) bool                               { return true }
func (f *fakeCluster) ResolveInstanceAddress(context.Context, string, int32) (string, error) {
	return "", cluster.ErrNotFound
}

// newTestService returns a Service with a nil harbor.Service — fine for
// every test here, since none of them call Build() (which needs a real
// registry to provision); they exercise buildInstanceSpec/
// waitForCompletion directly, the same boundary the pre-rewrite tests
// already drew (Build()'s own orchestration was never unit-tested either
// — it needs a working Harbor, which is integration-test territory).
func newTestService(t *testing.T) *Service {
	t.Helper()
	return NewService(&fakeCluster{}, nil, DefaultConfig())
}

func TestNewService_FillsDefaultsWhenConfigIsZeroValue(t *testing.T) {
	s := NewService(&fakeCluster{}, nil, Config{})
	if s.cfg.KanikoImage == "" {
		t.Error("KanikoImage was not defaulted")
	}
	if s.cfg.CPUUnits == 0 || s.cfg.MemoryGB == 0 || s.cfg.EphemeralStorageGB == 0 || s.cfg.Timeout == 0 {
		t.Errorf("zero-value Config was not fully defaulted: %+v", s.cfg)
	}
}

func TestNewService_PreservesExplicitConfig(t *testing.T) {
	s := NewService(&fakeCluster{}, nil, Config{
		KanikoImage:        "custom/kaniko:v9-debug",
		CPUUnits:           4,
		MemoryGB:           8,
		EphemeralStorageGB: 30,
		Timeout:            time.Minute,
	})
	if s.cfg.KanikoImage != "custom/kaniko:v9-debug" || s.cfg.CPUUnits != 4 || s.cfg.MemoryGB != 8 ||
		s.cfg.EphemeralStorageGB != 30 || s.cfg.Timeout != time.Minute {
		t.Errorf("explicit config was overwritten by defaults: %+v", s.cfg)
	}
}

func testRequest() Request {
	return Request{
		ProjectID:           uuid.New(),
		WorkspaceArchiveURL: "http://teepin-api.default.svc.cluster.local:8080/v1/kumbha/sessions/sess1/workspace/archive",
		WorkspaceToken:      "test-fetch-token",
		DockerfilePath:      "Dockerfile",
		Tag:                 "sess1",
	}
}

func TestBuildInstanceSpec_UsesDebugKanikoImageAndBusyboxShell(t *testing.T) {
	s := newTestService(t)
	spec := s.buildInstanceSpec("kaniko-build-sess1", testRequest(), `{"auths":{}}`, "registry.teepin.cloud/proj:sess1")

	if spec.Image != s.cfg.KanikoImage {
		t.Errorf("Image = %q, want the configured KanikoImage %q", spec.Image, s.cfg.KanikoImage)
	}
	if !strings.HasSuffix(s.cfg.KanikoImage, "-debug") {
		t.Errorf("KanikoImage = %q, must be a -debug variant — the plain executor image has no shell", s.cfg.KanikoImage)
	}
	if len(spec.Command) == 0 || spec.Command[0] != "/busybox/sh" {
		t.Errorf("Command = %v, want it to invoke the debug image's own busybox shell", spec.Command)
	}
	if spec.NeverRestart != true {
		t.Error("NeverRestart must be true — a restarted build would silently re-run from scratch")
	}
}

// TestBuildInstanceSpec_HiddenFromCustomerComputeList is the regression
// test for a real 2026-08-29 incident: a Kaniko build pod carried no
// label distinguishing it from a customer-created instance, so it showed
// up in the customer's own Compute list (ListInstances is cluster-
// authoritative — see pkg/cluster's managedSelector, which excludes
// anything carrying this exact label). Teepin's own build tooling should
// never be something a customer sees or is tempted to click Delete on.
func TestBuildInstanceSpec_HiddenFromCustomerComputeList(t *testing.T) {
	s := newTestService(t)
	spec := s.buildInstanceSpec("kaniko-build-sess1", testRequest(), `{"auths":{}}`, "registry.teepin.cloud/proj:sess1")

	if spec.Labels[hiddenFromComputeListLabel] != "true" {
		t.Errorf("Labels[%q] = %q, want \"true\" — a Kaniko build pod must be excluded from the customer's Compute list",
			hiddenFromComputeListLabel, spec.Labels[hiddenFromComputeListLabel])
	}
}

func TestBuildInstanceSpec_ScriptFetchesWorkspaceWritesDockerConfigThenExecsKaniko(t *testing.T) {
	s := newTestService(t)
	req := testRequest()
	spec := s.buildInstanceSpec("kaniko-build-sess1", req, `{"auths":{"x":"y"}}`, "registry.teepin.cloud/proj:sess1")

	if len(spec.Args) != 1 {
		t.Fatalf("got %d Args, want exactly 1 (the script)", len(spec.Args))
	}
	script := spec.Args[0]
	for _, want := range []string{
		"wget", "$TEEPIN_ARCHIVE_URL", "$TEEPIN_TOKEN",
		"unzip", "/workspace",
		"/kaniko/.docker/config.json", "$TEEPIN_DOCKER_CONFIG",
		"/kaniko/executor", "$TEEPIN_DOCKERFILE_PATH", "$TEEPIN_DESTINATION",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script does not contain %q:\n%s", want, script)
		}
	}
}

// TestBuildInstanceSpec_CreatesTmpBeforeFetchingWorkspace covers a real
// 2026-08-26 incident: every single build was failing, unconditionally,
// on the very first real command — the debug Kaniko image cannot be
// assumed to already have a /tmp directory, so `wget -O /tmp/...`
// failed with "can't open ... No such file or directory" before the
// customer's own Dockerfile was ever reached. Checks ORDER, not just
// presence: a `mkdir -p /tmp` anywhere in the script isn't enough if it
// comes after the wget line that needs it.
func TestBuildInstanceSpec_CreatesTmpBeforeFetchingWorkspace(t *testing.T) {
	s := newTestService(t)
	spec := s.buildInstanceSpec("kaniko-build-sess1", testRequest(), `{"auths":{}}`, "registry.teepin.cloud/proj:sess1")
	script := spec.Args[0]

	mkdirIdx := strings.Index(script, "mkdir -p /tmp")
	wgetIdx := strings.Index(script, "wget")
	if mkdirIdx == -1 {
		t.Fatal("script does not create /tmp at all")
	}
	if wgetIdx == -1 {
		t.Fatal("script does not fetch the workspace at all")
	}
	if mkdirIdx > wgetIdx {
		t.Errorf("mkdir -p /tmp (at %d) must come BEFORE the wget fetch (at %d), not after:\n%s", mkdirIdx, wgetIdx, script)
	}
}

func TestBuildInstanceSpec_SecretsReachTheContainerOnlyThroughEnvNeverArgv(t *testing.T) {
	s := newTestService(t)
	req := testRequest()
	req.WorkspaceToken = "super-secret-fetch-token"
	dockerConfig := `{"auths":{"registry":"super-secret-registry-password"}}`
	spec := s.buildInstanceSpec("kaniko-build-sess1", req, dockerConfig, "registry.teepin.cloud/proj:sess1")

	// The script text is static (only env-var REFERENCES like
	// "$TEEPIN_TOKEN", never the literal secret value) — Command/Args
	// must never contain the actual secret strings themselves.
	for _, arg := range append(append([]string{}, spec.Command...), spec.Args...) {
		if strings.Contains(arg, req.WorkspaceToken) {
			t.Errorf("fetch token leaked directly into Command/Args: %q", arg)
		}
		if strings.Contains(arg, dockerConfig) {
			t.Errorf("docker config leaked directly into Command/Args: %q", arg)
		}
	}
	if spec.Env["TEEPIN_TOKEN"] != req.WorkspaceToken {
		t.Errorf("TEEPIN_TOKEN = %q, want %q", spec.Env["TEEPIN_TOKEN"], req.WorkspaceToken)
	}
	if spec.Env["TEEPIN_DOCKER_CONFIG"] != dockerConfig {
		t.Errorf("TEEPIN_DOCKER_CONFIG = %q, want %q", spec.Env["TEEPIN_DOCKER_CONFIG"], dockerConfig)
	}
}

func TestBuildInstanceSpec_DockerfilePathAndDestinationPassedViaEnv(t *testing.T) {
	s := newTestService(t)
	req := testRequest()
	req.DockerfilePath = "backend/Dockerfile"
	spec := s.buildInstanceSpec("kaniko-build-x", req, `{}`, "registry.teepin.cloud/proj:x")

	if spec.Env["TEEPIN_DOCKERFILE_PATH"] != "backend/Dockerfile" {
		t.Errorf("TEEPIN_DOCKERFILE_PATH = %q, want backend/Dockerfile", spec.Env["TEEPIN_DOCKERFILE_PATH"])
	}
	if spec.Env["TEEPIN_DESTINATION"] != "registry.teepin.cloud/proj:x" {
		t.Errorf("TEEPIN_DESTINATION = %q, want registry.teepin.cloud/proj:x", spec.Env["TEEPIN_DESTINATION"])
	}
}

func TestWaitForCompletion_DetectsTerminatedAsSuccess(t *testing.T) {
	fc := &fakeCluster{statusSequence: []cluster.InstanceStatus{{Status: "terminated"}}}
	s := NewService(fc, nil, DefaultConfig())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := s.waitForCompletion(ctx, cluster.AllTenants(), "kaniko-build-x")
	if err != nil {
		t.Fatalf("waitForCompletion: %v", err)
	}
	if status.Status != "terminated" {
		t.Errorf("status = %q, want terminated", status.Status)
	}
}

func TestWaitForCompletion_DetectsFailedWithMessage(t *testing.T) {
	fc := &fakeCluster{statusSequence: []cluster.InstanceStatus{{Status: "failed", Message: "exit code 1"}}}
	s := NewService(fc, nil, DefaultConfig())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := s.waitForCompletion(ctx, cluster.AllTenants(), "kaniko-build-x")
	if err != nil {
		t.Fatalf("waitForCompletion: %v", err)
	}
	if status.Status != "failed" || status.Message == "" {
		t.Errorf("got %+v, want failed with a non-empty message", status)
	}
}

func TestWaitForCompletion_RespectsContextCancellation(t *testing.T) {
	// A build stuck "pending" forever (e.g. no capacity) must not hang the
	// caller past its own deadline.
	fc := &fakeCluster{statusSequence: []cluster.InstanceStatus{{Status: "pending"}}}
	s := NewService(fc, nil, DefaultConfig())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_, _ = s.waitForCompletion(ctx, cluster.AllTenants(), "kaniko-build-x")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForCompletion did not return after its context was cancelled")
	}
}

// TestCaptureFailureLog_ReplaysLogsThroughOnLogLine covers the fix for a
// real 2026-08-26 incident: a customer's failed build always came back
// with an empty log, because Build() relied ENTIRELY on a live-follow
// goroutine that raced the very failure it was meant to capture (the
// moment waitForCompletion sees "failed", Build()'s own deferred cleanup
// deletes the instance and cancels ctx). captureFailureLog is the
// synchronous, non-following fallback called on every failure path —
// this confirms it independently delivers log content through
// onLogLine, without depending on the live-goroutine's timing at all.
func TestCaptureFailureLog_ReplaysLogsThroughOnLogLine(t *testing.T) {
	fc := &fakeCluster{logs: "fetching workspace...\nwget: unable to resolve host\n"}
	s := NewService(fc, nil, DefaultConfig())

	var lines []string
	s.captureFailureLog(cluster.AllTenants(), "kaniko-build-x", func(line string) {
		lines = append(lines, line)
	})

	want := []string{"fetching workspace...", "wget: unable to resolve host"}
	if len(lines) != len(want) || lines[0] != want[0] || lines[1] != want[1] {
		t.Errorf("got %v, want %v", lines, want)
	}
}

func TestBuildInstanceID_IsDeterministicFromTag(t *testing.T) {
	if got, want := buildInstanceID("sess1"), "kaniko-build-sess1"; got != want {
		t.Errorf("buildInstanceID(%q) = %q, want %q", "sess1", got, want)
	}
}
