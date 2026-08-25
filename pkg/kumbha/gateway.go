// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// ProvisionGate answers whether an account may create resources right
// now — the same "no validated payment method, no resources" check
// compute provisioning already enforces (pkg/api.ProvisionGate).
// Duplicated as an interface here, rather than importing pkg/api, because
// pkg/api imports pkg/kumbha for the HTTP handlers — importing back would
// be a cycle. Implemented by billing.Service, same concrete type either
// side of the boundary uses.
type ProvisionGate interface {
	AccountCanProvision(ctx context.Context, accountID uuid.UUID) (bool, string, error)
}

// PricingProvider supplies the live Kumbha Gateway rates. Read fresh on
// every call by the concrete implementation (billing.Service) — same
// contract as every other rate in the platform: a price change must apply
// to the very next request, never a cached quote.
type PricingProvider interface {
	LLMPriceInputPerMillion(ctx context.Context) float64
	LLMPriceOutputPerMillion(ctx context.Context) float64
}

// UsageRecorder is the subset of billing.Service the Gateway needs at
// session close: write the ledger rows and draw down credit against them.
// An interface so kumbha's tests do not require a full *billing.Service.
type UsageRecorder interface {
	RecordUsage(ctx context.Context, record *billing.UsageRecord) error
	ConsumeCredit(ctx context.Context, accountID, usageRecordID uuid.UUID, cost float64) (float64, error)
}

var (
	// ErrGateUnavailable means the payment-status check itself failed —
	// fails CLOSED (refuses the session) rather than opening an unmetered
	// hole on a database blip, same posture as compute's payment gate.
	ErrGateUnavailable = errors.New("unable to verify billing status")
	// ErrPaymentRequired means the gate explicitly refused — no validated
	// payment method, or a non-active account.
	ErrPaymentRequired = errors.New("payment method required")
	// ErrAgentNotConfigured means LaunchAgent was called on a Gateway
	// built without WithAgent — e.g. before the Kumbha agent image exists
	// (see the plan's M3). Distinct from a runtime launch failure so a
	// caller can tell "not available on this deployment" from "briefly
	// failed, maybe retry".
	ErrAgentNotConfigured = errors.New("kumbha agent is not configured on this deployment")
)

// Gateway is the Kumbha Gateway's business logic — the request lifecycle
// in KUMBHA-DESIGN.md, minus the HTTP transport (pkg/api/kumbha_handlers.go
// owns stages 1-2 and 10-11; this is stages 3-9).
type Gateway struct {
	store   *Store
	router  *Router
	gate    ProvisionGate
	pricing PricingProvider
	usage   UsageRecorder

	// cluster/mintToken/agentConfig back LaunchAgent (agent.go) — all
	// nil/zero until WithAgent is called, which is expected to stay true
	// until the Kumbha agent image exists.
	cluster     cluster.Client
	mintToken   TokenMinter
	agentConfig AgentConfig
}

func NewGateway(store *Store, router *Router, gate ProvisionGate, pricing PricingProvider, usage UsageRecorder) *Gateway {
	return &Gateway{store: store, router: router, gate: gate, pricing: pricing, usage: usage}
}

// CreateSession pre-authorises a new build session, gated by the same
// payment check compute provisioning already enforces: an account that
// cannot create an instance cannot start a Kumbha session either.
func (g *Gateway) CreateSession(ctx context.Context, accountID, projectID uuid.UUID, budget float64, label string) (*Session, error) {
	if g.gate != nil {
		allowed, reason, err := g.gate.AccountCanProvision(ctx, accountID)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrGateUnavailable, err)
		}
		if !allowed {
			return nil, fmt.Errorf("%w: %s", ErrPaymentRequired, reason)
		}
	}
	return g.store.Create(ctx, accountID, projectID, budget, label)
}

// GetSession loads a session scoped to its owning account.
func (g *Gateway) GetSession(ctx context.Context, id, accountID uuid.UUID) (*Session, error) {
	return g.store.Get(ctx, id, accountID)
}

// ListSessions returns a project's Kumbha build history, most recent
// first.
func (g *Gateway) ListSessions(ctx context.Context, accountID, projectID uuid.UUID) ([]*Session, error) {
	return g.store.ListByProject(ctx, accountID, projectID)
}

// ApproveDeploy flips a session's pre-deploy cost-approval gate — see
// KUMBHA-DESIGN.md's "Pre-deploy cost approval" section. Called by the
// console when the customer approves the itemised Deployment Plan; the
// teepin-mcp-server's provisioning verbs (create_instance, deploy,
// attach_domain) check this via GetSession before making any real API
// call, so this is a hard backend gate a prompt-injected agent cannot
// talk its way past, not a UI-only confirmation.
func (g *Gateway) ApproveDeploy(ctx context.Context, id, accountID uuid.UUID) error {
	return g.store.SetDeployApproved(ctx, id, accountID)
}

// CompletionResult is what the HTTP handler needs to build its response —
// the provider's response plus the figures for the X-Teepin-Cost /
// X-Teepin-Session-Spent headers the design doc specifies.
type CompletionResult struct {
	Response *inference.Response
	Cost     float64
	Spent    float64
	Budget   float64
}

