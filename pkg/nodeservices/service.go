// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nodeservices

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"
)

// ErrNotFound means the node_services row does not exist.
var ErrNotFound = errors.New("node service not found")

// Service manages node_services rows — the desired-state side of the
// mount/unmount primitive. Reconciling desired state into reality (starting
// the actual pod or native process) is a separate, not-yet-built concern
// that reads this table, the same relationship compute.instances has with
// its own reconciler.
type Service struct {
	db *sql.DB
}

// NewService constructs the node-services service.
func NewService(db *sql.DB) *Service { return &Service{db: db} }

// Mount records a desired mount — Control Center asking for `kind` to be
// running on `nodeID` with the given kind-specific config. Always inserts a
// new row rather than upserting onto an existing one: two mounts of the
// same kind on the same node (e.g. two different models) are legitimately
// different rows, and a caller wanting "replace the existing mount" should
// Unmount the old one first — conflating the two would silently orphan
// whatever the old row's mount was tracking.
func (s *Service) Mount(ctx context.Context, nodeID uuid.UUID, kind Kind, config json.RawMessage, createdBy string) (*NodeService, error) {
	if kind != KindInferenceModel && kind != KindAgentBinary {
		return nil, fmt.Errorf("invalid kind %q", kind)
	}
	if createdBy == "" {
		return nil, fmt.Errorf("created_by is required")
	}
	if config == nil {
		config = json.RawMessage(`{}`)
	}

	var ns NodeService
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO compute.node_services (node_id, kind, config, desired_state, observed_state, created_by)
		VALUES ($1, $2, $3, 'mounted', 'pending', $4)
		RETURNING id, node_id, kind, config, desired_state, observed_state,
		          observed_error, observed_at, created_by, created_at, updated_at
	`, nodeID, string(kind), []byte(config), createdBy).Scan(
		&ns.ID, &ns.NodeID, &ns.Kind, &ns.Config, &ns.DesiredState, &ns.ObservedState,
		&ns.ObservedError, &ns.ObservedAt, &ns.CreatedBy, &ns.CreatedAt, &ns.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to mount %s on node %s: %w", kind, nodeID, err)
	}
	log.Printf("node_services: mount requested — node=%s kind=%s id=%s by=%s", nodeID, kind, ns.ID, createdBy)
	return &ns, nil
}

// Unmount flips desired_state to 'unmounted'. The row is kept, never
// deleted — history, and an idempotent re-mount both want the row to still
// exist rather than being recreated from nothing. Whatever reconciles this
// kind is responsible for actually tearing down the underlying pod/process
// and reporting ObservedUnmounted back via ReportObserved.
func (s *Service) Unmount(ctx context.Context, id uuid.UUID) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE compute.node_services
		SET desired_state = 'unmounted', updated_at = NOW()
		WHERE id = $1
	`, id)
	if err != nil {
		return fmt.Errorf("failed to unmount %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	log.Printf("node_services: unmount requested — id=%s", id)
	return nil
}

// ReportObserved records what actually happened — called by whatever
// reconciles this kind (the agent's status stream for a Linux node's pod,
// teepin-modeld's own report for a native Mac process), never by Control
// Center directly. observedErr is cleared (stored NULL) whenever
// observed is not ObservedError, so a stale error message can never
// linger past the state that caused it.
func (s *Service) ReportObserved(ctx context.Context, id uuid.UUID, observed ObservedState, observedErr *string) error {
	if observed != ObservedPending && observed != ObservedMounted &&
		observed != ObservedUnmounted && observed != ObservedError {
		return fmt.Errorf("invalid observed state %q", observed)
	}
	if observed != ObservedError {
		observedErr = nil
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE compute.node_services
		SET observed_state = $1, observed_error = $2, observed_at = NOW(), updated_at = NOW()
		WHERE id = $3
	`, string(observed), observedErr, id)
	if err != nil {
		return fmt.Errorf("failed to report observed state for %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Get returns one node_services row.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*NodeService, error) {
	ns, err := scanOne(s.db.QueryRowContext(ctx, selectSQL+` WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get node service %s: %w", id, err)
	}
	return ns, nil
}

// ListForNode returns every service row (mounted or unmounted) for one
// node — what Control Center's per-node view shows.
func (s *Service) ListForNode(ctx context.Context, nodeID uuid.UUID) ([]NodeService, error) {
	return s.list(ctx, selectSQL+` WHERE node_id = $1 ORDER BY created_at`, nodeID)
}

// ListByKind returns every row of one kind across all nodes — what the
// inference router uses to find every node currently (or once) serving a
// given kind, and what an agent-update rollout would use to find every
// node still on an old version.
func (s *Service) ListByKind(ctx context.Context, kind Kind) ([]NodeService, error) {
	return s.list(ctx, selectSQL+` WHERE kind = $1 ORDER BY created_at`, string(kind))
}

func (s *Service) list(ctx context.Context, query string, args ...any) ([]NodeService, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list node services: %w", err)
	}
	defer rows.Close()

	var out []NodeService
	for rows.Next() {
		ns, err := scanOne(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan node service: %w", err)
		}
		out = append(out, *ns)
	}
	return out, rows.Err()
}

const selectSQL = `
	SELECT id, node_id, kind, config, desired_state, observed_state,
	       observed_error, observed_at, created_by, created_at, updated_at
	FROM compute.node_services`

// row is satisfied by both *sql.Row and *sql.Rows.
type row interface {
	Scan(dest ...any) error
}

func scanOne(r row) (*NodeService, error) {
	var ns NodeService
	var kind, desired, observed string
	var config []byte
	if err := r.Scan(
		&ns.ID, &ns.NodeID, &kind, &config, &desired, &observed,
		&ns.ObservedError, &ns.ObservedAt, &ns.CreatedBy, &ns.CreatedAt, &ns.UpdatedAt,
	); err != nil {
		return nil, err
	}
	ns.Kind = Kind(kind)
	ns.DesiredState = DesiredState(desired)
	ns.ObservedState = ObservedState(observed)
	ns.Config = json.RawMessage(config)
	return &ns, nil
}
