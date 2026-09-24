// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// MaxMessageBytes bounds one follow-up message — generous for a chat
// instruction, well short of "paste an entire file in" (that's what the
// IDE's Save is for).
const MaxMessageBytes = 8000

// MaxAttachmentsPerMessage bounds how many files/images one prompt or
// follow-up may carry — generous for "a screenshot and a spec sheet,"
// nowhere near what would make a single agent turn unreasonably heavy.
const MaxAttachmentsPerMessage = 10

// ErrEmptyMessage means a follow-up message with no content was sent —
// caught before it reaches the database, not worth a round trip.
var ErrEmptyMessage = errors.New("message content is empty")

// ErrMessageTooLong means a follow-up message exceeded MaxMessageBytes.
var ErrMessageTooLong = errors.New("message exceeds the maximum length")

// ErrTooManyAttachments means a message named more than MaxAttachmentsPerMessage.
var ErrTooManyAttachments = errors.New("too many attachments on one message")

// ErrInvalidAttachment means an attachment was missing a URL or named an
// unrecognized Type.
var ErrInvalidAttachment = errors.New("invalid attachment")

// AttachmentType is the two kinds of attachment run.py handles completely
// differently — see pkg/api/kumbha_attachments.go's own doc comment on
// why: an image becomes real ImageContent inside the message the model
// itself sees (the model looks at it directly); a file has no equivalent
// content-block concept in the agent SDK at all, so it is materialized
// into the agent's own workspace directory instead, for the agent to read
// with tools (file_editor, terminal) it already has.
type AttachmentType string

const (
	AttachmentImage AttachmentType = "image"
	AttachmentFile  AttachmentType = "file"
)

func validAttachmentType(t AttachmentType) bool {
	return t == AttachmentImage || t == AttachmentFile
}

// Attachment is one file or image on a prompt or follow-up message. URL is
// the signed, ABSOLUTE download link CreateKumbhaAttachment minted —
// reachable from outside Teepin's own network, which matters differently
// for each Type (an image's URL is fetched by an external LLM provider; a
// file's is fetched by run.py itself before it ever reaches a model).
type Attachment struct {
	URL      string         `json:"url"`
	Type     AttachmentType `json:"type"`
	Filename string         `json:"filename,omitempty"`
}

func validateAttachments(attachments []Attachment) error {
	if len(attachments) > MaxAttachmentsPerMessage {
		return fmt.Errorf("%w: %d, over the %d limit", ErrTooManyAttachments, len(attachments), MaxAttachmentsPerMessage)
	}
	for _, a := range attachments {
		if a.URL == "" || !validAttachmentType(a.Type) {
			return fmt.Errorf("%w: %+v", ErrInvalidAttachment, a)
		}
	}
	return nil
}

// Message is one customer follow-up, queued for the agent's own poll loop
// (run.py's wait_for_next_instruction) to pick up.
type Message struct {
	ID          int64
	SessionID   uuid.UUID
	Content     string
	Attachments []Attachment
	CreatedAt   time.Time
	DeliveredAt *time.Time
}

// SendMessage queues a follow-up message for an OPEN session — refuses a
// closed one for the same reason Accrue refuses spend on one: there is
// nothing left running to ever deliver it to.
func (s *Store) SendMessage(ctx context.Context, sessionID uuid.UUID, content string, attachments []Attachment) (*Message, error) {
	if content == "" {
		return nil, ErrEmptyMessage
	}
	if len(content) > MaxMessageBytes {
		return nil, fmt.Errorf("%w: %d bytes, over the %d-byte limit", ErrMessageTooLong, len(content), MaxMessageBytes)
	}
	if err := validateAttachments(attachments); err != nil {
		return nil, err
	}

	var attachmentsJSON []byte
	if len(attachments) > 0 {
		var err error
		attachmentsJSON, err = json.Marshal(attachments)
		if err != nil {
			return nil, fmt.Errorf("failed to encode attachments: %w", err)
		}
	}

	msg := &Message{SessionID: sessionID, Content: content, Attachments: attachments}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO billing.kumbha_messages (session_id, content, attachments)
		SELECT $1, $2, $3
		WHERE EXISTS (SELECT 1 FROM billing.inference_sessions WHERE id = $1 AND status = 'open')
		RETURNING id, created_at
	`, sessionID, content, attachmentsJSON).Scan(&msg.ID, &msg.CreatedAt)
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
		SELECT id, content, attachments, created_at FROM billing.kumbha_messages
		WHERE session_id = $1 AND delivered_at IS NULL
		ORDER BY id
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to poll messages: %w", err)
	}
	var messages []Message
	for rows.Next() {
		var m Message
		var attachmentsJSON []byte
		if err := rows.Scan(&m.ID, &m.Content, &attachmentsJSON, &m.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan message: %w", err)
		}
		if len(attachmentsJSON) > 0 {
			if err := json.Unmarshal(attachmentsJSON, &m.Attachments); err != nil {
				rows.Close()
				return nil, fmt.Errorf("failed to decode attachments for message %d: %w", m.ID, err)
			}
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
