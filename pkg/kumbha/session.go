// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package kumbha implements the Kumbha Gateway's business logic — the
// pre-authorised, metered, routed path every model call must go through.
// See KUMBHA-DESIGN.md's "The Kumbha Gateway — full specification" for the
// full request lifecycle this package implements the middle of (stages
// 3-9); pkg/api/kumbha_handlers.go owns the HTTP transport around it
// (stages 1-2 and 10-11), the same split pkg/billing.Service and
// pkg/api.BillingHandler already use.
package kumbha

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// maxSessionBudget caps a single session's pre-authorised spend. Mirrors
// billing.maxGrant's reasoning: a typo'd extra zero on a budget request
// must not pre-authorise $500 of inference before a single token is spent.
// Compiled in rather than configurable, so raising it is a reviewed code
// change.
const maxSessionBudget = 500.0

var (
	// ErrSessionNotFound means the session does not exist, or does not
	// belong to the caller — indistinguishable on purpose (existence must
	// not leak), matching requireScope's pattern elsewhere in the API.
	ErrSessionNotFound = errors.New("session not found")
	// ErrSessionClosed means the session exists but is no longer open —
	// kept distinct from not-found because a closed session id is
	// legitimately something a client held a moment ago and needs a
	// different customer-facing message for.
	ErrSessionClosed = errors.New("session is closed")
	// ErrBudgetExhausted means the request would push spend past the
	// session's pre-authorised budget. Maps to 402 at the HTTP layer.
	ErrBudgetExhausted = errors.New("session budget exhausted")
	// ErrBudgetNotIncreased means IncreaseBudget was called with a value
	// at or below the session's current budget — a raise, not a generic
	// overwrite; a customer cannot use this to accidentally (or
	// deliberately) shrink their own already-authorised spend cap.
	ErrBudgetNotIncreased = errors.New("new budget must be higher than the current budget")
)

// Session mirrors one billing.inference_sessions row.
type Session struct {
	ID              uuid.UUID
	AccountID       uuid.UUID
	ProjectID       uuid.UUID
	Budget          float64
	Spent           float64
	Status          string // "open" | "closed" | "budget_exhausted" | "idle_timeout"
	Label           string
	AgentInstanceID string
	// AppInstanceID names the customer-facing app instance this session
	// deployed (a real compute.instances row, billed and manageable like
	// any other) — distinct from AgentInstanceID, which is Kumbha's own
	// never-customer-visible agent pod. Set once, by the session's FIRST
	// deploy; every deploy after that swaps THIS instance's pod in place
	// (pkg/api.redeployKumbhaInstance, cluster.Client.UpdateInstance)
	// rather than replacing it — see that function's own doc comment.
	AppInstanceID  string
	DeployApproved bool
	// LastDeployFailed/LastDeployError/LastDeployAt persist the OUTCOME of
	// the most recent build/deploy attempt (see migration 030) — what
	// backs the "Failed" status on the console's "Previous builds" list.
	// Set by buildKumbhaImage/DeployKumbhaSession/redeployKumbhaInstance
	// on both failure (true + the error) and success (false, cleared),
	// so a stale failure from days ago never lingers once the customer
	// has since shipped successfully. LastDeployFailed can be true while
	// AppInstanceID still names a perfectly healthy, currently-running
	// instance — the LATEST attempt failing does not touch whatever an
	// earlier successful deploy already has running.
	LastDeployFailed bool
	LastDeployError  string
	LastDeployAt     *time.Time
	StartedAt        time.Time
	EndedAt          *time.Time
}

// RouteUsage is one session's accumulated tokens for one route — the raw
// material CloseSession turns into the two-line-per-route usage_records
// split (see the migration 024 comment on inference_session_usage).
type RouteUsage struct {
	Route        string
	Provider     string
	InputTokens  int64
	OutputTokens int64
}

