// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// AgentConfig is the fixed platform policy every session's agent pod runs
// with — the image and resource sizing. Not customer-selectable: the
// agent's own compute is billed separately from whatever app it
// eventually deploys (see CloseSession's settlement), so there is nothing
// for a customer to size here, only an operator.
type AgentConfig struct {
	// Image is the Kumbha agent image (OpenHands + the teepin-mcp-server
	// binary — see the plan's M3). Required for LaunchAgent to do
	// anything; WithAgent's caller (main.go) is expected to leave the
	// whole capability unconfigured until this exists, the same way
	// execTickets/nodePlacer are conditionally wired.
	Image              string
	CPUUnits           int
	MemoryGB           int
	StorageGB          int
	EphemeralStorageGB int
	// SessionTokenTTL bounds the agent's own credential lifetime,
	// independent of the session's budget or idle timeout — belt and
	// braces alongside the session-open check every request already makes
	// (see auth.Middleware's session-scoped credential path).
	SessionTokenTTL time.Duration
	// APIBaseURL is the control-plane host the agent pod calls back to —
	// both the LLM's own base_url (run.py appends "/v1/kumbha") and the
	// teepin-mcp-server's target for every teepin.* API call. An in-cluster
	// service DNS name in production (e.g.
	// "http://teepin-api.default.svc.cluster.local:8080"), never a
	// customer-facing URL.
	APIBaseURL string
	// ImagePullSecret is the name of a Kubernetes docker-registry Secret,
	// already present in cluster.WorkloadNamespace on whichever cluster
	// LaunchAgent's pods land on, granting pull access to Image. Empty
	// means Image must be publicly pullable — the same "ships on, does
	// nothing until configured" contract as the rest of AgentConfig; there
	// is deliberately no provisioning path for this secret here (unlike a
	// customer's own registry via pkg/harbor.Service.ProvisionProjectRegistry,
	// which is keyed to a real auth.projects row) — Kumbha's own agent
	// image is a platform resource, not a per-customer one, so the Harbor
	// project/robot account/secret behind this name is operator-provisioned
	// once, out of band, directly against whichever cluster runs the pods.
	ImagePullSecret string
	// VisionCapable gates whether the agent's browser_screenshot tool
	// attaches the actual screenshot for the model to look at, versus
	// text-only console/network/page signals (see deploy/kumbha-agent/
	// browser_tool.py's module docstring). Operator-set, not
	// auto-detected: sending an image content block to a route whose
	// model cannot handle multimodal input is a real risk (a
	// malformed/rejected request breaks the whole next model turn, not
	// just the screenshot itself), so this defaults to false (safe) until
	// an operator has actually confirmed the hosted model supports
	// vision.
	VisionCapable bool
}

// TokenMinter mints the agent's own short-lived, session-scoped
// credential. A function type rather than an interface bound to
// auth.MintSessionToken directly: pkg/kumbha has no reason to import
// pkg/auth (or know what a JWT is) just to launch a pod, and this keeps
// the signing secret entirely inside main.go's composition root.
type TokenMinter func(accountID, projectID, sessionID uuid.UUID, ttl time.Duration) (string, error)

// agentLabel marks a pod as Kumbha's own workload — used only for
// operator visibility (e.g. filtering in kubectl); it plays no role in
// keeping the pod out of the customer's Compute list, which is achieved
// structurally by never inserting a compute.instances row for it (see
// LaunchAgent's doc comment).
const agentLabel = "teepin.io/kumbha-agent"

// WithAgent enables LaunchAgent. Returns the same *Gateway for chaining,
// so existing NewGateway call sites compile unchanged — without this,
// LaunchAgent returns ErrAgentNotConfigured rather than launching
// anything, the same fail-closed shape as every other optional capability
// in this codebase (s.execTickets == nil, s.nodePlacer == nil, ...).
func (g *Gateway) WithAgent(client cluster.Client, mintToken TokenMinter, cfg AgentConfig) *Gateway {
	g.cluster = client
	g.mintToken = mintToken
	g.agentConfig = cfg
	return g
}

