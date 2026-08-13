// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package compute persists instance state in PostgreSQL
// (compute.instances) and reconciles it with the live Kubernetes
// cluster. The database is the billing source of truth: the billing
// collector meters every row with status 'running', so instances MUST
// be written here on creation and marked terminated on deletion.
package compute

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Instance lifecycle statuses stored in compute.instances.
const (
	StatusPending    = "pending"
	StatusRunning    = "running"
	StatusFailed     = "failed"
	StatusTerminated = "terminated"
)

// InstanceRecord is a row of compute.instances.
type InstanceRecord struct {
	ID string
	// AccountID is the tenant that owns this instance. Denormalised
	// from the project so every tenancy check and billing aggregation
	// filters on it directly, without a join.
	AccountID    uuid.UUID
	ProjectID    uuid.UUID
	UserID       uuid.UUID
	Name         string
	Image        string
	InstanceType string // e.g. "gpu.h100.2g.20gb", derived from hardware
	Status       string
	GPUVRAMGB    int // 0 for CPU-only instances
	CPUUnits     int
	MemoryGB     int
	Endpoint     string
	K8sPodName   string
	K8sNamespace string
	// ProviderID / NodeName record which home provider+node ran this
	// instance. Empty for the datacenter path. ProviderID is how a later
	// delete/logs command routes to the same agent session; NodeName is
	// resolved to compute.nodes.id at insert time for load accounting.
	ProviderID string
	NodeName   string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	StartedAt    *time.Time
	TerminatedAt *time.Time
}

// Store provides CRUD access to compute.instances.
type Store struct {
	db *sql.DB
}

// NewStore creates an instance store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Create inserts a new instance record.
func (s *Store) Create(ctx context.Context, rec *InstanceRecord) error {
	// node_id is resolved from node_name via a sub-select so the API layer
	// never has to know the node's UUID. NULL for the datacenter path (no
	// node_name) or if the name does not match a row.
	query := `
		INSERT INTO compute.instances
		(id, account_id, project_id, user_id, name, image, instance_type_id, status,
		 gpu_vram_gb, cpu_units, memory_gb, endpoint, k8s_pod_name, k8s_namespace,
		 provider_id, node_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
		 (SELECT id FROM compute.nodes WHERE node_name = $16))
		RETURNING created_at, updated_at
	`

	var vram sql.NullInt64
	if rec.GPUVRAMGB > 0 {
		vram = sql.NullInt64{Int64: int64(rec.GPUVRAMGB), Valid: true}
	}

	err := s.db.QueryRowContext(ctx, query,
		rec.ID, rec.AccountID, rec.ProjectID, rec.UserID, rec.Name, rec.Image,
		rec.InstanceType, rec.Status, vram, rec.CPUUnits, rec.MemoryGB,
		nullIfEmpty(rec.Endpoint), nullIfEmpty(rec.K8sPodName), rec.K8sNamespace,
		nullIfEmpty(rec.ProviderID), nullIfEmpty(rec.NodeName),
	).Scan(&rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to persist instance %s: %w", rec.ID, err)
	}

	return nil
}

// UpdateStatus sets the instance status. When the status becomes
// running for the first time, started_at is stamped.
func (s *Store) UpdateStatus(ctx context.Context, id, status string) error {
	// $1 is cast explicitly: using the same placeholder for both the
	// assignment and the comparison makes PostgreSQL fail with
	// "inconsistent types deduced for parameter $1", so every status
	// update errored and started_at was never stamped.
	query := `
		UPDATE compute.instances
		SET status = $1::varchar,
		    started_at = CASE WHEN $1::varchar = 'running' AND started_at IS NULL
		                      THEN NOW() ELSE started_at END
		WHERE id = $2 AND terminated_at IS NULL
	`

	res, err := s.db.ExecContext(ctx, query, status, id)
	if err != nil {
		return fmt.Errorf("failed to update status of %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("instance %s not found or already terminated", id)
	}

	return nil
}

// MarkTerminated finalizes an instance: status becomes terminated and
// terminated_at is stamped, which stops billing collection for it.
// Idempotent: terminating an already-terminated instance is a no-op.
func (s *Store) MarkTerminated(ctx context.Context, id string) error {
	query := `
		UPDATE compute.instances
		SET status = $1, terminated_at = NOW()
		WHERE id = $2 AND terminated_at IS NULL
	`

	if _, err := s.db.ExecContext(ctx, query, StatusTerminated, id); err != nil {
		return fmt.Errorf("failed to mark %s terminated: %w", id, err)
	}

	return nil
}

// Get returns one instance by ID, or nil when it does not exist.
func (s *Store) Get(ctx context.Context, id string) (*InstanceRecord, error) {
	rows, err := s.query(ctx, "WHERE id = $1", id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ListActive returns every non-terminated instance (all projects) —
// used by the reconciler.
func (s *Store) ListActive(ctx context.Context) ([]InstanceRecord, error) {
	return s.query(ctx, "WHERE terminated_at IS NULL")
}

// ListActiveByAccount returns an account's non-terminated instances
// across all its projects — used by the suspension sweeper to tear down
// everything an account is running when its grace period elapses.
func (s *Store) ListActiveByAccount(ctx context.Context, accountID uuid.UUID) ([]InstanceRecord, error) {
	return s.query(ctx,
		"WHERE account_id = $1 AND terminated_at IS NULL ORDER BY created_at DESC",
		accountID)
}

// ListByProject returns the project's non-terminated instances.
// ListByProject returns an account's live instances in one project.
// Both predicates are required: project_id alone would let a caller
// read another account's instances by guessing a project UUID.
func (s *Store) ListByProject(ctx context.Context, accountID, projectID uuid.UUID) ([]InstanceRecord, error) {
	return s.query(ctx,
		"WHERE account_id = $1 AND project_id = $2 AND terminated_at IS NULL ORDER BY created_at DESC",
		accountID, projectID)
}

const selectColumns = `
	SELECT id, account_id, project_id, user_id, name, image,
	       COALESCE(instance_type_id, ''), status, COALESCE(gpu_vram_gb, 0),
	       cpu_units, memory_gb, COALESCE(endpoint, ''),
	       COALESCE(k8s_pod_name, ''), COALESCE(k8s_namespace, ''),
	       created_at, updated_at, started_at, terminated_at
	FROM compute.instances
`

func (s *Store) query(ctx context.Context, where string, args ...interface{}) ([]InstanceRecord, error) {
	rows, err := s.db.QueryContext(ctx, selectColumns+where, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query instances: %w", err)
	}
	defer rows.Close()

	var records []InstanceRecord
	for rows.Next() {
		var rec InstanceRecord
		if err := rows.Scan(
			&rec.ID, &rec.AccountID, &rec.ProjectID, &rec.UserID, &rec.Name, &rec.Image,
			&rec.InstanceType, &rec.Status, &rec.GPUVRAMGB,
			&rec.CPUUnits, &rec.MemoryGB, &rec.Endpoint,
			&rec.K8sPodName, &rec.K8sNamespace,
			&rec.CreatedAt, &rec.UpdatedAt, &rec.StartedAt, &rec.TerminatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan instance: %w", err)
		}
		records = append(records, rec)
	}

	return records, rows.Err()
}

func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