// Store persists Kumbha sessions and their per-route usage.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Create pre-authorises a new session. budget must be positive and within
// maxSessionBudget — see its comment.
func (s *Store) Create(ctx context.Context, accountID, projectID uuid.UUID, budget float64, label string) (*Session, error) {
	if budget <= 0 {
		return nil, fmt.Errorf("budget must be positive")
	}
	if budget > maxSessionBudget {
		return nil, fmt.Errorf("budget %.2f exceeds the per-session cap of %.2f", budget, maxSessionBudget)
	}

	sess := &Session{AccountID: accountID, ProjectID: projectID, Budget: budget, Label: label}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO billing.inference_sessions (account_id, project_id, budget, label)
		VALUES ($1, $2, $3, $4)
		RETURNING id, spent, status, started_at
	`, accountID, projectID, budget, nullIfEmpty(label)).Scan(&sess.ID, &sess.Spent, &sess.Status, &sess.StartedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}
	return sess, nil
}

// IsSessionOpen reports whether id belongs to accountID and is currently
// open — a single indexed lookup, deliberately cheaper than Get, because
// this runs on EVERY request authenticated with a Kumbha agent credential
// (see auth.SessionChecker, which this method satisfies). A false result
// covers "not found", "wrong account" and "closed" alike; the caller
// (auth.Middleware) is not meant to distinguish them.
func (s *Store) IsSessionOpen(ctx context.Context, id, accountID uuid.UUID) (bool, error) {
	var open bool
	err := s.db.QueryRowContext(ctx, `
		SELECT status = 'open' FROM billing.inference_sessions
		WHERE id = $1 AND account_id = $2
	`, id, accountID).Scan(&open)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to check session status: %w", err)
	}
	return open, nil
}

// IncreaseBudget raises an open session's pre-authorised spend cap — the
// console's "raise budget" control on the live budget meter, added
// alongside removing the composer's upfront budget picker (a customer
// cannot sensibly judge a build's cost before it has started; this lets
// them ask for more once they can actually see spend against the fixed
// default they started with). Same validation as Create (positive, at
// most maxSessionBudget) plus one more: the new value must be STRICTLY
// GREATER than the current budget — this is a one-way raise, not a
// generic overwrite, so it can never be used to shrink a session's spend
// cap out from under an agent mid-turn.
func (s *Store) IncreaseBudget(ctx context.Context, id, accountID uuid.UUID, newBudget float64) error {
	if newBudget > maxSessionBudget {
		return fmt.Errorf("budget %.2f exceeds the per-session cap of %.2f", newBudget, maxSessionBudget)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE billing.inference_sessions SET budget = $1
		WHERE id = $2 AND account_id = $3 AND status = 'open' AND $1 > budget
	`, newBudget, id, accountID)
	if err != nil {
		return fmt.Errorf("failed to increase budget: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		// Ambiguous on purpose, same as SetDeployApproved: could be "no
		// such open session" or "newBudget was not actually higher" — the
		// caller (Gateway.IncreaseBudget) checks the latter itself first
		// and returns a specific error for it, so by the time execution
		// reaches here a zero-rows result really does mean "not found or
		// not open".
		return ErrSessionNotFound
	}
	return nil
}

// SetDeployApproved flips the pre-deploy cost-approval gate — see
// KUMBHA-DESIGN.md's "Pre-deploy cost approval" section. Scoped to the
// owning account and requires the session to still be open: approving a
// closed session's spend is meaningless and would only confuse an
// operator reading the row later.
func (s *Store) SetDeployApproved(ctx context.Context, id, accountID uuid.UUID) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE billing.inference_sessions SET deploy_approved = true
		WHERE id = $1 AND account_id = $2 AND status = 'open'
	`, id, accountID)
	if err != nil {
		return fmt.Errorf("failed to approve deploy: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// Get loads a session, scoped to the owning account.
func (s *Store) Get(ctx context.Context, id, accountID uuid.UUID) (*Session, error) {
	var sess Session
	var label, agentInstanceID, appInstanceID, lastDeployError sql.NullString
	var endedAt, lastDeployAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, account_id, project_id, budget, spent, status, label,
		       agent_instance_id, app_instance_id, deploy_approved, started_at, ended_at,
		       last_deploy_failed, last_deploy_error, last_deploy_at
		FROM billing.inference_sessions
		WHERE id = $1 AND account_id = $2
	`, id, accountID).Scan(&sess.ID, &sess.AccountID, &sess.ProjectID, &sess.Budget,
		&sess.Spent, &sess.Status, &label, &agentInstanceID, &appInstanceID, &sess.DeployApproved,
		&sess.StartedAt, &endedAt, &sess.LastDeployFailed, &lastDeployError, &lastDeployAt)
	if err == sql.ErrNoRows {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load session: %w", err)
	}
	sess.Label = label.String
	sess.AgentInstanceID = agentInstanceID.String
	sess.AppInstanceID = appInstanceID.String
	sess.LastDeployError = lastDeployError.String
	if endedAt.Valid {
		sess.EndedAt = &endedAt.Time
	}
	if lastDeployAt.Valid {
		sess.LastDeployAt = &lastDeployAt.Time
	}
	return &sess, nil
}

