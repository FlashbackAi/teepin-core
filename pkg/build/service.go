// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package build turns an agent session's workspace into a pushed
// container image — the piece the Kumbha plan's M4 exists for, and what
// finally lets the "deploy" MCP verb stop being an honest stub.
//
// Uses Kaniko (github.com/GoogleContainerTools/kaniko, now maintained by
// Chainguard), not Docker-in-Docker or BuildKit: Kaniko needs no
// privileged access and no relaxed seccomp/AppArmor, so a build pod gets
// the exact same restrictive SecurityContext every other Teepin workload
// already runs under (see pkg/cluster/direct.go's buildPod) — no carve-out
// for this one workload type.
//
// This package holds a direct kubernetes.Interface, the same accepted
// exception pkg/harbor already established (main.go gates both on
// k8sClient != nil): registry/build provisioning is control-plane-local
// infrastructure, unlike customer compute, which must also work when the
// control plane runs on AWS with zero Kubernetes credentials at all
// (pkg/cluster's whole reason for existing). Build support in "agent"
// cluster mode is future work, not a regression introduced here.
package build

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/harbor"
)

// Config is the operator-fixed policy every build runs under — not
// customer-selectable, matching the Kumbha agent's own AgentConfig.
type Config struct {
	// KanikoImage is pinned, not floating (":latest" would let an
	// upstream release silently change build behaviour platform-wide).
	KanikoImage string
	// FetchImage runs the initContainer that downloads and unpacks the
	// session's current workspace archive before Kaniko starts — needs
	// only wget and unzip, both BusyBox applets, so a full distro image
	// is unnecessary. Pinned for the same reason KanikoImage is.
	FetchImage string
	CPUUnits   int
	MemoryGB   int
	// Timeout bounds how long a single build pod may run before it is
	// killed and reported as failed.
	Timeout time.Duration
}

// DefaultConfig is used wherever an operator has not overridden a field —
// see NewService.
func DefaultConfig() Config {
	return Config{
		KanikoImage: "gcr.io/kaniko-project/executor:v1.23.2",
		FetchImage:  "busybox:1.36.1",
		CPUUnits:    2,
		MemoryGB:    4,
		Timeout:     15 * time.Minute,
	}
}

// Service runs Kaniko builds against a Kumbha agent's own workspace PVC.
type Service struct {
	k8sClient kubernetes.Interface
	harbor    *harbor.Service
	cfg       Config
}

func NewService(k8sClient kubernetes.Interface, harborService *harbor.Service, cfg Config) *Service {
	if cfg.KanikoImage == "" {
		d := DefaultConfig()
		if cfg.CPUUnits == 0 {
			cfg.CPUUnits = d.CPUUnits
		}
		if cfg.MemoryGB == 0 {
			cfg.MemoryGB = d.MemoryGB
		}
		if cfg.Timeout == 0 {
			cfg.Timeout = d.Timeout
		}
		if cfg.FetchImage == "" {
			cfg.FetchImage = d.FetchImage
		}
		cfg.KanikoImage = d.KanikoImage
	}
	return &Service{k8sClient: k8sClient, harbor: harborService, cfg: cfg}
}

// Result is what a completed build produced.
type Result struct {
	ImageRef string
}

// Request describes one build.
type Request struct {
	ProjectID   uuid.UUID
	ProjectName string
	// WorkspaceArchiveURL and WorkspaceToken locate and authorise a GET of
	// the session's CURRENT workspace version (pkg/kumbha/gateway.go's
	// MintWorkspaceFetchToken) — an initContainer downloads and unpacks
	// this as the build context, rather than mounting the agent pod's own
	// live PVC. This is what makes a customer's IDE edit or a version
	// rollback actually reach the image: both only ever change what
	// pkg/kumbha/workspace.go considers "current", never the agent pod's
	// own on-disk copy, so building from anything else would silently
	// ignore them.
	WorkspaceArchiveURL string
	WorkspaceToken      string
	// DockerfilePath is relative to the workspace root.
	DockerfilePath string
	// Tag becomes the pushed image's tag — typically a short session id,
	// so repeated builds within one session overwrite rather than
	// accumulate.
	Tag string
}

// OnLogLine, when non-nil, is called once per line of the Kaniko build's
// own output as it happens — this is what lets a "building your image"
// observation reach the console's activity feed in real time rather than
// only after the build finishes.
type OnLogLine func(line string)

// Build provisions the project's Harbor registry if needed, runs a Kaniko
// pod against the session's workspace PVC, and returns the pushed image
// reference. The build pod is deleted before returning, success or
// failure — nothing about the build's own execution is meant to be
// customer-inspectable after the fact beyond what OnLogLine already
// streamed live.
func (s *Service) Build(ctx context.Context, req Request, onLogLine OnLogLine) (*Result, error) {
	access, err := s.harbor.ProvisionProjectRegistry(ctx, req.ProjectID, req.ProjectName)
	if err != nil {
		return nil, fmt.Errorf("failed to provision registry: %w", err)
	}

	imageRef := fmt.Sprintf("%s:%s", access.ImagePrefix, req.Tag)
	podName := "kaniko-build-" + req.Tag

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	pod := s.buildPod(podName, req, access.HarborProjectName, imageRef)
	if _, err := s.k8sClient.CoreV1().Pods(cluster.WorkloadNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("failed to create build pod: %w", err)
	}
	defer func() {
		// Best-effort cleanup with a fresh, short-lived context: the
		// caller's ctx may already be the one that just timed out or was
		// cancelled, which would make a cleanup Delete fail too and leak
		// the pod.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = s.k8sClient.CoreV1().Pods(cluster.WorkloadNamespace).Delete(cleanupCtx, podName, metav1.DeleteOptions{})
	}()

	if onLogLine != nil {
		go s.streamLogs(ctx, podName, onLogLine)
	}

	phase, message, err := s.waitForCompletion(ctx, podName)
	if err != nil {
		return nil, err
	}
	if phase != corev1.PodSucceeded {
		return nil, fmt.Errorf("build failed: %s", message)
	}

	return &Result{ImageRef: imageRef}, nil
}

