// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package compute

import (
	"context"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ExecSessionRecord is one interactive-exec ticket's audit row (see
// migrations/022_exec_sessions). Created at ticket issue, before any
// attach is attempted, and updated once at session end.
type ExecSessionRecord struct {
	TicketID   string
	InstanceID string
	AccountID  uuid.UUID
	ProjectID  uuid.UUID
	UserID     uuid.UUID // uuid.Nil is valid: written as SQL NULL
	Container  string
	Command    []string
}

// CreateExecSession records a ticket at issue time — this is the audit
// trail for the most sensitive customer-facing action on the platform,
// so the row exists from the moment access was GRANTED, not just from a
// successful attach.
func (s *Store) CreateExecSession(ctx context.Context, rec ExecSessionRecord) error {
	var userID any
	if rec.UserID != uuid.Nil {
		userID = rec.UserID
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO compute.exec_sessions
		(ticket_id, instance_id, account_id, project_id, user_id, container, command)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, rec.TicketID, rec.InstanceID, rec.AccountID, rec.ProjectID, userID,
		rec.Container, pq.Array(rec.Command))
	return err
}

// EndExecSession updates the row keyed by ticket_id once the session
// ends, whatever the reason (clean exit, agent rejection, node offline,
// idle timeout, or simply abandoned before attaching). Called exactly
// once per ticket from pkg/cluster's ExecHandler via the
// WithSessionRecorder callback — pkg/cluster must not import this
// package directly, so the signature is duplicated there as a plain
// func type rather than shared.
func (s *Store) EndExecSession(ctx context.Context, ticketID, podName string, exitCode *int, closeReason string) error {
	var pod any
	if podName != "" {
		pod = podName
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE compute.exec_sessions
		SET pod_name = $2, exit_code = $3, close_reason = $4, ended_at = NOW()
		WHERE ticket_id = $1
	`, ticketID, pod, exitCode, closeReason)
	return err
}