// LaunchAgent provisions the Kumbha agent pod for a session and records
// its identifier on the session row.
//
// The pod is created by calling cluster.Client.CreateInstance DIRECTLY —
// the same interface method the public POST /v1/compute/instances handler
// calls, reusing 100% of its pod-building machinery (SecurityContext,
// NetworkPolicy, PVC, ephemeral-storage cap) — but NOT through that public
// handler, and the pod is never inserted into compute.instances. This is
// deliberate: the agent's own workload is Kumbha's, not a resource the
// customer manages, so a mystery "kumbha-agent-xyz" entry never appears in
// their Compute list. What the customer DOES eventually manage is the
// finished app the agent creates via the real API once deploy is
// approved — that one lands in compute.instances exactly like any
// customer-created instance always has.
func (g *Gateway) LaunchAgent(ctx context.Context, sess *Session, prompt string) error {
	if g.cluster == nil || g.mintToken == nil {
		return ErrAgentNotConfigured
	}

	token, err := g.mintToken(sess.AccountID, sess.ProjectID, sess.ID, g.agentConfig.SessionTokenTTL)
	if err != nil {
		return fmt.Errorf("failed to mint agent credential: %w", err)
	}

	// Short and deterministic from the session id, matching the existing
	// "inst-<short-uuid>" convention compute instances already use, so an
	// operator recognises the shape immediately in kubectl/logs even
	// though this pod is never customer-visible.
	podID := "kumbha-agent-" + sess.ID.String()[:8]

	spec := cluster.InstanceSpec{
		InstanceID: podID,
		AccountID:  sess.AccountID.String(),
		ProjectID:  sess.ProjectID.String(),
		Image:      g.agentConfig.Image,
		Env: map[string]string{
			// The agent's own credential and everything it needs to start
			// working — never a platform-wide token, never long-lived (see
			// TokenMinter's doc comment and MintSessionToken).
			"TEEPIN_SESSION_TOKEN":  token,
			"TEEPIN_SESSION_ID":     sess.ID.String(),
			"TEEPIN_PROMPT":         prompt,
			"TEEPIN_API_BASE_URL":   g.agentConfig.APIBaseURL,
			"TEEPIN_VISION_CAPABLE": strconv.FormatBool(g.agentConfig.VisionCapable),
		},
		Labels:             map[string]string{agentLabel: "true"},
		CPUUnits:           g.agentConfig.CPUUnits,
		MemoryGB:           g.agentConfig.MemoryGB,
		StorageGB:          g.agentConfig.StorageGB,
		EphemeralStorageGB: g.agentConfig.EphemeralStorageGB,
		ImagePullSecret:    g.agentConfig.ImagePullSecret,
		// The agent image's tag is not a genuinely immutable identifier
		// today (see the proto field's own doc comment) — force a fresh
		// pull every launch rather than risk a node silently reusing a
		// stale cached image under the same tag.
		AlwaysPullImage: true,
		// A bare Pod's default RestartPolicy (Always) is correct for a
		// customer's persistent compute instance and wrong for this one:
		// the agent is a one-shot process that is SUPPOSED to exit when
		// it finishes (or after wait_for_deploy_approval's bounded wait) —
		// left at the default, a Kubernetes restart would silently
		// re-run the entire build from scratch against the same original
		// prompt, on repeat, burning the customer's session budget on
		// duplicate work (found live 2026-08-24: a build session's
		// activity feed showed re-generated, differently-worded text for
		// actions already completed — the tell that this had already
		// happened before this fix).
		NeverRestart: true,
	}

	if _, err := g.cluster.CreateInstance(ctx, spec); err != nil {
		return fmt.Errorf("failed to launch agent: %w", err)
	}

	if err := g.store.SetAgentInstanceID(ctx, sess.ID, podID); err != nil {
		// The pod is running but the session does not know its own agent's
		// id — clean up immediately rather than leak a pod nothing can
		// find to delete later (CloseSession only tears down what
		// sess.AgentInstanceID names).
		_ = g.cluster.DeleteInstance(ctx, cluster.ProjectScope(sess.ProjectID.String()), podID)
		return fmt.Errorf("agent launched but could not be recorded on the session: %w", err)
	}
	sess.AgentInstanceID = podID
	return nil
}

