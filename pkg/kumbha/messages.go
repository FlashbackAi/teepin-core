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

// MaxMessageBytes bounds one follow-up message — generous for a chat
// instruction, well short of "paste an entire file in" (that's what the
// IDE's Save is for).
const MaxMessageBytes = 8000

// ErrEmptyMessage means a follow-up message with no content was sent —
// caught before it reaches the database, not worth a round trip.
var ErrEmptyMessage = errors.New("message content is empty")

// ErrMessageTooLong means a follow-up message exceeded MaxMessageBytes.
var ErrMessageTooLong = errors.New("message exceeds the maximum length")

// Message is one customer follow-up, queued for the agent's own poll loop
// (run.py's wait_for_next_instruction) to pick up.
type Message struct {
	ID          int64
	SessionID   uuid.UUID
	Content     string
	CreatedAt   time.Time
	DeliveredAt *time.Time
}

// SendMessage queues a follow-up message for an OPEN session — refuses a
// closed one for the same reason Accrue refuses spend on one: there is
// nothing left running to ever deliver it to.
func (s *Store) SendMessage(ctx context.Context, sessionID uuid.UUID, content string) (*Message, error) {
	if content == "" {
		return nil, ErrEmptyMessage
	}
	if len(content) > MaxMessageBytes {
		return nil, fmt.Errorf("%w: %d bytes, over the %d-byte limit", ErrMessageTooLong, len(content), MaxMessageBytes)
	}

	msg := &Message{SessionID: sessionID, Content: content}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO billing.kumbha_messages (session_id, content)
		SELECT $1, $2
		WHERE EXISTS (SELECT 1 FROM billing.inference_sessions WHERE id = $1 AND status = 'open')
		RETURNING id, created_at
	`, sessionID, content).Scan(&msg.ID, &msg.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrSessionClosed
	}
	if err != nil {
		return nil, fmt.Errorf("failed to queue message: %w", err)
	}
	return msg, nil
}

// PollMessages returns every undelivered message for a session, oldest
// first, and marks them delivered in the SAME transaction — at-most-once
// delivery (see migration 027's own note on why re-delivery would be
// worse than the rare loss). Called by the agent pod's own poll loop, not
// a customer — no account scoping here, the session-scoped credential
// that reaches this is already scoped to exactly one session (see
// auth.GetSessionID).
func (s *Store) PollMessages(ctx context.Context, sessionID uuid.UUID) ([]Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	rows, err := tx.QueryContext(ctx, `
		SELECT id, content, created_at FROM billing.kumbha_messages
		WHERE session_id = $1 AND delivered_at IS NULL
		ORDER BY id
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to poll messages: %w", err)
	}
	var messages []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Content, &m.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan message: %w", err)
		}
		m.SessionID = sessionID
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("failed to read messages: %w", err)
	}
	rows.Close()

	if len(messages) == 0 {
		return nil, nil
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE billing.kumbha_messages SET delivered_at = NOW()
		WHERE session_id = $1 AND delivered_at IS NULL
	`, sessionID); err != nil {
		return nil, fmt.Errorf("failed to mark messages delivered: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit message poll: %w", err)
	}
	return messages, nil
}
