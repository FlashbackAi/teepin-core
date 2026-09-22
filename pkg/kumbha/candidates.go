// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrCandidateNotFound means the id does not exist in
// billing.kumbha_route_candidates.
var ErrCandidateNotFound = errors.New("route candidate not found")

// RouteCandidate is one backend configured to serve a route, ranked
// against any others registered for the same route_name by Priority
// (lower tried first). See migration 052's own doc comment for the full
// design rationale.
type RouteCandidate struct {
	ID              uuid.UUID
	RouteName       string
	Priority        int
	ProviderType    string // "vllm" | "anthropic"
	BaseURL         string
	Model           string
	ContextWindow   int
	SupportsTools   bool
	MaxOutputTokens int
	Enabled         bool
	HasSecret       bool
	UpdatedBy       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// CandidateInput is what a caller (the admin API) supplies to create or
// update a candidate's configuration — everything except identity/
// bookkeeping fields the store itself owns.
type CandidateInput struct {
	RouteName       string
	Priority        int
	ProviderType    string
	BaseURL         string
	Model           string
	ContextWindow   int
	SupportsTools   bool
	MaxOutputTokens int
	Enabled         bool
}

func (in CandidateInput) validate() error {
	if in.RouteName == "" {
		return fmt.Errorf("route_name is required")
	}
	if in.ProviderType != "vllm" && in.ProviderType != "anthropic" {
		return fmt.Errorf("provider_type must be %q or %q, got %q", "vllm", "anthropic", in.ProviderType)
	}
	if in.Model == "" {
		return fmt.Errorf("model is required")
	}
	if in.ProviderType == "vllm" && in.BaseURL == "" {
		return fmt.Errorf("base_url is required for a vllm candidate")
	}
	return nil
}

// CandidateStore is the DB-backed catalog of route candidates — the live,
// Control-Center-editable configuration DynamicRouter and the admin API
// read/write. The API key itself never passes through here; see
// SecretsClient.
type CandidateStore struct {
	db *sql.DB
}

func NewCandidateStore(db *sql.DB) *CandidateStore { return &CandidateStore{db: db} }

const selectCandidatesSQL = `
	SELECT id, route_name, priority, provider_type, base_url, model,
	       context_window, supports_tools, max_output_tokens, enabled,
	       has_secret, COALESCE(updated_by, ''), created_at, updated_at
	FROM billing.kumbha_route_candidates`

// ListByRoute returns every candidate registered for a route, priority
// ascending (lower tried first), enabled and disabled alike — callers that
// only want routable candidates filter on Enabled themselves, same
// convention as modelcatalog.Service.ListModels.
func (s *CandidateStore) ListByRoute(ctx context.Context, routeName string) ([]RouteCandidate, error) {
	rows, err := s.db.QueryContext(ctx, selectCandidatesSQL+` WHERE route_name = $1 ORDER BY priority ASC, created_at ASC`, routeName)
	if err != nil {
		return nil, fmt.Errorf("failed to list candidates for %q: %w", routeName, err)
	}
	defer rows.Close()
	return scanCandidates(rows)
}

// ListAll returns every candidate across every route, grouped by
// route_name, for the admin listing (Control Center shows every route's
// candidates at once) and the health monitor.
func (s *CandidateStore) ListAll(ctx context.Context) (map[string][]RouteCandidate, error) {
	rows, err := s.db.QueryContext(ctx, selectCandidatesSQL+` ORDER BY route_name ASC, priority ASC, created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("failed to list route candidates: %w", err)
	}
	defer rows.Close()
	all, err := scanCandidates(rows)
	if err != nil {
		return nil, err
	}
	out := map[string][]RouteCandidate{}
	for _, c := range all {
		out[c.RouteName] = append(out[c.RouteName], c)
	}
	return out, nil
}

// Get returns one candidate by id.
func (s *CandidateStore) Get(ctx context.Context, id uuid.UUID) (RouteCandidate, error) {
	row := s.db.QueryRowContext(ctx, selectCandidatesSQL+` WHERE id = $1`, id)
	c, err := scanCandidate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RouteCandidate{}, ErrCandidateNotFound
	}
	if err != nil {
		return RouteCandidate{}, fmt.Errorf("failed to get candidate %s: %w", id, err)
	}
	return c, nil
}

// Create registers a new candidate and returns it with its generated id
// and timestamps.
func (s *CandidateStore) Create(ctx context.Context, in CandidateInput, updatedBy string) (RouteCandidate, error) {
	if err := in.validate(); err != nil {
		return RouteCandidate{}, err
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO billing.kumbha_route_candidates
			(route_name, priority, provider_type, base_url, model,
			 context_window, supports_tools, max_output_tokens, enabled, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, route_name, priority, provider_type, base_url, model,
		          context_window, supports_tools, max_output_tokens, enabled,
		          has_secret, COALESCE(updated_by, ''), created_at, updated_at`,
		in.RouteName, in.Priority, in.ProviderType, in.BaseURL, in.Model,
		in.ContextWindow, in.SupportsTools, in.MaxOutputTokens, in.Enabled, updatedBy)
	c, err := scanCandidate(row)
	if err != nil {
		return RouteCandidate{}, fmt.Errorf("failed to create candidate for %q: %w", in.RouteName, err)
	}
	return c, nil
}

// Update replaces a candidate's configuration (not its secret — see
// SetHasSecret). RouteName is immutable once created (moving a candidate
// to a different route is a delete-then-create, so nothing silently
// re-parents a backend that's mid-flight).
func (s *CandidateStore) Update(ctx context.Context, id uuid.UUID, in CandidateInput, updatedBy string) (RouteCandidate, error) {
	if err := in.validate(); err != nil {
		return RouteCandidate{}, err
	}
	row := s.db.QueryRowContext(ctx, `
		UPDATE billing.kumbha_route_candidates
		SET priority = $1, provider_type = $2, base_url = $3, model = $4,
		    context_window = $5, supports_tools = $6, max_output_tokens = $7,
		    enabled = $8, updated_by = $9, updated_at = NOW()
		WHERE id = $10
		RETURNING id, route_name, priority, provider_type, base_url, model,
		          context_window, supports_tools, max_output_tokens, enabled,
		          has_secret, COALESCE(updated_by, ''), created_at, updated_at`,
		in.Priority, in.ProviderType, in.BaseURL, in.Model,
		in.ContextWindow, in.SupportsTools, in.MaxOutputTokens, in.Enabled, updatedBy, id)
	c, err := scanCandidate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RouteCandidate{}, ErrCandidateNotFound
	}
	if err != nil {
		return RouteCandidate{}, fmt.Errorf("failed to update candidate %s: %w", id, err)
	}
	return c, nil
}

