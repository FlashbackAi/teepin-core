// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

func newMockStore(t *testing.T) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db), mock
}

func TestStore_Create_RejectsNonPositiveBudget(t *testing.T) {
	store, _ := newMockStore(t)
	_, err := store.Create(context.Background(), uuid.New(), uuid.New(), 0, "test")
	if err == nil {
		t.Error("budget 0 accepted, want error")
	}
}

func TestStore_Create_RejectsOverCapBudget(t *testing.T) {
	store, _ := newMockStore(t)
	_, err := store.Create(context.Background(), uuid.New(), uuid.New(), maxSessionBudget+0.01, "test")
	if err == nil {
		t.Error("over-cap budget accepted, want error")
	}
}

func TestStore_Create_InsertsAndReturnsSession(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID := uuid.New(), uuid.New()
	sessionID := uuid.New()
	now := time.Now()

	mock.ExpectQuery(`INSERT INTO billing\.inference_sessions`).
		WithArgs(accountID, projectID, 5.0, "build booking app").
		WillReturnRows(sqlmock.NewRows([]string{"id", "spent", "status", "started_at"}).
			AddRow(sessionID, 0.0, "open", now))

	sess, err := store.Create(context.Background(), accountID, projectID, 5.0, "build booking app")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.ID != sessionID || sess.Status != "open" || sess.Budget != 5.0 {
		t.Errorf("unexpected session: %+v", sess)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStore_Get_NotFoundIsIndistinguishableFromWrongAccount(t *testing.T) {
	store, mock := newMockStore(t)
	id, accountID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(id, accountID).
		WillReturnError(sql.ErrNoRows)

	_, err := store.Get(context.Background(), id, accountID)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("got %v, want ErrSessionNotFound", err)
	}
}

func TestStore_IsSessionOpen_TrueForOpenSession(t *testing.T) {
	store, mock := newMockStore(t)
	id, accountID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT status = 'open' FROM billing\.inference_sessions`).
		WithArgs(id, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(true))

	open, err := store.IsSessionOpen(context.Background(), id, accountID)
	if err != nil {
		t.Fatalf("IsSessionOpen: %v", err)
	}
	if !open {
		t.Error("got false, want true for an open session")
	}
}

func TestStore_IsSessionOpen_FalseForClosedSession(t *testing.T) {
	store, mock := newMockStore(t)
	id, accountID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT status = 'open' FROM billing\.inference_sessions`).
		WithArgs(id, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(false))

	open, err := store.IsSessionOpen(context.Background(), id, accountID)
	if err != nil {
		t.Fatalf("IsSessionOpen: %v", err)
	}
	if open {
		t.Error("got true, want false for a closed session")
	}
}

