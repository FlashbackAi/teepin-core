// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package build turns a Kumbha session's current workspace version into a
// pushed container image — the piece that lets the `deploy` MCP verb and
// the console IDE's Deploy button actually deploy something.
//
// Uses Kaniko (github.com/GoogleContainerTools/kaniko, now maintained by
// Chainguard), not Docker-in-Docker or BuildKit: Kaniko needs no
// privileged access, so a build pod gets the exact same restrictive
// SecurityContext every other Teepin workload already runs under.
//
// Expressed as a SINGLE cluster.InstanceSpec-based instance — the same
// abstraction LaunchAgent, CreateInstance, and DeleteInstance already use
// — rather than a hand-built Kubernetes Pod via a direct client. This is
// deliberate, not incidental: InstanceSpec has no concept of a second
// (init) container or a mounted Secret volume (confirmed by reading
// pkg/cluster's own client.go/direct.go/agent.go and the agentpb proto —
// CreateInstanceCommand is a 1:1 wire mirror of InstanceSpec, nothing
// more), so this package used to require its OWN direct kubernetes.Interface
// specifically to express a two-container (fetch + kaniko) pod with a
// Secret mount — which meant it only ever worked when the control plane
// itself had direct Kubernetes credentials (TEEPIN_CLUSTER_MODE=direct),
// never in the ECS/agent-mode topology this product actually runs in
// production. Collapsing to one container, with BOTH the workspace fetch
// and the registry push credential delivered via Env instead of an
// initContainer/Secret (see buildPod's own comment), means this package
// now goes through cluster.Client like everything else — same code path,
// same behaviour, in either cluster mode.
package build

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// RegistryProvider is whatever pkg/build pushes Kaniko's output to —
// Harbor (*harbor.Service) or ECR (*ecrregistry.Service), abstracted so
// this package does not care which is configured on a given deployment.
// Both concrete types satisfy this structurally (Go interfaces, no import
// needed in either direction) — see each's own DockerConfigJSONForBuild
// doc comment for why the credential travels as a docker-config string
// rather than a mounted Secret.
type RegistryProvider interface {
	// ImagePrefix returns (provisioning the underlying repository/project
	// if needed) the pushable image prefix for a project, e.g.
	// "123456789012.dkr.ecr.us-east-1.amazonaws.com/teepin/kumbha-builds-dev"
	// or "registry.teepin.cloud/teepin-myapp-abc123".
	ImagePrefix(ctx context.Context, projectID uuid.UUID, projectName string) (string, error)
	// DockerConfigJSONForBuild returns a marshaled .dockerconfigjson
	// granting push access to that same prefix.
	DockerConfigJSONForBuild(ctx context.Context, projectID uuid.UUID) (string, error)
	// ImageAuth returns the same credential DockerConfigJSONForBuild
	// wraps into a .dockerconfigjson, as a plain (username, password)
	// pair — for a caller that wants to authenticate a registry client
	// directly (see DeployKumbhaSession's use of pkg/imageinfo to
	// resolve a just-pushed image's own declared ports) rather than
	// write a Kaniko config file.
	ImageAuth(ctx context.Context, projectID uuid.UUID) (username, password string, err error)
}

// Config is the operator-fixed policy every build runs under — not
// customer-selectable, matching the Kumbha agent's own AgentConfig.
type Config struct {
	// KanikoImage must be a "debug" (BusyBox-shell) variant — the plain
	// distroless executor image has no shell, no wget, nothing but the
	// kaniko binary itself, and this package needs a shell to fetch the
	// workspace archive and write the registry credential before
	// exec'ing kaniko in the SAME container. Pinned, not floating (an
	// unpinned ":debug" would let an upstream release silently change
	// build behaviour platform-wide).
	KanikoImage string
	CPUUnits    int
	MemoryGB    int
	// EphemeralStorageGB caps the build container's own writable-layer
	// disk — there is no separate scratch volume (InstanceSpec has none
	// to offer beyond the billed, StorageGB-backed /data PVC, which this
	// package deliberately does not use for a few-minutes-lived build).
	// Kaniko needs room to unpack the base image's layers plus write the
	// new ones; generous by design since a generated web app's own
	// source is tiny compared to a base image.
	EphemeralStorageGB int
	// Timeout bounds how long a single build may run before it is
	// killed and reported as failed.
	Timeout time.Duration
}

