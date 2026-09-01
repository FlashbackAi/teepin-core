// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	// ErrAgentNotRunning means StopAgent was called on a session with no
	// live agent pod — nothing to interrupt. Distinct from
	// ErrAgentNotConfigured (no agent capability at all) so the HTTP layer
	// can return a clear "nothing running" 409 rather than a confusing
	// "not available on this deployment".
	ErrAgentNotRunning = errors.New("no agent is currently running for this session")
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

// IncreaseBudget raises an open session's pre-authorised spend cap — the
// console's "raise budget" control (build/[id]/budget-meter.tsx),
// replacing the old up-front budget picker on the composer: a customer
// cannot sensibly judge a build's cost before it has started, so every
// session now starts at a fixed default and asks for more only once
// there is real spend to judge it against. Same payment-gate check
// CreateSession applies — more authorised spend is the same kind of
// commitment a new session's own budget is.
//
// Loads the session first (rather than leaving the "was it too low"
// distinction to a SQL WHERE clause's RowsAffected) so the caller gets a
// specific, actionable error — ErrBudgetNotIncreased vs ErrSessionNotFound
// vs ErrSessionClosed — instead of one ambiguous failure for all three.
func (g *Gateway) IncreaseBudget(ctx context.Context, id, accountID uuid.UUID, newBudget float64) error {
	if g.gate != nil {
		allowed, reason, err := g.gate.AccountCanProvision(ctx, accountID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrGateUnavailable, err)
		}
		if !allowed {
			return fmt.Errorf("%w: %s", ErrPaymentRequired, reason)
		}
	}

	sess, err := g.store.Get(ctx, id, accountID)
	if err != nil {
		return err
	}
	if sess.Status != "open" {
		return ErrSessionClosed
	}
	if newBudget <= sess.Budget {
		return ErrBudgetNotIncreased
	}

	return g.store.IncreaseBudget(ctx, id, accountID, newBudget)
}

// ListSessions returns a project's Kumbha build history, most recent
// first.
func (g *Gateway) ListSessions(ctx context.Context, accountID, projectID uuid.UUID) ([]*Session, error) {
	return g.store.ListByProject(ctx, accountID, projectID)
}

// DeleteSessions removes the given sessions from the account's build
// history — a customer explicitly requesting a delete (through a
// confirming UI, see build/page.tsx's BulkDeleteDialog) is authorization
// to stop it too if it is still building, not just to clean up ones that
// already finished. There is no separate "stop, then delete" step:
// found live 2026-08-26 that requiring one is worse UX for no real safety
// gain, since the confirmation dialog already IS the "are you sure" gate
// — the same one-step pattern GitHub Actions/Vercel/Render use for
// cancelling a running job.
//
// For each requested id still marked "open" in the DB, this closes it
// first via CloseSession — settling any unbilled usage and tearing down
// the agent pod, exactly as an explicit customer Close already does —
// before Store.Delete removes the row. This also fixes a related bug
// found the same day: the stored status column alone is not trustworthy
// evidence of a live pod (nothing closes a session whose pod died on its
// own — crashed, evicted, or exited after its own idle timeout — without
// ever reaching CloseSession), so without this step an account's history
// could get stuck showing "Building" indefinitely and be entirely
// undeletable.
func (g *Gateway) DeleteSessions(ctx context.Context, accountID uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error) {
	for _, id := range ids {
		sess, err := g.store.Get(ctx, id, accountID)
		if err != nil || sess.Status != "open" {
			continue // not found, wrong account, or already closed — Store.Delete handles it
		}
		if _, err := g.CloseSession(ctx, id, accountID, "closed"); err != nil {
			// Best-effort, same posture as CloseSession's own settlement
			// errors: log for an operator, but still let Store.Delete
			// below decide this row's fate on its now-updated status.
			log.Printf("WARN: closing Kumbha session %s before delete had settlement errors: %v", id, err)
		}
	}
	return g.store.Delete(ctx, accountID, ids)
}

// SaveWorkspaceVersion appends a new workspace version for a session and
// moves the current-version pointer to it. createdBy distinguishes an
// agent's automatic save (after a file_editor call) from a customer's
// explicit edit-and-save in the console IDE — both go through this one
// path, since both are just "a new version," differing only in who made
// it. Returns the new version number.
func (g *Gateway) SaveWorkspaceVersion(ctx context.Context, sessionID uuid.UUID, files []WorkspaceFile, skipped []SkippedFile, createdBy CreatedBy) (int, error) {
	return g.store.SaveVersion(ctx, sessionID, files, skipped, createdBy)
}

// CurrentWorkspace returns whatever version is currently live for a
// session, scoped to the owning account — what the file browser and ZIP
// download show by default.
func (g *Gateway) CurrentWorkspace(ctx context.Context, sessionID, accountID uuid.UUID) (*Snapshot, error) {
	return g.store.GetCurrentVersion(ctx, sessionID, accountID)
}

// WorkspaceVersion returns one specific version, scoped to the owning
// account — used to view or download an older version before deciding
// whether to roll back to it.
func (g *Gateway) WorkspaceVersion(ctx context.Context, sessionID, accountID uuid.UUID, version int) (*Snapshot, error) {
	return g.store.GetVersion(ctx, sessionID, accountID, version)
}