// waitForCompletion polls the build pod's phase. Polling rather than
// watching: a one-off build pod's lifetime is short and this keeps the
// implementation to a plain loop instead of a watch client — acceptable
// for something that runs for minutes, not something latency-sensitive.
func (s *Service) waitForCompletion(ctx context.Context, podName string) (corev1.PodPhase, string, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("build timed out or was cancelled: %w", ctx.Err())
		case <-ticker.C:
			pod, err := s.k8sClient.CoreV1().Pods(cluster.WorkloadNamespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					continue // creation may not have propagated to this read yet
				}
				return "", "", fmt.Errorf("failed to check build status: %w", err)
			}
			switch pod.Status.Phase {
			case corev1.PodSucceeded:
				return corev1.PodSucceeded, "", nil
			case corev1.PodFailed:
				return corev1.PodFailed, podFailureMessage(pod), nil
			}
		}
	}
}

func podFailureMessage(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return fmt.Sprintf("exit code %d: %s", cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}
	}
	return pod.Status.Message
}

// streamLogs follows the build pod's own output and calls onLogLine per
// line — best-effort: a log-streaming failure (pod not ready yet, stream
// hiccup) is not itself a build failure, so errors here are swallowed
// rather than propagated.
func (s *Service) streamLogs(ctx context.Context, podName string, onLogLine OnLogLine) {
	// The pod may not be Running yet when this goroutine starts; retry the
	// log stream a few times rather than giving up on the very first
	// "container not created" error.
	var stream io.ReadCloser
	for attempt := 0; attempt < 30; attempt++ {
		s, err := s.k8sClient.CoreV1().Pods(cluster.WorkloadNamespace).
			GetLogs(podName, &corev1.PodLogOptions{Follow: true}).Stream(ctx)
		if err == nil {
			stream = s
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	if stream == nil {
		return
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		onLogLine(scanner.Text())
	}
}

// buildPod constructs the build pod spec — an initContainer that fetches
// and unpacks the session's current workspace archive into a shared
// emptyDir, then Kaniko building from it. SecurityContext matches
// buildPod's own restrictive posture in pkg/cluster/direct.go exactly
// (drop ALL capabilities, no privilege escalation, RuntimeDefault
// seccomp) on BOTH containers, since a build workload gets no special
// exemption from the isolation model the rest of the platform runs under.
func (s *Service) buildPod(podName string, req Request, harborProjectName, imageRef string) *corev1.Pod {
	cpu := resource.MustParse(fmt.Sprintf("%dm", s.cfg.CPUUnits*1000))
	memory := resource.MustParse(fmt.Sprintf("%dGi", s.cfg.MemoryGB))
	fetchCPU := resource.MustParse("200m")
	fetchMemory := resource.MustParse("256Mi")

	restrictedSecurityContext := &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: cluster.WorkloadNamespace,
			Labels: map[string]string{
				"teepin.io/kumbha-build": "true",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			InitContainers: []corev1.Container{{
				Name:  "fetch-workspace",
				Image: s.cfg.FetchImage,
				// A short-lived, session-scoped token (see
				// MintWorkspaceFetchToken) — referenced via env expansion
				// rather than baked into the command string, the same
				// "credential lives in env, not argv" convention
				// LaunchAgent's own TEEPIN_SESSION_TOKEN already uses.
				Command: []string{"sh", "-c",
					`set -e; wget -q --header="Authorization: Bearer $TEEPIN_TOKEN" -O /tmp/workspace.zip "$TEEPIN_ARCHIVE_URL"; unzip -o -q /tmp/workspace.zip -d /workspace; rm -f /tmp/workspace.zip`,
				},
				Env: []corev1.EnvVar{
					{Name: "TEEPIN_TOKEN", Value: req.WorkspaceToken},
					{Name: "TEEPIN_ARCHIVE_URL", Value: req.WorkspaceArchiveURL},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "workspace", MountPath: "/workspace"},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: fetchCPU, corev1.ResourceMemory: fetchMemory},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: fetchCPU, corev1.ResourceMemory: fetchMemory},
				},
				SecurityContext: restrictedSecurityContext,
			}},
			Containers: []corev1.Container{{
				Name:  "kaniko",
				Image: s.cfg.KanikoImage,
				Args: []string{
					"--dockerfile=" + req.DockerfilePath,
					"--context=dir:///workspace",
					"--destination=" + imageRef,
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "workspace", MountPath: "/workspace", ReadOnly: true},
					{Name: "docker-config", MountPath: "/kaniko/.docker"},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory},
				},
				SecurityContext: restrictedSecurityContext,
			}},
			Volumes: []corev1.Volume{
				{
					// Populated by the fetch-workspace initContainer above,
					// not a PVC — see Request.WorkspaceArchiveURL's own doc
					// comment on why this replaced mounting the agent's PVC.
					Name:         "workspace",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
				{
					Name: "docker-config",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: harbor.RegistrySecretName(harborProjectName),
							Items: []corev1.KeyToPath{
								{Key: ".dockerconfigjson", Path: "config.json"},
							},
						},
					},
				},
			},
		},
	}
}

func boolPtr(b bool) *bool { return &b }