// DefaultConfig is used wherever an operator has not overridden a field —
// see NewService.
func DefaultConfig() Config {
	return Config{
		KanikoImage:        "gcr.io/kaniko-project/executor:v1.23.2-debug",
		CPUUnits:           2,
		MemoryGB:           4,
		EphemeralStorageGB: 15,
		Timeout:            15 * time.Minute,
	}
}

// Service runs Kaniko builds of a Kumbha session's current workspace
// version, through cluster.Client — the same transport-neutral interface
// LaunchAgent/CreateInstance/DeleteInstance already use, so this package
// works identically whether the control plane is in direct or agent
// cluster mode.
type Service struct {
	cluster  cluster.Client
	registry RegistryProvider
	cfg      Config
}

func NewService(clusterClient cluster.Client, registry RegistryProvider, cfg Config) *Service {
	if cfg.KanikoImage == "" {
		d := DefaultConfig()
		if cfg.CPUUnits == 0 {
			cfg.CPUUnits = d.CPUUnits
		}
		if cfg.MemoryGB == 0 {
			cfg.MemoryGB = d.MemoryGB
		}
		if cfg.EphemeralStorageGB == 0 {
			cfg.EphemeralStorageGB = d.EphemeralStorageGB
		}
		if cfg.Timeout == 0 {
			cfg.Timeout = d.Timeout
		}
		cfg.KanikoImage = d.KanikoImage
	}
	return &Service{cluster: clusterClient, registry: registry, cfg: cfg}
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
	// MintWorkspaceFetchToken) — fetched and unpacked by the build
	// container's own entrypoint script before it invokes kaniko, so a
	// customer's IDE edit or a version rollback (both of which only ever
	// change what pkg/kumbha/workspace.go considers "current", never
	// anything a Kubernetes volume could already hold) is what actually
	// gets built.
	WorkspaceArchiveURL string
	WorkspaceToken      string
	// DockerfilePath is relative to the workspace root.
	DockerfilePath string
	// Tag becomes the pushed image's tag — typically a short session id,
	// so repeated builds within one session overwrite rather than
	// accumulate.
	Tag string
}

// OnLogLine, when non-nil, is called once per line of the build's own
// output as it happens — this is what lets a "building your image"
// observation reach the console's activity feed in real time rather than
// only after the build finishes.
type OnLogLine func(line string)

// buildInstanceID names the build instance, deterministic from the
// requested tag so a retried/duplicate build call for the same tag
// collides (fails fast on a still-running duplicate) rather than
// silently launching two builds pushing the same destination
// concurrently.
func buildInstanceID(tag string) string {
	return "kaniko-build-" + tag
}

// ImageAuth exposes the registry credential a just-built image was
// pushed with — see RegistryProvider.ImageAuth's own doc comment. A
// thin delegation so a caller holding only this exported *Service (e.g.
// pkg/api.DeployKumbhaSession) does not need its own reference to the
// unexported registry field.
func (s *Service) ImageAuth(ctx context.Context, projectID uuid.UUID) (username, password string, err error) {
	return s.registry.ImageAuth(ctx, projectID)
}

