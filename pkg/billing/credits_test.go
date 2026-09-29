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

// consumeMock opens a mock DB and a Service for the ConsumeCredit tests.
func consumeMock(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewService(db), mock
}

// expectConsumeStart primes everything ConsumeCredit does before it draws:
// the idempotency check, the account lock, the balance, and the lots (in
// the order the query returns them, which is the draw order).
func expectConsumeStart(mock sqlmock.Sqlmock, account, usageID uuid.UUID, balance float64, lots [][2]any) {
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM billing\.credit_transactions WHERE usage_record_id`).
		WithArgs(usageID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`SELECT 1 FROM auth\.accounts WHERE id = \$1 FOR UPDATE`).
		WithArgs(account).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT billing\.credit_balance\(\$1\)`).
		WithArgs(account).
		WillReturnRows(sqlmock.NewRows([]string{"credit_balance"}).AddRow(balance))
	rows := sqlmock.NewRows([]string{"id", "remaining"})
	for _, l := range lots {
		rows.AddRow(l[0], l[1])
	}
	mock.ExpectQuery(`FROM billing\.credit_transactions l\s+WHERE l\.account_id = \$1\s+AND l\.kind IN \('grant', 'purchase'\).*ORDER BY \(l\.expires_at IS NULL\), l\.expires_at, l\.created_at, l\.id`).
		WithArgs(account).
		WillReturnRows(rows)
}

// A partial draw: $2 of credit against a $5 charge applies 2 and records
// which lot it came from; the remaining $3 is not collected (prepaid).
func TestConsumeCredit_PartialDraw(t *testing.T) {
	s, mock := consumeMock(t)
	account, usageID, lot := uuid.New(), uuid.New(), uuid.New()

	expectConsumeStart(mock, account, usageID, 2.0, [][2]any{{lot, 2.0}})
	mock.ExpectExec(`INSERT INTO billing\.credit_transactions.*'consumption'`).
		WithArgs(account, -2.0, usageID, lot).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	applied, err := s.ConsumeCredit(context.Background(), account, usageID, 5.0)
	if err != nil {
		t.Fatalf("ConsumeCredit: %v", err)
	}
	if applied != 2.0 {
		t.Errorf("applied = %.2f, want 2.00", applied)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// One charge larger than the first lot spans lots, in query order (soonest
// expiring first): a row per lot, each naming its lot, summing to the cost.
func TestConsumeCredit_SpansLotsInDrawOrder(t *testing.T) {
	s, mock := consumeMock(t)
	account, usageID := uuid.New(), uuid.New()
	expiring, forever := uuid.New(), uuid.New()

	expectConsumeStart(mock, account, usageID, 25.0, [][2]any{{expiring, 4.0}, {forever, 21.0}})
	mock.ExpectExec(`INSERT INTO billing\.credit_transactions.*'consumption'`).
		WithArgs(account, -4.0, usageID, expiring).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO billing\.credit_transactions.*'consumption'`).
		WithArgs(account, -6.0, usageID, forever).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	applied, err := s.ConsumeCredit(context.Background(), account, usageID, 10.0)
	if err != nil {
		t.Fatalf("ConsumeCredit: %v", err)
	}
	if applied != 10.0 {
		t.Errorf("applied = %.2f, want 10.00", applied)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A fully-spent lot is skipped, and the draw never exceeds the account's
// balance even if the lots would allow more (an unattributed legacy row
// could make the lots overstate it).
func TestConsumeCredit_SkipsEmptyLotsAndCapsAtBalance(t *testing.T) {
	s, mock := consumeMock(t)
	account, usageID := uuid.New(), uuid.New()
	empty, open := uuid.New(), uuid.New()

	expectConsumeStart(mock, account, usageID, 3.0, [][2]any{{empty, 0.0}, {open, 10.0}})
	mock.ExpectExec(`INSERT INTO billing\.credit_transactions.*'consumption'`).
		WithArgs(account, -3.0, usageID, open).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	applied, err := s.ConsumeCredit(context.Background(), account, usageID, 8.0)
	if err != nil {
		t.Fatalf("ConsumeCredit: %v", err)
	}
	if applied != 3.0 {
		t.Errorf("applied = %.2f, want 3.00 (the balance)", applied)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// No credit at all: nothing is written and nothing is applied.
func TestConsumeCredit_NoCreditWritesNothing(t *testing.T) {
	s, mock := consumeMock(t)
	account, usageID := uuid.New(), uuid.New()

	expectConsumeStart(mock, account, usageID, 0, nil)
	mock.ExpectCommit()

	applied, err := s.ConsumeCredit(context.Background(), account, usageID, 5.0)
	if err != nil || applied != 0 {
		t.Errorf("applied=%.2f err=%v, want 0/nil", applied, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
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

// The balance comes from the billing.credit_balance database function, the
// single source of truth shared with the provisioning gate and
// ConsumeCredit. The lot arithmetic itself (an expired grant forfeits only
// its unspent part) is proven against a real Postgres by the migration
// drill, migrations/credit_lots_drill_test.go.
func TestCreditBalance_ReadsTheDatabaseFunction(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := NewService(db)
	account := uuid.New()

	mock.ExpectQuery(`SELECT billing\.credit_balance\(\$1\)`).
		WithArgs(account).
		WillReturnRows(sqlmock.NewRows([]string{"credit_balance"}).AddRow(42.0))

	bal, err := s.CreditBalance(context.Background(), account)
	if err != nil {
		t.Fatalf("CreditBalance: %v", err)
	}
	if bal != 42.0 {
		t.Errorf("balance = %.2f, want 42.00", bal)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Forfeiture is one idempotent statement; the count of newly forfeited lots
// is reported.
func TestExpireCredits(t *testing.T) {
	s, mock := consumeMock(t)
	mock.ExpectExec(`INSERT INTO billing\.credit_transactions.*'expiry'.*ON CONFLICT \(lot_id\) WHERE kind = 'expiry' DO NOTHING`).
		WillReturnResult(sqlmock.NewResult(0, 2))

	n, err := s.ExpireCredits(context.Background())
	if err != nil || n != 2 {
		t.Errorf("ExpireCredits = %d, %v; want 2, nil", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