// SetHasSecret flips the has_secret flag after a successful Secrets
// Manager write — the store never sees the secret value itself, only
// whether one has been set.
func (s *CandidateStore) SetHasSecret(ctx context.Context, id uuid.UUID, hasSecret bool, updatedBy string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE billing.kumbha_route_candidates
		SET has_secret = $1, updated_by = $2, updated_at = NOW()
		WHERE id = $3`, hasSecret, updatedBy, id)
	if err != nil {
		return fmt.Errorf("failed to update has_secret for candidate %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCandidateNotFound
	}
	return nil
}

// Delete removes a candidate entirely. The caller (the admin handler) is
// responsible for also removing its Secrets Manager secret, if any — this
// store only owns the catalog row.
func (s *CandidateStore) Delete(ctx context.Context, id uuid.UUID) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM billing.kumbha_route_candidates WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("failed to delete candidate %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCandidateNotFound
	}
	return nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanCandidate(r scannable) (RouteCandidate, error) {
	var c RouteCandidate
	if err := r.Scan(
		&c.ID, &c.RouteName, &c.Priority, &c.ProviderType, &c.BaseURL, &c.Model,
		&c.ContextWindow, &c.SupportsTools, &c.MaxOutputTokens, &c.Enabled,
		&c.HasSecret, &c.UpdatedBy, &c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return RouteCandidate{}, err
	}
	return c, nil
}

func scanCandidates(rows *sql.Rows) ([]RouteCandidate, error) {
	out := []RouteCandidate{}
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