// Build provisions the project's registry, launches one build instance
// through cluster.Client, waits for it to finish, and returns the pushed
// image reference. The instance is deleted before returning, success or
// failure — nothing about the build's own execution is meant to be
// customer-inspectable after the fact beyond what OnLogLine already
// streamed live.
func (s *Service) Build(ctx context.Context, req Request, onLogLine OnLogLine) (*Result, error) {
	imagePrefix, err := s.registry.ImagePrefix(ctx, req.ProjectID, req.ProjectName)
	if err != nil {
		return nil, fmt.Errorf("failed to provision registry: %w", err)
	}
	dockerConfigJSON, err := s.registry.DockerConfigJSONForBuild(ctx, req.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("failed to load registry credentials: %w", err)
	}

	imageRef := fmt.Sprintf("%s:%s", imagePrefix, req.Tag)
	instanceID := buildInstanceID(req.Tag)
	scope := cluster.ProjectScope(req.ProjectID.String())

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	spec := s.buildInstanceSpec(instanceID, req, dockerConfigJSON, imageRef)
	if _, err := s.cluster.CreateInstance(ctx, spec); err != nil {
		return nil, fmt.Errorf("failed to create build instance: %w", err)
	}
	defer func() {
		// Best-effort cleanup with a fresh, short-lived context: the
		// caller's ctx may already be the one that just timed out or was
		// cancelled, which would make a cleanup delete fail too and leak
		// the instance.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = s.cluster.DeleteInstance(cleanupCtx, scope, instanceID)
	}()

	if onLogLine != nil {
		go s.streamLogs(ctx, scope, instanceID, onLogLine)
	}

	status, waitErr := s.waitForCompletion(ctx, scope, instanceID)
	if waitErr != nil {
		// Timed out or was cancelled — still worth a best-effort capture
		// of whatever the instance had printed before giving up, same
		// reasoning as the failure path below.
		if onLogLine != nil {
			s.captureFailureLog(scope, instanceID, onLogLine)
		}
		return nil, waitErr
	}
	if status.Status != "terminated" {
		msg := status.Message
		if msg == "" {
			msg = status.Status
		}
		// Best-effort, synchronous, one-shot fetch of whatever the failed
		// container actually printed — NOT reliant on the live-follow
		// goroutine started above, which races the very failure it is
		// trying to capture: the moment waitForCompletion reports
		// "failed", this function's own deferred cleanup deletes the
		// instance and cancels ctx, so a fast failure (a build script
		// erroring within the first couple of seconds — the common case)
		// could easily leave the live stream having captured nothing at
		// all. Found live 2026-08-26: a real customer-facing build
		// failure came back with "log":"" every single time. Errors from
		// this fetch are swallowed — it is a diagnostic nicety, not
		// itself something that should mask the real failure below.
		if onLogLine != nil {
			s.captureFailureLog(scope, instanceID, onLogLine)
		}
		return nil, fmt.Errorf("build failed: %s", msg)
	}

	return &Result{ImageRef: imageRef}, nil
}

// captureFailureLog does a synchronous, non-following fetch of a build
// instance's log output and replays it line by line through onLogLine —
// see Build's own doc comment on why the live-follow goroutine cannot be
// relied on for this. Uses a fresh, short-lived context, not the
// caller's (which may already be cancelled or near its own deadline),
// and never returns an error: this is best-effort diagnostics, and a
// failure to fetch it must not mask the real build failure.
func (s *Service) captureFailureLog(scope cluster.Scope, instanceID string, onLogLine OnLogLine) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			onLogLine(scanner.Text())
		}
	}()

	_ = s.cluster.StreamLogs(ctx, scope, instanceID, cluster.LogOptions{Follow: false}, pw)
	_ = pw.Close()
	<-done
}

// waitForCompletion polls the build instance's status. Polling rather
// than watching: a one-off build's lifetime is short and this keeps the
// implementation to a plain loop instead of a watch client — acceptable
// for something that runs for minutes, not something latency-sensitive.
// "terminated" is cluster.Client's status string for a successful exit
// (PodSucceeded in direct mode — see cluster.podStatus); "failed" covers
// both a nonzero exit AND unrecoverable pull/crash states the underlying
// client already detects (ImagePullBackOff, CrashLoopBackOff), which is a
// strict improvement over this package's own previous phase-only check.
func (s *Service) waitForCompletion(ctx context.Context, scope cluster.Scope, instanceID string) (*cluster.InstanceStatus, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("build timed out or was cancelled: %w", ctx.Err())
		case <-ticker.C:
			status, err := s.cluster.GetInstanceStatus(ctx, scope, instanceID)
			if err != nil {
				if err == cluster.ErrNotFound {
					continue // creation may not have propagated to this read yet
				}
				return nil, fmt.Errorf("failed to check build status: %w", err)
			}
			if status.Status == "terminated" || status.Status == "failed" {
				return status, nil
			}
		}
	}
}

