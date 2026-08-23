// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"fmt"
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
			"TEEPIN_SESSION_TOKEN": token,
			"TEEPIN_SESSION_ID":    sess.ID.String(),
			"TEEPIN_PROMPT":        prompt,
			"TEEPIN_API_BASE_URL":  g.agentConfig.APIBaseURL,
		},
		Labels:             map[string]string{agentLabel: "true"},
		CPUUnits:           g.agentConfig.CPUUnits,
		MemoryGB:           g.agentConfig.MemoryGB,
		StorageGB:          g.agentConfig.StorageGB,
		EphemeralStorageGB: g.agentConfig.EphemeralStorageGB,
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
