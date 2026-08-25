// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestSendMessage_RejectsEmpty(t *testing.T) {
	store, mock := newMockStore(t)
	_, err := store.SendMessage(context.Background(), uuid.New(), "")
	if !errors.Is(err, ErrEmptyMessage) {
		t.Errorf("got %v, want ErrEmptyMessage", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected database interaction: %v", err)
	}
}

func TestSendMessage_RejectsOverLength(t *testing.T) {
	store, mock := newMockStore(t)
	_, err := store.SendMessage(context.Background(), uuid.New(), strings.Repeat("a", MaxMessageBytes+1))
	if !errors.Is(err, ErrMessageTooLong) {
		t.Errorf("got %v, want ErrMessageTooLong", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected database interaction: %v", err)
	}
}

func TestSendMessage_Success(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()
	createdAt := time.Now()

	mock.ExpectQuery(`INSERT INTO billing\.kumbha_messages`).
		WithArgs(sessionID, "add a footer").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), createdAt))

	msg, err := store.SendMessage(context.Background(), sessionID, "add a footer")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if msg.ID != 1 || msg.Content != "add a footer" {
		t.Errorf("got %+v, want id=1 content=%q", msg, "add a footer")
	}
}

func TestSendMessage_ClosedOrMissingSessionIsErrSessionClosed(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	// The INSERT's own WHERE EXISTS(... status = 'open') means a closed or
	// nonexistent session returns zero rows, not an error — sql.ErrNoRows
	// from Scan is how that surfaces.
	mock.ExpectQuery(`INSERT INTO billing\.kumbha_messages`).
		WithArgs(sessionID, "hello").
		WillReturnError(sql.ErrNoRows)

	_, err := store.SendMessage(context.Background(), sessionID, "hello")
	if !errors.Is(err, ErrSessionClosed) {
		t.Errorf("got %v, want ErrSessionClosed", err)
	}
}

func TestPollMessages_ReturnsNothingWhenEmpty(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, content, created_at FROM billing\.kumbha_messages`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "created_at"}))
	mock.ExpectRollback()

	messages, err := store.PollMessages(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("PollMessages: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("got %d messages, want 0", len(messages))
	}
}

func TestPollMessages_ReturnsAndMarksDelivered(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()
	now := time.Now()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, content, created_at FROM billing\.kumbha_messages`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "created_at"}).
			AddRow(int64(1), "add a footer", now).
			AddRow(int64(2), "make the header blue", now))
	mock.ExpectExec(`UPDATE billing\.kumbha_messages SET delivered_at = NOW\(\)`).
		WithArgs(sessionID).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	messages, err := store.PollMessages(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("PollMessages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(messages))
	}
	if messages[0].Content != "add a footer" || messages[1].Content != "make the header blue" {
		t.Errorf("got %+v, want messages in insertion order", messages)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