// MintWorkspaceFetchToken mints a short-lived credential authorising
// exactly a GET of this session's current workspace archive, plus the URL
// to fetch it from. Used by the build pipeline's Kaniko pod: an
// initContainer fetches the archive over HTTP using this token and unpacks
// it as the build context, instead of mounting the agent's own live PVC —
// so a customer's IDE edit or a rollback (both of which only ever change
// what pkg/kumbha/workspace.go considers "current", never the agent pod's
// own working copy) is what actually gets built, not whatever the agent's
// pod happens to still have on disk.
//
// Reuses the exact same TokenMinter and APIBaseURL WithAgent already
// configured — same trust boundary and TTL policy as the agent's own
// credential, not a second mechanism to keep in sync. Returns
// ErrAgentNotConfigured if WithAgent was never called: without it there is
// no APIBaseURL to build a reachable URL from, so there is nothing this
// method could correctly return.
func (g *Gateway) MintWorkspaceFetchToken(sess *Session, ttl time.Duration) (token, archiveURL string, err error) {
	if g.mintToken == nil || g.agentConfig.APIBaseURL == "" {
		return "", "", ErrAgentNotConfigured
	}
	token, err = g.mintToken(sess.AccountID, sess.ProjectID, sess.ID, ttl)
	if err != nil {
		return "", "", fmt.Errorf("failed to mint workspace fetch token: %w", err)
	}
	archiveURL = strings.TrimRight(g.agentConfig.APIBaseURL, "/") +
		"/v1/kumbha/sessions/" + sess.ID.String() + "/workspace/archive"
	return token, archiveURL, nil
}

// isAgentRunning reports whether a session's agent pod is currently
// pending or running — "will eventually reach its poll loop", not
// "idle right now". A pod mid-completion still counts: it will pick up
// a queued message once its current turn ends. False (with no error) for
// both "no agent has ever been launched" and "the pod has exited" — the
// caller (DeliverMessage) treats both the same way, by launching one.
func (g *Gateway) isAgentRunning(ctx context.Context, sess *Session) (bool, error) {
	if g.cluster == nil || sess.AgentInstanceID == "" {
		return false, nil
	}
	status, err := g.cluster.GetInstanceStatus(ctx, cluster.ProjectScope(sess.ProjectID.String()), sess.AgentInstanceID)
	if err != nil {
		if errors.Is(err, cluster.ErrNotFound) {
			return false, nil
		}
		// An ambiguous failure (cluster unreachable, etc.) is NOT the same
		// as "confirmed gone" — propagate rather than guess and risk
		// launching a duplicate agent pod alongside one that is actually
		// still fine.
		return false, fmt.Errorf("failed to check agent status: %w", err)
	}
	return status.Status == "pending" || status.Status == "running", nil
}

// DeliverMessage is the whole of "chat + resume": queue a follow-up for
// the agent's own poll loop to pick up if it's still alive (run.py's
// wait_for_next_instruction, resuming the SAME conversation with full
// history intact — nothing about a queued message launches a new agent by
// itself), or relaunch the agent with the message as a fresh prompt if the
// previous pod already exited.
//
// The relaunch path is an honest, bounded degradation, not real
// conversation persistence: a relaunched agent has NO memory of what it
// discussed before (no persistence_dir/conversation_id wiring exists
// yet — a real follow-up item, not silently glossed over here) — but the
// WORKSPACE it built is still there (versioned, on disk via the PVC), and
// the relaunch prompt says so explicitly, so the agent inspects what
// exists rather than starting over or contradicting itself.
//
// Returns relaunched=true when a new agent pod was started rather than an
// existing one resuming — the caller (SendMessage's HTTP handler) surfaces
// this so the console can explain a longer-than-usual wait before the
// activity feed picks back up.
func (g *Gateway) DeliverMessage(ctx context.Context, sess *Session, content string) (relaunched bool, err error) {
	running, err := g.isAgentRunning(ctx, sess)
	if err != nil {
		return false, err
	}
	if running {
		if _, err := g.store.SendMessage(ctx, sess.ID, content); err != nil {
			return false, err
		}
		return false, nil
	}

	if g.mintToken == nil {
		return false, ErrAgentNotConfigured
	}
	resumePrompt := "The customer sent a new message after your previous run ended: \"" + content + "\"\n\n" +
		"Your workspace already contains what you built before this message — inspect its current " +
		"contents before making any change, and do not discard or rewrite existing work unless the " +
		"customer explicitly asks you to."
	if err := g.LaunchAgent(ctx, sess, resumePrompt); err != nil {
		return false, err
	}
	return true, nil
}
