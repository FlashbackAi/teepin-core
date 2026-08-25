// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package build

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/harbor"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	return NewService(fake.NewSimpleClientset(), nil, DefaultConfig())
}

func TestNewService_FillsDefaultsWhenConfigIsZeroValue(t *testing.T) {
	s := NewService(fake.NewSimpleClientset(), nil, Config{})
	if s.cfg.KanikoImage == "" {
		t.Error("KanikoImage was not defaulted")
	}
	if s.cfg.CPUUnits == 0 || s.cfg.MemoryGB == 0 || s.cfg.Timeout == 0 {
		t.Errorf("zero-value Config was not fully defaulted: %+v", s.cfg)
	}
}

func TestNewService_PreservesExplicitConfig(t *testing.T) {
	s := NewService(fake.NewSimpleClientset(), nil, Config{
		KanikoImage: "custom/kaniko:v9",
		CPUUnits:    4,
		MemoryGB:    8,
		Timeout:     time.Minute,
	})
	if s.cfg.KanikoImage != "custom/kaniko:v9" || s.cfg.CPUUnits != 4 || s.cfg.MemoryGB != 8 || s.cfg.Timeout != time.Minute {
		t.Errorf("explicit config was overwritten by defaults: %+v", s.cfg)
	}
}

func TestBuildPod_FetchesWorkspaceViaInitContainerIntoEmptyDir(t *testing.T) {
	s := newTestService(t)
	req := Request{
		WorkspaceArchiveURL: "http://teepin-api.default.svc.cluster.local:8080/v1/kumbha/sessions/sess1/workspace/archive",
		WorkspaceToken:      "test-fetch-token",
		DockerfilePath:      "Dockerfile",
		Tag:                 "sess1",
	}

	pod := s.buildPod("kaniko-build-sess1", req, "teepin-myapp-a1b2c3d4", "registry.teepin.cloud/teepin-myapp-a1b2c3d4:sess1")

	var workspaceVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "workspace" {
			workspaceVol = &pod.Spec.Volumes[i]
		}
	}
	if workspaceVol == nil || workspaceVol.EmptyDir == nil {
		t.Fatal("workspace volume is missing or not an EmptyDir source — nothing PVC-based should remain")
	}

	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("got %d init containers, want exactly 1 (fetch-workspace)", len(pod.Spec.InitContainers))
	}
	fetch := pod.Spec.InitContainers[0]
	if fetch.Name != "fetch-workspace" {
		t.Errorf("init container name = %q, want fetch-workspace", fetch.Name)
	}
	var gotURL, gotToken string
	for _, e := range fetch.Env {
		switch e.Name {
		case "TEEPIN_ARCHIVE_URL":
			gotURL = e.Value
		case "TEEPIN_TOKEN":
			gotToken = e.Value
		}
	}
	if gotURL != req.WorkspaceArchiveURL {
		t.Errorf("TEEPIN_ARCHIVE_URL = %q, want %q", gotURL, req.WorkspaceArchiveURL)
	}
	if gotToken != req.WorkspaceToken {
		t.Errorf("TEEPIN_TOKEN = %q, want %q", gotToken, req.WorkspaceToken)
	}
	// The token must never appear directly in the command string — it
	// belongs in env only (referenced via shell expansion), matching
	// LaunchAgent's own "credential lives in env, not argv" convention.
	for _, arg := range fetch.Command {
		if strings.Contains(arg, req.WorkspaceToken) {
			t.Errorf("fetch token leaked directly into the container command: %q", arg)
		}
	}

	var fetchMount *corev1.VolumeMount
	for i := range fetch.VolumeMounts {
		if fetch.VolumeMounts[i].Name == "workspace" {
			fetchMount = &fetch.VolumeMounts[i]
		}
	}
	if fetchMount == nil {
		t.Fatal("fetch-workspace does not mount the workspace volume")
	}
	if fetchMount.ReadOnly {
		t.Error("fetch-workspace must be able to WRITE into the workspace volume to unpack the archive")
	}

	var kanikoMount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].Name == "workspace" {
			kanikoMount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if kanikoMount == nil || !kanikoMount.ReadOnly {
		t.Error("kaniko must mount the workspace volume read-only — a build must never modify its own fetched source")
	}
}