// WorkspaceHistory returns every checkpointed version's metadata (no file
// content), newest first, scoped to the owning account — the console's
// version history list.
func (g *Gateway) WorkspaceHistory(ctx context.Context, sessionID, accountID uuid.UUID) ([]VersionInfo, error) {
	return g.store.ListVersions(ctx, sessionID, accountID)
}

// CheckpointWorkspace marks the session's current draft workspace version
// as a permanent, customer-visible checkpoint — called once, right after
// a Kumbha deploy actually succeeds (see DeployKumbhaSession). See
// Store.CheckpointCurrentVersion for the full reasoning.
func (g *Gateway) CheckpointWorkspace(ctx context.Context, sessionID uuid.UUID) error {
	return g.store.CheckpointCurrentVersion(ctx, sessionID)
}

// PollMessages returns and marks-delivered every undelivered follow-up
// message for a session — called by the agent pod's own poll loop
// (run.py's wait_for_next_instruction), authenticated with its
// session-scoped credential, not a customer JWT.
func (g *Gateway) PollMessages(ctx context.Context, sessionID uuid.UUID) ([]Message, error) {
	return g.store.PollMessages(ctx, sessionID)
}

// SetAppInstanceID records the compute instance a session's Deploy most
// recently created — see migration 026 and Session.AppInstanceID's own
// doc comment.
func (g *Gateway) SetAppInstanceID(ctx context.Context, sessionID uuid.UUID, instanceID string) error {
	return g.store.SetAppInstanceID(ctx, sessionID, instanceID)
}

// AcquireDeployLock/ReleaseDeployLock expose Store's own methods of the
// same name — see AcquireDeployLock's own doc comment for the incident
// this closes (two overlapping deploy calls for one session racing each
// other's outcome).
func (g *Gateway) AcquireDeployLock(ctx context.Context, sessionID uuid.UUID, staleAfter time.Duration) (bool, error) {
	return g.store.AcquireDeployLock(ctx, sessionID, staleAfter)
}

func (g *Gateway) ReleaseDeployLock(ctx context.Context, sessionID uuid.UUID) error {
	return g.store.ReleaseDeployLock(ctx, sessionID)
}

// GetGithubRepo/SetGithubRepo expose Store's own methods of the same
// name — see their doc comments (pkg/kumbha/session.go) for why this is
// deliberately not a Session field.
func (g *Gateway) GetGithubRepo(ctx context.Context, sessionID uuid.UUID) (string, error) {
	return g.store.GetGithubRepo(ctx, sessionID)
}

func (g *Gateway) SetGithubRepo(ctx context.Context, sessionID uuid.UUID, repo string) error {
	return g.store.SetGithubRepo(ctx, sessionID, repo)
}

// SaveScreenshot records the deployed app's most recently captured
// screenshot — called only by the screenshot pod's own upload endpoint,
// see Store.SaveScreenshot.
func (g *Gateway) SaveScreenshot(ctx context.Context, sessionID uuid.UUID, png []byte) error {
	return g.store.SaveScreenshot(ctx, sessionID, png)
}

// SetLastDeployStatus records the outcome of a session's most recent
// build/deploy attempt — see Store.SetLastDeployStatus.
func (g *Gateway) SetLastDeployStatus(ctx context.Context, sessionID uuid.UUID, errMsg string) error {
	return g.store.SetLastDeployStatus(ctx, sessionID, errMsg)
}

// Screenshot returns a session's most recently captured screenshot,
// scoped to the owning account — see Store.GetScreenshot.
func (g *Gateway) Screenshot(ctx context.Context, sessionID, accountID uuid.UUID) ([]byte, time.Time, error) {
	return g.store.GetScreenshot(ctx, sessionID, accountID)
}

// RollbackWorkspace moves the current-version pointer to an existing,
// older (or newer) version — the undo for a customer edit or an agent
// step that broke something. Does not delete or overwrite anything: the
// version being rolled back FROM is still there, so a rollback can itself
// be undone by rolling forward again.
func (g *Gateway) RollbackWorkspace(ctx context.Context, sessionID, accountID uuid.UUID, version int) error {
	return g.store.SetCurrentVersion(ctx, sessionID, accountID, version)
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

	start := time.Now()
	resp, err := route.Provider.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	end := time.Now()

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

	// Settled immediately, per completion — not batched until the session
	// closes. Billing is metered per-token already; there is no reason a
	// customer's ability to keep chatting (and thus keep building) should
	// depend on an explicit "close this session" action, and batching to
	// Close meant a session that was simply abandoned (never explicitly
	// closed) left its spend sitting unrecorded in usage_records
	// indefinitely. See CloseSession's own doc comment for what it does
	// instead now (pull down its own agent pod only — settlement no
	// longer happens there at all, so a session's `spent` counter and its
	// invoice-visible cost never diverge). Best-effort in the sense that a
	// settlement failure surfaces to the CALLER (so the customer sees it,
	// same as an accrual failure) but the tokens were already genuinely
	// spent upstream either way.
	inRate := g.pricing.LLMPriceInputPerMillion(ctx)
	outRate := g.pricing.LLMPriceOutputPerMillion(ctx)
	var settleErrs []error
	if resp.Usage.InputTokens > 0 {
		settleErrs = append(settleErrs, g.settleLine(ctx, sess, req.Model+":input", route.ProviderName,
			float64(resp.Usage.InputTokens), float64(resp.Usage.InputTokens)/1e6*inRate, start, end))
	}
	if resp.Usage.OutputTokens > 0 {
		settleErrs = append(settleErrs, g.settleLine(ctx, sess, req.Model+":output", route.ProviderName,
			float64(resp.Usage.OutputTokens), float64(resp.Usage.OutputTokens)/1e6*outRate, start, end))
	}
	if err := errors.Join(settleErrs...); err != nil {
		return nil, fmt.Errorf("completion served but settlement failed: %w", err)
	}

	return &CompletionResult{Response: resp, Cost: cost, Spent: newSpent, Budget: sess.Budget}, nil
}