// Complete runs stages 3-9 of the request lifecycle: check budget,
// resolve route, dispatch, capture tokens, compute cost, accrue.
//
// A pre-flight budget check happens here (sess.Spent >= sess.Budget) so an
// already-exhausted session is refused before spending a round trip on a
// provider call; the authoritative check is still Accrue's atomic
// compare-under-lock afterward, since the pre-flight value can be stale by
// the time dispatch completes under concurrent requests.
func (g *Gateway) Complete(ctx context.Context, sess *Session, req inference.Request) (*CompletionResult, error) {
	if sess.Status != "open" {
		return nil, ErrSessionClosed
	}
	if sess.Spent >= sess.Budget {
		return nil, ErrBudgetExhausted
	}

	route, err := g.router.Resolve(req.Model)
	if err != nil {
		return nil, err
	}

	resp, err := route.Provider.Complete(ctx, req)
	if err != nil {
		return nil, err
	}

	cost := g.cost(ctx, resp.Usage)

	newSpent, err := g.store.Accrue(ctx, sess.ID, sess.AccountID, cost,
		req.Model, route.ProviderName, resp.Usage.InputTokens, resp.Usage.OutputTokens)
	if err != nil {
		// The completion already happened and tokens were genuinely spent
		// upstream — this is a recording failure, not a request failure,
		// and it is surfaced distinctly so a caller does not mistake it for
		// "no completion occurred" (which would invite a retry that spends
		// tokens twice).
		return nil, fmt.Errorf("completion served but accrual failed: %w", err)
	}

	return &CompletionResult{Response: resp, Cost: cost, Spent: newSpent, Budget: sess.Budget}, nil
}

// cost prices a completion's usage at the live admin-configured rate.
func (g *Gateway) cost(ctx context.Context, usage inference.Usage) float64 {
	inRate := g.pricing.LLMPriceInputPerMillion(ctx)
	outRate := g.pricing.LLMPriceOutputPerMillion(ctx)
	return float64(usage.InputTokens)/1e6*inRate + float64(usage.OutputTokens)/1e6*outRate
}

// CloseSession ends an open session and settles its ledger: one
// usage_records row per (route, direction) — "kumbha/fast:input",
// "kumbha/fast:output", and so on for every route the session touched —
// each immediately drawn against the account's credit balance via the
// existing ConsumeCredit path. This is what turns a session's running
// `spent` counter into an invoice-visible, credit-consuming fact; before
// this runs, spend exists only inside the session row.
//
// Best-effort per line: one route's ledger write failing must not lose
// the others — each is independent money, and losing one is a smaller
// failure than losing all of them because one had a transient DB error.
// Errors are collected and returned together so nothing is silently
// dropped.
func (g *Gateway) CloseSession(ctx context.Context, id, accountID uuid.UUID, reason string) (*Session, error) {
	sess, err := g.store.Close(ctx, id, accountID, reason)
	if err != nil {
		return nil, err
	}

	usage, err := g.store.RouteUsage(ctx, id)
	if err != nil {
		return sess, fmt.Errorf("session closed but its usage could not be loaded for settlement: %w", err)
	}

	inRate := g.pricing.LLMPriceInputPerMillion(ctx)
	outRate := g.pricing.LLMPriceOutputPerMillion(ctx)
	now := time.Now()

	var settleErrs []error
	for _, u := range usage {
		if u.InputTokens > 0 {
			settleErrs = append(settleErrs, g.settleLine(ctx, sess, u.Route+":input", u.Provider,
				float64(u.InputTokens), float64(u.InputTokens)/1e6*inRate, now))
		}
		if u.OutputTokens > 0 {
			settleErrs = append(settleErrs, g.settleLine(ctx, sess, u.Route+":output", u.Provider,
				float64(u.OutputTokens), float64(u.OutputTokens)/1e6*outRate, now))
		}
	}

	// The agent pod is Kumbha's own workload (see LaunchAgent's doc
	// comment) — nothing else deletes it, so a session that never tears
	// its pod down here leaks it permanently. The customer never sees or
	// manages it either way.
	if sess.AgentInstanceID != "" && g.cluster != nil {
		if err := g.cluster.DeleteInstance(ctx, cluster.ProjectScope(sess.ProjectID.String()), sess.AgentInstanceID); err != nil {
			settleErrs = append(settleErrs, fmt.Errorf("failed to tear down agent pod %s: %w", sess.AgentInstanceID, err))
		}
	}

	return sess, errors.Join(settleErrs...)
}

// settleLine writes one usage_records row for a session's (route,
// direction) and draws it down against the account's credits.
func (g *Gateway) settleLine(ctx context.Context, sess *Session, resourceType, provider string, quantity, totalCost float64, now time.Time) error {
	record := &billing.UsageRecord{
		AccountID:    sess.AccountID,
		ProjectID:    sess.ProjectID,
		SubjectType:  "inference_session",
		SubjectID:    sess.ID.String(),
		ResourceType: "kumbha/" + resourceType,
		Quantity:     quantity,
		Unit:         "tokens",
		TotalCost:    totalCost,
		Provider:     provider,
		StartTime:    sess.StartedAt,
		EndTime:      now,
	}
	if quantity > 0 {
		record.UnitPrice = totalCost / quantity * 1e6 // back into a per-million rate for display
	}
	if err := g.usage.RecordUsage(ctx, record); err != nil {
		return fmt.Errorf("failed to record %s: %w", resourceType, err)
	}
	if totalCost > 0 {
		if _, err := g.usage.ConsumeCredit(ctx, sess.AccountID, record.ID, totalCost); err != nil {
			return fmt.Errorf("failed to consume credit for %s: %w", resourceType, err)
		}
	}
	return nil
}