// Accrue atomically records one completion's cost and tokens against a
// session: locks the session row, refuses if it is not open or the cost
// would exceed budget, then updates both the running spend and the
// per-route token counters in the same transaction.
//
// Locked with FOR UPDATE rather than a compare-and-set UPDATE, matching
// billing.Service.ConsumeCredit's idiom exactly — the same reasoning
// applies: two concurrent accruals must not both read a balance that is
// stale by the time either writes. The design doc's noted residual race
// (overshoot bounded by max_tokens*concurrency across requests that were
// already in flight before either committed) is unaffected by this choice
// either way; closing it needs a per-session concurrency cap, not built
// here.
func (s *Store) Accrue(ctx context.Context, id, accountID uuid.UUID, cost float64, route, provider string, inputTokens, outputTokens int) (newSpent float64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var status string
	var budget, spent float64
	err = tx.QueryRowContext(ctx, `
		SELECT status, budget, spent FROM billing.inference_sessions
		WHERE id = $1 AND account_id = $2
		FOR UPDATE
	`, id, accountID).Scan(&status, &budget, &spent)
	if err == sql.ErrNoRows {
		return 0, ErrSessionNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("failed to lock session: %w", err)
	}
	if status != "open" {
		return 0, ErrSessionClosed
	}
	newSpent = spent + cost
	if newSpent > budget {
		return 0, ErrBudgetExhausted
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE billing.inference_sessions SET spent = $2 WHERE id = $1
	`, id, newSpent); err != nil {
		return 0, fmt.Errorf("failed to record accrual: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing.inference_session_usage (session_id, route, provider, input_tokens, output_tokens)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (session_id, route) DO UPDATE
		SET input_tokens  = billing.inference_session_usage.input_tokens  + EXCLUDED.input_tokens,
		    output_tokens = billing.inference_session_usage.output_tokens + EXCLUDED.output_tokens,
		    provider      = EXCLUDED.provider
	`, id, route, provider, inputTokens, outputTokens); err != nil {
		return 0, fmt.Errorf("failed to record route usage: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit accrual: %w", err)
	}
	return newSpent, nil
}

