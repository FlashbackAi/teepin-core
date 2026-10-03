// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package teepinbuild

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// What the customer approves is a specific deployment plan, not "deploys, in
// general". Before this, one boolean on the session (deploy_approved) was
// flipped once and never reset, so approving a $1/hour plan also approved any
// larger one the agent presented later. Now:
//
//   - every plan the agent presents is recorded (billing.build_plans);
//   - approving names the plan, and the session remembers which one;
//   - a deploy may only use resources that plan covers.
//
// An update that fits inside the approved plan needs nothing new (the owner
// asked that changing an already-deployed app not go through approval again);
// asking for more than the plan allows needs a new plan and a new approval.

var (
	// ErrPlanNotFound means the plan does not exist or belongs to another
	// session.
	ErrPlanNotFound = errors.New("deployment plan not found")
	// ErrExceedsApprovedPlan means a deploy asks for more than the plan the
	// customer approved. Maps to 403 exceeds_approved_plan.
	ErrExceedsApprovedPlan = errors.New("this deploy asks for more CPU, memory or storage than the plan the customer approved: present a new plan with present_deployment_plan and wait for approval")
	// ErrInvalidPlan means a plan was refused for its content.
	ErrInvalidPlan = errors.New("invalid deployment plan")
	// ErrTooManyPlans means the session has presented more plans than any
	// real build needs.
	ErrTooManyPlans = errors.New("this session has presented too many deployment plans")
)

const (
	// MaxPlansPerSession bounds the rows one session can create here.
	MaxPlansPerSession = 100
	// MaxPlanResources bounds the lines in one plan.
	MaxPlanResources = 20
	// maxPlanUnits bounds any single figure, so a typo cannot be approved as
	// a deployment of a million cores.
	maxPlanUnits = 4096
)

// PlanResource is one line of a plan, as the agent presented it.
type PlanResource struct {
	Name      string `json:"name"`
	CPUUnits  int    `json:"cpu_units"`
	MemoryGB  int    `json:"memory_gb"`
	StorageGB int    `json:"storage_gb"`
}

func validatePlanResources(res []PlanResource) ([]PlanResource, error) {
	if len(res) == 0 {
		return nil, fmt.Errorf("%w: at least one resource is required", ErrInvalidPlan)
	}
	if len(res) > MaxPlanResources {
		return nil, fmt.Errorf("%w: at most %d resources", ErrInvalidPlan, MaxPlanResources)
	}
	out := make([]PlanResource, 0, len(res))
	for _, r := range res {
		r.Name = strings.TrimSpace(r.Name)
		if r.Name == "" || len(r.Name) > 80 {
			return nil, fmt.Errorf("%w: every resource needs a name of up to 80 characters", ErrInvalidPlan)
		}
		for _, v := range []int{r.CPUUnits, r.MemoryGB, r.StorageGB} {
			if v < 0 || v > maxPlanUnits {
				return nil, fmt.Errorf("%w: %q has a figure outside 0..%d", ErrInvalidPlan, r.Name, maxPlanUnits)
			}
		}
		if r.CPUUnits == 0 && r.MemoryGB == 0 && r.StorageGB == 0 {
			return nil, fmt.Errorf("%w: %q asks for nothing", ErrInvalidPlan, r.Name)
		}
		out = append(out, r)
	}
	return out, nil
}

