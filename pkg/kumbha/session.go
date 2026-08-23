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
	DeployApproved  bool
	StartedAt       time.Time
	EndedAt         *time.Time
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
	var label, agentInstanceID sql.NullString
	var endedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, account_id, project_id, budget, spent, status, label,
		       agent_instance_id, deploy_approved, started_at, ended_at
		FROM billing.inference_sessions
		WHERE id = $1 AND account_id = $2
	`, id, accountID).Scan(&sess.ID, &sess.AccountID, &sess.ProjectID, &sess.Budget,
		&sess.Spent, &sess.Status, &label, &agentInstanceID, &sess.DeployApproved,
		&sess.StartedAt, &endedAt)
	if err == sql.ErrNoRows {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load session: %w", err)
	}
	sess.Label = label.String
	sess.AgentInstanceID = agentInstanceID.String
	if endedAt.Valid {
		sess.EndedAt = &endedAt.Time
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
	var label, agentInstanceID sql.NullString
	var endedAt time.Time
	err := s.db.QueryRowContext(ctx, `
		UPDATE billing.inference_sessions
		SET status = $3, ended_at = NOW()
		WHERE id = $1 AND account_id = $2 AND status = 'open'
		RETURNING id, account_id, project_id, budget, spent, status, label,
		          agent_instance_id, deploy_approved, started_at, ended_at
	`, id, accountID, reason).Scan(&sess.ID, &sess.AccountID, &sess.ProjectID,
		&sess.Budget, &sess.Spent, &sess.Status, &label, &agentInstanceID,
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
	sess.EndedAt = &endedAt
	return &sess, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