// streamLogs follows the build instance's own output and calls
// onLogLine per line — best-effort: a log-streaming failure (instance
// not ready yet, stream hiccup) is not itself a build failure, so errors
// here are swallowed rather than propagated. Retries a few times before
// giving up, since the instance may not be schedulable/running yet when
// this goroutine starts.
func (s *Service) streamLogs(ctx context.Context, scope cluster.Scope, instanceID string, onLogLine OnLogLine) {
	pr, pw := io.Pipe()
	go func() {
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			onLogLine(scanner.Text())
		}
	}()
	defer pw.Close()

	for attempt := 0; attempt < 30; attempt++ {
		err := s.cluster.StreamLogs(ctx, scope, instanceID, cluster.LogOptions{Follow: true}, pw)
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// buildInstanceSpec constructs the single-container build instance —
// SecurityContext is entirely the underlying cluster.Client
// implementation's own concern (buildPod in direct.go applies the same
// restrictive posture, drop ALL capabilities/no privilege escalation/
// RuntimeDefault seccomp, to every instance uniformly; there is no
// per-request override to opt out of it, so this package need not — and
// cannot — ask for one).
//
// The container's own entrypoint is overridden to a shell script (Command:
// the debug image's own busybox shell, Args: the script) that:
//  1. Fetches the session's current workspace archive over HTTP with the
//     short-lived fetch token (same mechanism the console's own download
//     button uses), and unpacks it.
//  2. Writes the registry push credential to /kaniko/.docker/config.json
//     — DockerConfigJSONForBuild's own doc comment covers why this
//     travels via Env rather than a mounted Secret (InstanceSpec has no
//     Secret-volume concept), the "credential lives in env, not argv"
//     convention already established for the fetch token itself, and a
//     REAL, explicitly acknowledged tradeoff: this credential is visible
//     via `kubectl describe pod`/the container's own environment for the
//     build's lifetime, not filesystem-mounted-and-nothing-else the way a
//     Secret volume would be. It remains a narrowly-scoped, revocable
//     per-project robot account (push/pull/delete on ONE Harbor project),
//     not a platform-wide credential — the same class of exposure this
//     codebase already accepts for TEEPIN_SESSION_TOKEN.
//  3. execs the kaniko binary itself.
//
// Every value that could plausibly contain shell metacharacters
// (DockerfilePath is customer-supplied) is passed via Env and referenced
// as a quoted "$VAR" inside the script, never string-formatted into the
// script's own text — quoted env-var expansion does not re-parse for
// shell metacharacters, so this is not shell-injectable the way building
// the script text via fmt.Sprintf(dockerfilePath) would be.
func (s *Service) buildInstanceSpec(instanceID string, req Request, dockerConfigJSON, imageRef string) cluster.InstanceSpec {
	// mkdir -p /tmp first: found live 2026-08-26 that the debug Kaniko
	// image cannot be assumed to already have a /tmp directory the way a
	// full distro would — every single build was failing, 100% of the
	// time, on the very first real command (wget: can't open
	// '/tmp/workspace.zip': No such file or directory), before the
	// customer's own Dockerfile was ever reached. The script already did
	// this for /kaniko/.docker a few lines down; it just never applied
	// the same caution to /tmp.
	const script = `set -e
mkdir -p /tmp
wget -q --header="Authorization: Bearer $TEEPIN_TOKEN" -O /tmp/workspace.zip "$TEEPIN_ARCHIVE_URL"
unzip -o -q /tmp/workspace.zip -d /workspace
rm -f /tmp/workspace.zip
mkdir -p /kaniko/.docker
printf '%s' "$TEEPIN_DOCKER_CONFIG" > /kaniko/.docker/config.json
exec /kaniko/executor --dockerfile="$TEEPIN_DOCKERFILE_PATH" --context=dir:///workspace --destination="$TEEPIN_DESTINATION"
`

	return cluster.InstanceSpec{
		InstanceID: instanceID,
		ProjectID:  req.ProjectID.String(),
		Image:      s.cfg.KanikoImage,
		Command:    []string{"/busybox/sh", "-c"},
		Args:       []string{script},
		Env: map[string]string{
			"TEEPIN_TOKEN":           req.WorkspaceToken,
			"TEEPIN_ARCHIVE_URL":     req.WorkspaceArchiveURL,
			"TEEPIN_DOCKER_CONFIG":   dockerConfigJSON,
			"TEEPIN_DOCKERFILE_PATH": req.DockerfilePath,
			"TEEPIN_DESTINATION":     imageRef,
		},
		CPUUnits:           s.cfg.CPUUnits,
		MemoryGB:           s.cfg.MemoryGB,
		EphemeralStorageGB: s.cfg.EphemeralStorageGB,
		// A bare instance's default RestartPolicy (Always) would silently
		// re-run the entire build from scratch on any exit, success or
		// failure alike — the exact "silently re-ran the whole build"
		// class of incident InstanceSpec.NeverRestart's own doc comment
		// already documents from the Kumbha agent pod's history.
		NeverRestart: true,
		// Kaniko must chown/chmod arbitrary files it does not own while
		// unpacking a base image's layers — see the field's own doc
		// comment. The one workload in this codebase that legitimately
		// needs it.
		AllowFilesystemOwnershipChanges: true,
	}
}