// RecordPlan stores a plan the agent presented and returns its id. It is
// pending until the customer approves it by id.
func (s *Store) RecordPlan(ctx context.Context, sessionID, accountID uuid.UUID, resources []PlanResource) (uuid.UUID, error) {
	resources, err := validatePlanResources(resources)
	if err != nil {
		return uuid.Nil, err
	}
	encoded, err := json.Marshal(resources)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to encode plan: %w", err)
	}

	var status string
	var existing int
	err = s.db.QueryRowContext(ctx, `
		SELECT s.status, (SELECT COUNT(*) FROM billing.build_plans p WHERE p.session_id = s.id)
		FROM billing.inference_sessions s
		WHERE s.id = $1 AND s.account_id = $2
	`, sessionID, accountID).Scan(&status, &existing)
	if err == sql.ErrNoRows {
		return uuid.Nil, ErrSessionNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to check session before recording plan: %w", err)
	}
	if status != "open" {
		return uuid.Nil, ErrSessionClosed
	}
	if existing >= MaxPlansPerSession {
		return uuid.Nil, ErrTooManyPlans
	}

	var id uuid.UUID
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO billing.build_plans (session_id, resources) VALUES ($1, $2::jsonb) RETURNING id
	`, sessionID, encoded).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("failed to record plan: %w", err)
	}
	return id, nil
}

// ApprovePlan records the customer's approval. planID names the plan they
// were shown; uuid.Nil means "the newest plan", which keeps an older console
// (one that does not send an id) working. A session that has presented no plan
// at all (an agent image from before plans were recorded) is approved the old
// way, without a plan to bind to.
func (s *Store) ApprovePlan(ctx context.Context, sessionID, accountID, planID uuid.UUID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	var locked uuid.UUID
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM billing.inference_sessions
		WHERE id = $1 AND account_id = $2 AND status = 'open'
		FOR UPDATE
	`, sessionID, accountID).Scan(&locked)
	if err == sql.ErrNoRows {
		return ErrSessionNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to lock session: %w", err)
	}

	if planID == uuid.Nil {
		err = tx.QueryRowContext(ctx, `
			SELECT id FROM billing.build_plans WHERE session_id = $1 ORDER BY created_at DESC, id LIMIT 1
		`, sessionID).Scan(&planID)
		if err == sql.ErrNoRows {
			if _, err := tx.ExecContext(ctx, `
				UPDATE billing.inference_sessions SET deploy_approved = true WHERE id = $1
			`, sessionID); err != nil {
				return fmt.Errorf("failed to approve deploy: %w", err)
			}
			return tx.Commit()
		}
		if err != nil {
			return fmt.Errorf("failed to find the newest plan: %w", err)
		}
	} else {
		var owner uuid.UUID
		err = tx.QueryRowContext(ctx, `
			SELECT session_id FROM billing.build_plans WHERE id = $1
		`, planID).Scan(&owner)
		if err == sql.ErrNoRows || (err == nil && owner != sessionID) {
			return ErrPlanNotFound
		}
		if err != nil {
			return fmt.Errorf("failed to read plan: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE billing.build_plans SET approved_at = COALESCE(approved_at, NOW()) WHERE id = $1
	`, planID); err != nil {
		return fmt.Errorf("failed to mark plan approved: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing.inference_sessions SET deploy_approved = true, approved_plan_id = $2 WHERE id = $1
	`, sessionID, planID); err != nil {
		return fmt.Errorf("failed to approve deploy: %w", err)
	}
	return tx.Commit()
}

// CheckApprovedFor reports whether a deploy of this size is covered by the
// plan the customer approved. It is a nil no-op when no plan is bound to the
// session (never approved, or approved before plans were recorded): the
// deploy_approved flag, checked separately, still gates those.
func (s *Store) CheckApprovedFor(ctx context.Context, sessionID uuid.UUID, cpuUnits, memoryGB, storageGB int) error {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT p.resources
		FROM billing.inference_sessions s
		JOIN billing.build_plans p ON p.id = s.approved_plan_id
		WHERE s.id = $1
	`, sessionID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read the approved plan: %w", err)
	}
	var lines []PlanResource
	if err := json.Unmarshal(raw, &lines); err != nil {
		return fmt.Errorf("failed to decode the approved plan: %w", err)
	}
	if resourceFitsPlan(lines, cpuUnits, memoryGB, storageGB) {
		return nil
	}
	return ErrExceedsApprovedPlan
}

// resourceFitsPlan: one deployed resource must fit within at least one line
// of the plan on every figure.
func resourceFitsPlan(lines []PlanResource, cpuUnits, memoryGB, storageGB int) bool {
	for _, l := range lines {
		if cpuUnits <= l.CPUUnits && memoryGB <= l.MemoryGB && storageGB <= l.StorageGB {
			return true
		}
	}
	return false
}

// RecordPlan stores a plan the agent presented (see Store.RecordPlan).
func (g *Gateway) RecordPlan(ctx context.Context, sessionID, accountID uuid.UUID, resources []PlanResource) (uuid.UUID, error) {
	return g.store.RecordPlan(ctx, sessionID, accountID, resources)
}

// ApprovePlan approves one plan by id (uuid.Nil: the newest).
func (g *Gateway) ApprovePlan(ctx context.Context, sessionID, accountID, planID uuid.UUID) error {
	return g.store.ApprovePlan(ctx, sessionID, accountID, planID)
}

// CheckResourcesApproved refuses a deploy that asks for more than the approved
// plan covers (see Store.CheckApprovedFor).
func (g *Gateway) CheckResourcesApproved(ctx context.Context, sessionID uuid.UUID, cpuUnits, memoryGB, storageGB int) error {
	return g.store.CheckApprovedFor(ctx, sessionID, cpuUnits, memoryGB, storageGB)
}

// ApprovedPlanID returns the id of the plan the customer approved, or "" when
// none is bound.
func (s *Store) ApprovedPlanID(ctx context.Context, sessionID uuid.UUID) (string, error) {
	var id sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT approved_plan_id::text FROM billing.inference_sessions WHERE id = $1
	`, sessionID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", ErrSessionNotFound
	}
	if err != nil {
		return "", fmt.Errorf("failed to read the approved plan id: %w", err)
	}
	return id.String, nil
}

// ApprovedPlanID is Store.ApprovedPlanID.
func (g *Gateway) ApprovedPlanID(ctx context.Context, sessionID uuid.UUID) (string, error) {
	return g.store.ApprovedPlanID(ctx, sessionID)
}
