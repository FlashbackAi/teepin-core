// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// fakeSuspender records which accounts had their resources torn down.
type fakeSuspender struct {
	called []uuid.UUID
}

func (f *fakeSuspender) SuspendAccountResources(_ context.Context, accountID uuid.UUID) (int, error) {
	f.called = append(f.called, accountID)
	return 2, nil
}

// An account past its 24h grace period is suspended and its resources
// torn down. The interval comparison lives in SQL, so the test asserts
// the sweep acts on whatever the query returns — the row IS the "past
// grace" account.
func TestSuspensionSweep_SuspendsDueAccount(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	sus := &fakeSuspender{}
	s := NewSuspensionSweeper(db, sus)
	account := uuid.New()

	// The due-accounts query returns one account past grace.
	mock.ExpectQuery(`SELECT id FROM auth\.accounts\s+WHERE status = 'active'\s+AND payment_failed_at IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(account))
	// suspendOne: flip status (1 row affected → proceed to teardown).
	mock.ExpectExec(`UPDATE auth\.accounts SET status = 'suspended'`).
		WithArgs(account).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(sus.called) != 1 || sus.called[0] != account {
		t.Errorf("resources not torn down for the due account: %v", sus.called)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// No due accounts → nothing suspended, no teardown.
func TestSuspensionSweep_NoDueAccounts(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	sus := &fakeSuspender{}
	s := NewSuspensionSweeper(db, sus)

	mock.ExpectQuery(`SELECT id FROM auth\.accounts`).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // empty

	if err := s.sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(sus.called) != 0 {
		t.Errorf("tore down resources with no due accounts: %v", sus.called)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// If the status flip affects zero rows (a concurrent reactivation), the
// account's resources are NOT torn down — we only suspend accounts we
// actually transitioned.
func TestSuspensionSweep_ConcurrentReactivationSkipsTeardown(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	sus := &fakeSuspender{}
	s := NewSuspensionSweeper(db, sus)
	account := uuid.New()

	mock.ExpectQuery(`SELECT id FROM auth\.accounts`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(account))
	// Zero rows affected: someone else changed the status first.
	mock.ExpectExec(`UPDATE auth\.accounts SET status = 'suspended'`).
		WithArgs(account).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := s.sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(sus.called) != 0 {
		t.Errorf("tore down resources despite no status transition: %v", sus.called)
	}
}