// RouteUsage returns a session's accumulated per-route token totals — the
// raw material for CloseSession's usage_records lines.
// SetAgentInstanceID records which pod is running this session's Kumbha
// agent — set once, right after LaunchAgent's CreateInstance call
// succeeds, so CloseSession later knows what to tear down.
func (s *Store) SetAgentInstanceID(ctx context.Context, sessionID uuid.UUID, agentInstanceID string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE billing.inference_sessions SET agent_instance_id = $2 WHERE id = $1
	`, sessionID, agentInstanceID)
	if err != nil {
		return fmt.Errorf("failed to record agent instance id: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// SetAppInstanceID records which compute instance a Deploy most recently
// created for this session — set right after the create succeeds, so the
// NEXT deploy knows what to tear down (see migration 026). An empty
// string clears it (used when the instance is torn down without a
// replacement, e.g. DeployKumbhaSession's own cleanup-on-partial-failure
// path).
func (s *Store) SetAppInstanceID(ctx context.Context, sessionID uuid.UUID, appInstanceID string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE billing.inference_sessions SET app_instance_id = $2 WHERE id = $1
	`, sessionID, nullIfEmpty(appInstanceID))
	if err != nil {
		return fmt.Errorf("failed to record app instance id: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// GetGithubRepo returns the Teepin-owned GitHub repo (migration 034)
// already provisioned for this session's checkpoint pushes, or "" if
// none has been created yet. Deliberately NOT a field on Session/Get —
// this is internal bookkeeping the customer must never see, and keeping
// it out of the shared struct means no existing SELECT that scans a
// Session needs to change, and no existing response-building code
// (kumbhaSessionResponse and friends, pkg/api/kumbha_handlers.go) can
// accidentally start returning it.
func (s *Store) GetGithubRepo(ctx context.Context, sessionID uuid.UUID) (string, error) {
	var repo sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT github_repo FROM billing.inference_sessions WHERE id = $1
	`, sessionID).Scan(&repo)
	if err == sql.ErrNoRows {
		return "", ErrSessionNotFound
	}
	if err != nil {
		return "", fmt.Errorf("failed to load github repo: %w", err)
	}
	return repo.String, nil
}

// SetGithubRepo records the repo ProvisionRepo just created for this
// session — called once, the first time a checkpoint push happens; every
// later push reuses the value GetGithubRepo returns instead of
// re-provisioning. See GetGithubRepo's own doc comment for why this is a
// dedicated method rather than a Session field.
func (s *Store) SetGithubRepo(ctx context.Context, sessionID uuid.UUID, repo string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE billing.inference_sessions SET github_repo = $2 WHERE id = $1
	`, sessionID, repo)
	if err != nil {
		return fmt.Errorf("failed to record github repo: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// SetLastDeployStatus records the outcome of a session's most recent
// build/deploy attempt (see migration 030) — errMsg empty means the
// attempt SUCCEEDED, clearing any earlier failure so a stale "Failed"
// never lingers once the customer has since shipped successfully.
// Best-effort by design at every call site (buildKumbhaImage/
// DeployKumbhaSession/redeployKumbhaInstance): a failure to persist THIS
// bookkeeping must never mask or replace the real build/deploy error
// already being returned to the customer.
func (s *Store) SetLastDeployStatus(ctx context.Context, sessionID uuid.UUID, errMsg string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE billing.inference_sessions
		SET last_deploy_failed = $2, last_deploy_error = $3, last_deploy_at = now()
		WHERE id = $1
	`, sessionID, errMsg != "", nullIfEmpty(errMsg))
	if err != nil {
		return fmt.Errorf("failed to record last deploy status: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// SaveScreenshot stores the most recent capture of a session's deployed
// app — overwritten on every successful capture, no history kept (see
// migration 029's own doc comment: one row per session, not a version
// table). Written only by the screenshot pod's own upload call,
// authenticated by a session-scoped token exactly like
// MintWorkspaceFetchToken's fetch token — never customer-supplied.
func (s *Store) SaveScreenshot(ctx context.Context, sessionID uuid.UUID, png []byte) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE billing.inference_sessions
		SET screenshot = $2, screenshot_captured_at = now()
		WHERE id = $1
	`, sessionID, png)
	if err != nil {
		return fmt.Errorf("failed to save screenshot: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// GetScreenshot returns a session's most recently captured screenshot,
// scoped to accountID exactly like GetSession — this is read by the
// customer-facing console. A nil slice (with no error) means no capture
// has ever succeeded yet: a session that hasn't deployed, or whose
// capture pod hasn't finished, is a normal state, not a failure.
func (s *Store) GetScreenshot(ctx context.Context, sessionID, accountID uuid.UUID) ([]byte, time.Time, error) {
	var png []byte
	var capturedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT screenshot, screenshot_captured_at
		FROM billing.inference_sessions
		WHERE id = $1 AND account_id = $2
	`, sessionID, accountID).Scan(&png, &capturedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, ErrSessionNotFound
	}
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to load screenshot: %w", err)
	}
	return png, capturedAt.Time, nil
}

func (s *Store) RouteUsage(ctx context.Context, sessionID uuid.UUID) ([]RouteUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT route, provider, input_tokens, output_tokens
		FROM billing.inference_session_usage
		WHERE session_id = $1
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load route usage: %w", err)
	}
	defer rows.Close()

	var usage []RouteUsage
	for rows.Next() {
		var u RouteUsage
		if err := rows.Scan(&u.Route, &u.Provider, &u.InputTokens, &u.OutputTokens); err != nil {
			return nil, fmt.Errorf("failed to scan route usage: %w", err)
		}
		usage = append(usage, u)
	}
	return usage, rows.Err()
}

// Close marks an open session terminal. reason becomes the recorded
// status ("closed", "budget_exhausted", "idle_timeout") — distinct values
// so an operator reading the table can tell how a session ended without
// cross-referencing anything else.
func (s *Store) Close(ctx context.Context, id, accountID uuid.UUID, reason string) (*Session, error) {
	var sess Session
	var label, agentInstanceID, appInstanceID sql.NullString
	var endedAt time.Time
	err := s.db.QueryRowContext(ctx, `
		UPDATE billing.inference_sessions
		SET status = $3, ended_at = NOW()
		WHERE id = $1 AND account_id = $2 AND status = 'open'
		RETURNING id, account_id, project_id, budget, spent, status, label,
		          agent_instance_id, app_instance_id, deploy_approved, started_at, ended_at
	`, id, accountID, reason).Scan(&sess.ID, &sess.AccountID, &sess.ProjectID,
		&sess.Budget, &sess.Spent, &sess.Status, &label, &agentInstanceID, &appInstanceID,
		&sess.DeployApproved, &sess.StartedAt, &endedAt)
	if err == sql.ErrNoRows {
		// Already closed, or does not belong to this account — same
		// indistinguishable-existence reasoning as Get.
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to close session: %w", err)
	}
	sess.Label = label.String
	sess.AgentInstanceID = agentInstanceID.String
	sess.AppInstanceID = appInstanceID.String
	sess.EndedAt = &endedAt
	return &sess, nil
}

// ListByProject returns a project's Kumbha sessions, most recent first —
// the history view (KUMBHA-DESIGN.md has no console page of its own for
// Kumbha; this is what lets a customer find a build they started earlier
// instead of only ever reaching one via a URL they happened to keep).
// Read-only: this is a list of past/current sessions, not a way to
// resume one — see build/[id]/page.tsx's own doc comment on why
// continuing a finished session's conversation is separate, unbuilt work.
func (s *Store) ListByProject(ctx context.Context, accountID, projectID uuid.UUID) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_id, project_id, budget, spent, status, label,
		       agent_instance_id, app_instance_id, deploy_approved, started_at, ended_at,
		       last_deploy_failed, last_deploy_error, last_deploy_at
		FROM billing.inference_sessions
		WHERE account_id = $1 AND project_id = $2
		ORDER BY started_at DESC
	`, accountID, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []*Session
	for rows.Next() {
		var sess Session
		var label, agentInstanceID, appInstanceID, lastDeployError sql.NullString
		var endedAt, lastDeployAt sql.NullTime
		if err := rows.Scan(&sess.ID, &sess.AccountID, &sess.ProjectID, &sess.Budget,
			&sess.Spent, &sess.Status, &label, &agentInstanceID, &appInstanceID, &sess.DeployApproved,
			&sess.StartedAt, &endedAt, &sess.LastDeployFailed, &lastDeployError, &lastDeployAt); err != nil {
			return nil, fmt.Errorf("failed to scan session: %w", err)
		}
		sess.Label = label.String
		sess.AgentInstanceID = agentInstanceID.String
		sess.AppInstanceID = appInstanceID.String
		sess.LastDeployError = lastDeployError.String
		if lastDeployAt.Valid {
			sess.LastDeployAt = &lastDeployAt.Time
		}
		if endedAt.Valid {
			sess.EndedAt = &endedAt.Time
		}
		sessions = append(sessions, &sess)
	}
	return sessions, rows.Err()
}

// Delete removes the given sessions' history — the console's bulk-delete
// on the "Previous builds" list. Scoped by account (an id belonging to
// another customer is silently ignored, same existence-must-not-leak
// posture as everywhere else) and restricted to sessions NOT currently
// open: an active build has a live agent pod whose session-scoped
// credential resolves through this exact row (see auth.Middleware's
// session_id check) — deleting it out from under a running agent would
// turn its very next request into a confusing 401 instead of a clean
// close. Best-effort, not all-or-nothing: returns whichever of the
// requested ids were actually deleted, so one still-open session in a
// larger batch does not block the rest from being cleaned up.
//
// Safe to cascade: billing.kumbha_workspace_versions and
// billing.kumbha_messages both reference inference_sessions with ON
// DELETE CASCADE (migrations 025/027) and hold nothing but this session's
// own source/chat history. billing.usage_records — the actual invoiced
// ledger — does NOT reference inference_sessions at all (subject_id is a
// polymorphic string column, migration 024); a deleted build's spend
// stays on the customer's invoice exactly as it already was.
func (s *Store) Delete(ctx context.Context, accountID uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		DELETE FROM billing.inference_sessions
		WHERE account_id = $1 AND id = ANY($2) AND status != 'open'
		RETURNING id
	`, accountID, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("failed to delete sessions: %w", err)
	}
	defer rows.Close()

	var deleted []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan deleted session id: %w", err)
		}
		deleted = append(deleted, id)
	}
	return deleted, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
