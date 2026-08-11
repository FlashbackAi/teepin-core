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

func TestGrantCredit_Validation(t *testing.T) {
	past := time.Now().AddDate(0, 0, -1)
	cases := []struct {
		name string
		req  GrantRequest
	}{
		{"blank reason", GrantRequest{AccountID: uuid.New(), Amount: 10, Reason: "  "}},
		{"zero amount", GrantRequest{AccountID: uuid.New(), Amount: 0, Reason: "x"}},
		{"negative amount", GrantRequest{AccountID: uuid.New(), Amount: -5, Reason: "x"}},
		{"over cap", GrantRequest{AccountID: uuid.New(), Amount: maxGrant + 1, Reason: "x"}},
		{"past expiry", GrantRequest{AccountID: uuid.New(), Amount: 10, Reason: "x", ExpiresAt: &past}},
	}
	// nil DB is fine: every case must be rejected BEFORE any query.
	s := NewService(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.GrantCredit(context.Background(), tc.req); err == nil {
				t.Errorf("%s was accepted, want a validation error", tc.name)
			}
		})
	}
}

func TestGrantCredit_ValidInserts(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := NewService(db)

	mock.ExpectExec(`INSERT INTO billing\.credit_transactions`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = s.GrantCredit(context.Background(), GrantRequest{
		AccountID: uuid.New(), Amount: 500, Reason: "design partner", GrantedBy: "operator",
	})
	if err != nil {
		t.Fatalf("GrantCredit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A partial draw: $2 of credit against a $5 charge applies 2 and leaves
// the remaining $3 to bill to the card.
func TestConsumeCredit_PartialDraw(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := NewService(db)
	account := uuid.New()
	usageID := uuid.New()

	mock.ExpectBegin()
	// Not previously consumed.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM billing\.credit_transactions WHERE usage_record_id`).
		WithArgs(usageID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`SELECT 1 FROM auth\.accounts WHERE id = \$1 FOR UPDATE`).
		WithArgs(account).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(amount\), 0\)\s+FROM billing\.credit_transactions`).
		WithArgs(account).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(2.0))
	// Applies exactly the available 2.00 (negative row).
	mock.ExpectExec(`INSERT INTO billing\.credit_transactions.*'consumption'`).
		WithArgs(account, -2.0, usageID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	applied, err := s.ConsumeCredit(context.Background(), account, usageID, 5.0)
	if err != nil {
		t.Fatalf("ConsumeCredit: %v", err)
	}
	if applied != 2.0 {
		t.Errorf("applied = %.2f, want 2.00", applied)
	}
}

// Re-processing the same usage record applies nothing — the ledger stays
// idempotent even if the collector re-runs an interval.
func TestConsumeCredit_IdempotentPerUsageRecord(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := NewService(db)
	account := uuid.New()
	usageID := uuid.New()

	mock.ExpectBegin()
	// Already consumed once.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM billing\.credit_transactions WHERE usage_record_id`).
		WithArgs(usageID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectRollback()

	applied, err := s.ConsumeCredit(context.Background(), account, usageID, 5.0)
	if err != nil {
		t.Fatalf("ConsumeCredit: %v", err)
	}
	if applied != 0 {
		t.Errorf("applied = %.2f on replay, want 0", applied)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Zero-cost consumption is a no-op that opens no transaction.
func TestConsumeCredit_ZeroCostNoOp(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := NewService(db)

	applied, err := s.ConsumeCredit(context.Background(), uuid.New(), uuid.New(), 0)
	if err != nil || applied != 0 {
		t.Errorf("zero-cost: applied=%.2f err=%v, want 0/nil", applied, err)
	}
}

// Balance excludes expired grants: the SQL predicate carries that, so the
// test pins that the query is shaped to exclude them (regex-matched).
func TestCreditBalance_ExcludesExpiredGrants(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := NewService(db)
	account := uuid.New()

	mock.ExpectQuery(`kind != 'grant' OR expires_at IS NULL OR expires_at > NOW\(\)`).
		WithArgs(account).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(42.0))

	bal, err := s.CreditBalance(context.Background(), account)
	if err != nil {
		t.Fatalf("CreditBalance: %v", err)
	}
	if bal != 42.0 {
		t.Errorf("balance = %.2f, want 42.00", bal)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("query not shaped to exclude expired grants: %v", err)
	}
}