// cost prices a completion's usage at the live admin-configured rate.
func (g *Gateway) cost(ctx context.Context, usage inference.Usage) float64 {
	inRate := g.pricing.LLMPriceInputPerMillion(ctx)
	outRate := g.pricing.LLMPriceOutputPerMillion(ctx)
	return float64(usage.InputTokens)/1e6*inRate + float64(usage.OutputTokens)/1e6*outRate
}

// CloseSession marks a session closed and tears down its agent pod.
//
// No longer settles any billing — Complete() settles every completion's
// cost into usage_records IMMEDIATELY as it happens (see its own doc
// comment on why: a customer's ability to keep chatting must not depend
// on an explicit close, and batching settlement to Close left an
// abandoned-but-never-closed session's spend sitting unrecorded
// indefinitely). Settling AGAIN here from RouteUsage's cumulative totals
// would double-charge the account for everything Complete() already
// settled. This is now purely internal bookkeeping — there is no
// customer-facing "Close session" action any more; the only caller is
// DeleteSessions' own stop-then-delete step, closing a still-open session
// immediately before removing its row.
func (g *Gateway) CloseSession(ctx context.Context, id, accountID uuid.UUID, reason string) (*Session, error) {
	sess, err := g.store.Close(ctx, id, accountID, reason)
	if err != nil {
		return nil, err
	}

	// The agent pod is Kumbha's own workload (see LaunchAgent's doc
	// comment) — nothing else deletes it, so a session that never tears
	// its pod down here leaks it permanently. The customer never sees or
	// manages it either way.
	if sess.AgentInstanceID != "" && g.cluster != nil {
		if err := g.cluster.DeleteInstance(ctx, cluster.ProjectScope(sess.ProjectID.String()), sess.AgentInstanceID); err != nil {
			return sess, fmt.Errorf("failed to tear down agent pod %s: %w", sess.AgentInstanceID, err)
		}
	}

	return sess, nil
}

// StopAgent interrupts a session's currently-running agent pod immediately
// — the "Stop" action (replaces the old "Close session" button, which
// bundled an unrelated permanent chat-block into what was really just
// "I want this run to stop"). Deliberately a hard kill, not a graceful
// mid-turn pause: openhands-sdk exposes no confirmed public API for
// cleanly cancelling an in-flight conversation step (see
// deploy/kumbha-agent/run.py's own flagged uncertainty about this SDK's
// exact surface elsewhere), and guessing at one here would repeat exactly
// the class of mistake this codebase has been careful to avoid throughout
// Kumbha's build-out. Whatever the agent had written to the workspace
// (PVC) up to the moment of the kill is kept — DeliverMessage's own
// relaunch path already handles resuming from there with a fresh agent
// turn, the same as when a pod exits on its own idle timeout. The session
// itself is left "open": nothing about Stop should block a later message.
func (g *Gateway) StopAgent(ctx context.Context, sess *Session) error {
	if g.cluster == nil {
		return ErrAgentNotConfigured
	}
	// No agent pod ever recorded on this session is exactly as "nothing
	// running to interrupt" as one that was recorded but has since
	// exited — both are ErrAgentNotRunning, not ErrAgentNotConfigured
	// (reserved for "this platform has no agent capability at all").
	// isAgentRunning already treats sess.AgentInstanceID == "" as false
	// with no error, so this short-circuit just skips a redundant call.
	if sess.AgentInstanceID == "" {
		return ErrAgentNotRunning
	}
	running, err := g.isAgentRunning(ctx, sess)
	if err != nil {
		return fmt.Errorf("failed to check agent status: %w", err)
	}
	if !running {
		return ErrAgentNotRunning
	}
	return g.cluster.DeleteInstance(ctx, cluster.ProjectScope(sess.ProjectID.String()), sess.AgentInstanceID)
}

// settleLine writes one usage_records row for a session's (route,
// direction) and draws it down against the account's credits.
func (g *Gateway) settleLine(ctx context.Context, sess *Session, resourceType, provider string, quantity, totalCost float64, start, end time.Time) error {
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
		StartTime:    start,
		EndTime:      end,
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
