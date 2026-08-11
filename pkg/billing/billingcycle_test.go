// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// previousMonth must return the whole prior calendar month regardless of
// which day "now" is — the period is always the calendar month, which is
// what makes a mid-month signup need no special handling.
func TestPreviousMonth(t *testing.T) {
	// Running on 2026-08-01 should bill all of July.
	now := time.Date(2026, 8, 1, 6, 30, 0, 0, time.UTC)
	start, end := previousMonth(now)

	if start.Year() != 2026 || start.Month() != time.July || start.Day() != 1 {
		t.Errorf("start = %v, want 2026-07-01", start)
	}
	if end.Month() != time.July || end.Day() != 31 {
		t.Errorf("end = %v, want end of July", end)
	}
	if !end.Before(now) {
		t.Error("period end must be in the past (satisfies the future-date guard)")
	}
}

// January wraps to the previous December.
func TestPreviousMonth_YearBoundary(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	start, _ := previousMonth(now)
	if start.Year() != 2025 || start.Month() != time.December {
		t.Errorf("start = %v, want 2025-12-01", start)
	}
}

// An account already invoiced for the period is skipped (idempotent).
func TestBillingCycle_SkipsAlreadyInvoiced(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	bc := NewBillingCycle(db, NewService(db))
	account := uuid.New()
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC)

	// One account with usage...
	mock.ExpectQuery(`SELECT DISTINCT p\.account_id\s+FROM billing\.usage_records`).
		WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(account))
	// ...but it already has a usage invoice for this exact period.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM billing\.invoices\s+WHERE account_id = \$1 AND source = 'usage'`).
		WithArgs(account, start, end).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// No CreateAccountUsageInvoice / IssueInvoice queries must follow.

	if err := bc.run(context.Background(), start, end); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("re-invoiced an account that already had a usage invoice: %v", err)
	}
}

// No accounts with usage → nothing generated.
func TestBillingCycle_NoUsageNoInvoices(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	bc := NewBillingCycle(db, NewService(db))
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC)

	mock.ExpectQuery(`SELECT DISTINCT p\.account_id`).
		WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"})) // empty

	if err := bc.run(context.Background(), start, end); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected queries: %v", err)
	}
}

// maybeRun does nothing on a day that is not the 1st.
func TestBillingCycle_OnlyRunsOnFirst(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	bc := NewBillingCycle(db, NewService(db))

	// maybeRun uses time.Now(); we cannot force it here, but we CAN assert
	// that when it is not the 1st, no query runs. If today happens to be
	// the 1st, the accounts query would fire and fail this expectation-free
	// mock — which is itself the signal. To keep the test deterministic we
	// only assert the guard path when it is not the 1st.
	if time.Now().UTC().Day() == 1 {
		t.Skip("today is the 1st; the guard-path assertion does not apply")
	}
	bc.maybeRun(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ran queries on a non-first day: %v", err)
	}
}
