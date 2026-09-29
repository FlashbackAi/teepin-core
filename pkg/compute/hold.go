// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package compute

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrNoLaunchSpec means an instance has no stored launch spec, so it cannot
// be stopped-and-held (there would be no way to start it again).
var ErrNoLaunchSpec = errors.New("instance has no stored launch spec")

// SaveLaunchSpec stores the sealed launch spec for an instance, replacing any
// earlier one (a redeploy changes the image/env, and Start must relaunch the
// current version, not the first).
func (s *Store) SaveLaunchSpec(ctx context.Context, id string, sealed []byte) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE compute.instances SET launch_spec = $2 WHERE id = $1 AND terminated_at IS NULL
	`, id, sealed)
	if err != nil {
		return fmt.Errorf("failed to save launch spec for %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("failed to save launch spec for %s: no active instance", id)
	}
	return nil
}

// LoadLaunchSpec returns the sealed launch spec, or ErrNoLaunchSpec.
func (s *Store) LoadLaunchSpec(ctx context.Context, id string) ([]byte, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, `SELECT launch_spec FROM compute.instances WHERE id = $1`, id).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoLaunchSpec
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load launch spec for %s: %w", id, err)
	}
	if len(sealed) == 0 {
		return nil, ErrNoLaunchSpec
	}
	return sealed, nil
}

// MarkStopped records that the instance's pod was stopped with its disk kept.
// Billing for the running interval ends at stopped_at. Reports whether the
// row changed (false: it was not an active running/pending instance).
func (s *Store) MarkStopped(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE compute.instances
		SET status = $2, stopped_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND terminated_at IS NULL AND status IN ($3, $4)
	`, id, StatusStopped, StatusRunning, StatusPending)
	if err != nil {
		return false, fmt.Errorf("failed to mark %s stopped: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// StartResult is the endpoint picture the cluster reports for a relaunch.
type StartResult struct {
	PodName    string
	Endpoint   string
	DNSName    string
	PublicIP   string
	TLSEnabled bool
	TLSReady   bool
}

// MarkStarted records that a stopped instance was launched again. resumed_at
// is what billing resumes from, so the stopped gap is never charged as
// running time. Reports whether the row changed.
func (s *Store) MarkStarted(ctx context.Context, id string, r StartResult) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE compute.instances
		SET status = $2, stopped_at = NULL, resumed_at = NOW(), updated_at = NOW(),
		    k8s_pod_name = $3, endpoint = $4, dns_name = $5, public_ip = $6,
		    tls_enabled = $7, tls_ready = $8
		WHERE id = $1 AND terminated_at IS NULL AND status = $9
	`, id, StatusPending, r.PodName, r.Endpoint, r.DNSName, r.PublicIP, r.TLSEnabled, r.TLSReady, StatusStopped)
	if err != nil {
		return false, fmt.Errorf("failed to mark %s started: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AccountsWithStoppedInstances lists accounts that have any instance held in
// the stopped state - the accounts whose disks the storage hold must cover.
func (s *Store) AccountsWithStoppedInstances(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT account_id FROM compute.instances
		WHERE status = $1 AND terminated_at IS NULL
	`, StatusStopped)
	if err != nil {
		return nil, fmt.Errorf("failed to list accounts with stopped instances: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListStoppedByAccount returns an account's stopped instances.
func (s *Store) ListStoppedByAccount(ctx context.Context, accountID uuid.UUID) ([]InstanceRecord, error) {
	return s.query(ctx, "WHERE account_id = $1 AND status = $2 AND terminated_at IS NULL", accountID, StatusStopped)
}
