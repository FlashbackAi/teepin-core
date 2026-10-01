// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/google/uuid"
)

// The agent's activity feed used to live only in its pod's log, which is gone
// when the pod is. Each sanitized event line is now also kept in
// billing.kumbha_events (migration 068), so a finished or crashed build still
// shows what happened. The rows are the same fields the console already reads
// (see eventFieldAllowlist) and nothing more.

const (
	// MaxStoredEventBytes bounds one stored event. A tool's output can be
	// large; the console shows a trimmed view anyway, and a runaway line must
	// not become a runaway row.
	MaxStoredEventBytes = 64 * 1024
	// MaxEventLinesPerLaunch bounds how much of one pod's log is kept. A
	// build that writes more than this is looping; the rest is dropped.
	MaxEventLinesPerLaunch = 20000
)

// capEventPayload returns payload unchanged if it fits, otherwise a copy with
// the bulky optional fields (diff, reasoning) dropped and the summary trimmed.
func capEventPayload(payload json.RawMessage) json.RawMessage {
	if len(payload) <= MaxStoredEventBytes {
		return payload
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil
	}
	delete(fields, "diff")
	delete(fields, "reasoning")
	if raw, ok := fields["summary"]; ok {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			budget := MaxStoredEventBytes / 2
			if len(text) > budget {
				cut := budget
				for cut > 0 && !utf8.RuneStart(text[cut]) {
					cut--
				}
				text = text[:cut] + " [trimmed]"
				fields["summary"], _ = json.Marshal(text)
			}
		}
	}
	out, err := json.Marshal(fields)
	if err != nil || len(out) > MaxStoredEventBytes {
		return nil
	}
	return out
}

// BumpLaunchSeq counts one more agent launch for the session and returns the
// new count. A relaunched pod reuses its name and starts a fresh log, so the
// launch number, not the name, says which log an event line belongs to.
func (s *Store) BumpLaunchSeq(ctx context.Context, sessionID uuid.UUID) (int, error) {
	var seq int
	err := s.db.QueryRowContext(ctx, `
		UPDATE billing.inference_sessions SET agent_launch_seq = agent_launch_seq + 1
		WHERE id = $1 RETURNING agent_launch_seq
	`, sessionID).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, ErrSessionNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("failed to count agent launch: %w", err)
	}
	return seq, nil
}

// AgentLaunchSeq returns how many times the session's agent has been launched.
func (s *Store) AgentLaunchSeq(ctx context.Context, sessionID uuid.UUID) (int, error) {
	var seq int
	err := s.db.QueryRowContext(ctx, `
		SELECT agent_launch_seq FROM billing.inference_sessions WHERE id = $1
	`, sessionID).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, ErrSessionNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("failed to read agent launch count: %w", err)
	}
	return seq, nil
}

// RecordEvent stores one event line. Recording the same (session, launch, line)
// again is a no-op, because a pod's log is replayed from the start whenever the
// tail reconnects.
func (s *Store) RecordEvent(ctx context.Context, sessionID uuid.UUID, launchSeq, lineNo int, payload json.RawMessage) error {
	if lineNo > MaxEventLinesPerLaunch {
		return nil
	}
	payload = capEventPayload(payload)
	if payload == nil {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO billing.kumbha_events (session_id, launch_seq, line_no, payload)
		VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (session_id, launch_seq, line_no) DO NOTHING
	`, sessionID, launchSeq, lineNo, []byte(payload)); err != nil {
		return fmt.Errorf("failed to record event: %w", err)
	}
	return nil
}

// MaxRecordedLine returns the highest line number stored for one launch (0 if
// none), so a restarted tail can skip what it already has.
func (s *Store) MaxRecordedLine(ctx context.Context, sessionID uuid.UUID, launchSeq int) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(line_no), 0) FROM billing.kumbha_events
		WHERE session_id = $1 AND launch_seq = $2
	`, sessionID, launchSeq).Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to read recorded events: %w", err)
	}
	return n, nil
}

// ListEvents returns the stored event payloads whose launch is between minSeq
// and maxSeq inclusive, oldest first.
func (s *Store) ListEvents(ctx context.Context, sessionID uuid.UUID, minSeq, maxSeq int) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload FROM billing.kumbha_events
		WHERE session_id = $1 AND launch_seq BETWEEN $2 AND $3
		ORDER BY id
	`, sessionID, minSeq, maxSeq)
	if err != nil {
		return nil, fmt.Errorf("failed to list events: %w", err)
	}
	defer rows.Close()

	var out []json.RawMessage
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan event: %w", err)
		}
		out = append(out, json.RawMessage(raw))
	}
	return out, rows.Err()
}