func TestBuildPod_MountsTheProjectsRegistrySecretForPush(t *testing.T) {
	s := newTestService(t)
	req := Request{DockerfilePath: "Dockerfile", Tag: "sess1"}

	pod := s.buildPod("kaniko-build-sess1", req, "teepin-myapp-a1b2c3d4", "ref:sess1")

	var secretVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "docker-config" {
			secretVol = &pod.Spec.Volumes[i]
		}
	}
	if secretVol == nil || secretVol.Secret == nil {
		t.Fatal("docker-config volume is missing or not a Secret source")
	}
	wantSecret := harbor.RegistrySecretName("teepin-myapp-a1b2c3d4")
	if secretVol.Secret.SecretName != wantSecret {
		t.Errorf("secret name = %q, want %q (must match what harbor.ProvisionProjectRegistry created)",
			secretVol.Secret.SecretName, wantSecret)
	}
}

func TestBuildPod_SecurityContextMatchesPlatformIsolationPosture(t *testing.T) {
	s := newTestService(t)
	pod := s.buildPod("kaniko-build-x", Request{DockerfilePath: "Dockerfile", Tag: "x"}, "proj", "ref")

	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.SeccompProfile == nil ||
		pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("pod SecurityContext must set RuntimeDefault seccomp — same posture as every other Teepin workload")
	}
	assertRestricted := func(name string, sc *corev1.SecurityContext) {
		t.Helper()
		if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s must set AllowPrivilegeEscalation: false", name)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s must drop ALL capabilities, got %+v", name, sc.Capabilities)
		}
	}
	assertRestricted("kaniko container", pod.Spec.Containers[0].SecurityContext)
	assertRestricted("fetch-workspace init container", pod.Spec.InitContainers[0].SecurityContext)
}

func TestBuildPod_PassesDockerfileContextAndDestinationToKaniko(t *testing.T) {
	s := newTestService(t)
	pod := s.buildPod("kaniko-build-x", Request{DockerfilePath: "backend/Dockerfile", Tag: "x"}, "proj", "registry.teepin.cloud/proj:x")

	args := pod.Spec.Containers[0].Args
	want := []string{
		"--dockerfile=backend/Dockerfile",
		"--context=dir:///workspace",
		"--destination=registry.teepin.cloud/proj:x",
	}
	if len(args) != len(want) {
		t.Fatalf("got %d args, want %d: %v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestWaitForCompletion_DetectsSucceeded(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "kaniko-build-x", Namespace: cluster.WorkloadNamespace},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	})
	s := NewService(client, nil, DefaultConfig())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	phase, _, err := s.waitForCompletion(ctx, "kaniko-build-x")
	if err != nil {
		t.Fatalf("waitForCompletion: %v", err)
	}
	if phase != corev1.PodSucceeded {
		t.Errorf("phase = %v, want Succeeded", phase)
	}
}

func TestWaitForCompletion_DetectsFailedWithMessage(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "kaniko-build-x", Namespace: cluster.WorkloadNamespace},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1, Reason: "Error",
				}},
			}},
		},
	})
	s := NewService(client, nil, DefaultConfig())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	phase, message, err := s.waitForCompletion(ctx, "kaniko-build-x")
	if err != nil {
		t.Fatalf("waitForCompletion: %v", err)
	}
	if phase != corev1.PodFailed {
		t.Errorf("phase = %v, want Failed", phase)
	}
	if message == "" {
		t.Error("failure message was empty — the caller has nothing to tell the customer")
	}
}

func TestWaitForCompletion_RespectsContextCancellation(t *testing.T) {
	// A pod stuck Pending forever (e.g. no capacity) must not hang the
	// caller past its own deadline.
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "kaniko-build-x", Namespace: cluster.WorkloadNamespace},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	})
	s := NewService(client, nil, DefaultConfig())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_, _, _ = s.waitForCompletion(ctx, "kaniko-build-x")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForCompletion did not return after its context was cancelled")
	}
}

func TestPodFailureMessage_FallsBackToPodMessageWhenNoContainerStatus(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{Message: "scheduling failed"}}
	if got := podFailureMessage(pod); got != "scheduling failed" {
		t.Errorf("got %q, want the pod-level message", got)
	}
}