func TestStore_IsSessionOpen_FalseWhenNotFound(t *testing.T) {
	// Not found and wrong-account both hit this path (the query filters
	// on account_id) — the caller (auth.Middleware) must not be able to
	// distinguish them, same as every other tenancy check in this codebase.
	store, mock := newMockStore(t)
	id, accountID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT status = 'open' FROM billing\.inference_sessions`).
		WithArgs(id, accountID).
		WillReturnError(sql.ErrNoRows)

	open, err := store.IsSessionOpen(context.Background(), id, accountID)
	if err != nil {
		t.Fatalf("IsSessionOpen: %v", err)
	}
	if open {
		t.Error("got true, want false for a nonexistent/foreign session")
	}
}

func TestStore_Accrue_BlocksWhenBudgetWouldBeExceeded(t *testing.T) {
	store, mock := newMockStore(t)
	id, accountID := uuid.New(), uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, budget, spent FROM billing\.inference_sessions`).
		WithArgs(id, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "budget", "spent"}).
			AddRow("open", 1.0, 0.90))
	mock.ExpectRollback()

	_, err := store.Accrue(context.Background(), id, accountID, 0.20, "teepin/fast", "vllm", 1000, 500)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("got %v, want ErrBudgetExhausted", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStore_Accrue_RefusesWhenSessionNotOpen(t *testing.T) {
	store, mock := newMockStore(t)
	id, accountID := uuid.New(), uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, budget, spent FROM billing\.inference_sessions`).
		WithArgs(id, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "budget", "spent"}).
			AddRow("closed", 5.0, 1.0))
	mock.ExpectRollback()

	_, err := store.Accrue(context.Background(), id, accountID, 0.10, "teepin/fast", "vllm", 100, 50)
	if !errors.Is(err, ErrSessionClosed) {
		t.Errorf("got %v, want ErrSessionClosed", err)
	}
}

func TestStore_Accrue_CommitsSpendAndRouteUsageTogether(t *testing.T) {
	store, mock := newMockStore(t)
	id, accountID := uuid.New(), uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, budget, spent FROM billing\.inference_sessions`).
		WithArgs(id, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "budget", "spent"}).
			AddRow("open", 5.0, 1.0))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET spent`).
		WithArgs(id, 1.25).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO billing\.inference_session_usage`).
		WithArgs(id, "teepin/fast", "vllm", 1000, 500).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	newSpent, err := store.Accrue(context.Background(), id, accountID, 0.25, "teepin/fast", "vllm", 1000, 500)
	if err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	if newSpent != 1.25 {
		t.Errorf("newSpent = %.2f, want 1.25", newSpent)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStore_SetDeployApproved_Success(t *testing.T) {
	store, mock := newMockStore(t)
	id, accountID := uuid.New(), uuid.New()

	mock.ExpectExec(`UPDATE billing\.inference_sessions SET deploy_approved`).
		WithArgs(id, accountID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.SetDeployApproved(context.Background(), id, accountID); err != nil {
		t.Fatalf("SetDeployApproved: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStore_SetDeployApproved_NotFoundOrClosedIsErrSessionNotFound(t *testing.T) {
	store, mock := newMockStore(t)
	id, accountID := uuid.New(), uuid.New()

	mock.ExpectExec(`UPDATE billing\.inference_sessions SET deploy_approved`).
		WithArgs(id, accountID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := store.SetDeployApproved(context.Background(), id, accountID)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("got %v, want ErrSessionNotFound (covers not-found, wrong-account, and closed alike)", err)
	}
}

func TestStore_IncreaseBudget_Success(t *testing.T) {
	store, mock := newMockStore(t)
	id, accountID := uuid.New(), uuid.New()

	mock.ExpectExec(`UPDATE billing\.inference_sessions SET budget`).
		WithArgs(15.0, id, accountID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.IncreaseBudget(context.Background(), id, accountID, 15.0); err != nil {
		t.Fatalf("IncreaseBudget: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStore_IncreaseBudget_NotFoundOrNotHigherIsErrSessionNotFound(t *testing.T) {
	store, mock := newMockStore(t)
	id, accountID := uuid.New(), uuid.New()

	mock.ExpectExec(`UPDATE billing\.inference_sessions SET budget`).
		WithArgs(15.0, id, accountID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := store.IncreaseBudget(context.Background(), id, accountID, 15.0)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("got %v, want ErrSessionNotFound", err)
	}
}

func TestStore_IncreaseBudget_RejectsOverTheCap(t *testing.T) {
	store, _ := newMockStore(t)
	id, accountID := uuid.New(), uuid.New()

	err := store.IncreaseBudget(context.Background(), id, accountID, maxSessionBudget+1)
	if err == nil {
		t.Error("expected an error for a budget over maxSessionBudget, got nil")
	}
}

func TestStore_Close_OnlyClosesAnOpenSession(t *testing.T) {
	store, mock := newMockStore(t)
	id, accountID := uuid.New(), uuid.New()

	mock.ExpectQuery(`UPDATE billing\.inference_sessions`).
		WithArgs(id, accountID, "budget_exhausted").
		WillReturnError(sql.ErrNoRows)

	_, err := store.Close(context.Background(), id, accountID, "budget_exhausted")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("got %v, want ErrSessionNotFound (already closed or wrong account)", err)
	}
}

func TestStore_Delete_EmptyIDsIsNoQuery(t *testing.T) {
	store, mock := newMockStore(t)
	deleted, err := store.Delete(context.Background(), uuid.New(), nil)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(deleted) != 0 {
		t.Errorf("got %v, want no deleted ids", deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStore_Delete_ReturnsExactlyWhatWasDeleted(t *testing.T) {
	store, mock := newMockStore(t)
	accountID := uuid.New()
	closedID, openID := uuid.New(), uuid.New()

	// Requested both a closed and a still-open session; only the closed
	// one comes back — the open one is silently skipped (WHERE status !=
	// 'open' in the real query), not an error for the whole batch.
	mock.ExpectQuery(`DELETE FROM billing\.inference_sessions`).
		WithArgs(accountID, pq.Array([]uuid.UUID{closedID, openID})).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(closedID))

	deleted, err := store.Delete(context.Background(), accountID, []uuid.UUID{closedID, openID})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != closedID {
		t.Errorf("got %v, want only %v deleted", deleted, closedID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
