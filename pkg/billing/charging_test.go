// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// fakeGateway is a test double for StripeGateway. It counts charge calls so
// idempotency can be asserted, and lets a test choose the returned status or
// force an error to exercise the decline path.
type fakeGateway struct {
	chargeCalls int
	lastAmount  int64
	retStatus   string // status returned on success (default "succeeded")
	retErr      error  // if set, CreatePaymentIntent returns this error
}

func (f *fakeGateway) EnsureCustomer(existingID, email, name, accountNumber string) (string, error) {
	return "cus_test", nil
}
func (f *fakeGateway) CreateSetupIntent(customerID, currency string) (string, string, error) {
	return "seti_secret", "seti_test", nil
}
func (f *fakeGateway) GetPaymentMethod(pmID string) (*CardSummary, error) {
	return &CardSummary{Brand: "visa", Last4: "4242", PaymentMethodID: pmID}, nil
}
func (f *fakeGateway) DetachPaymentMethod(pmID string) error { return nil }
func (f *fakeGateway) CreatePaymentIntent(customerID, pmID, currency string, amountCents int64, invoiceID, idempotencyKey string) (string, string, error) {
	f.chargeCalls++
	f.lastAmount = amountCents
	if f.retErr != nil {
		return "", "", f.retErr
	}
	status := f.retStatus
	if status == "" {
		status = "succeeded"
	}
	return "pi_test", status, nil
}

// invoiceLoadRows builds the row the ChargeInvoice loader expects.
func invoiceLoadRows(status, source string, settled interface{}, accountID uuid.UUID, total float64) *sqlmock.Rows {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC)
	return sqlmock.NewRows([]string{
		"status", "source", "stripe_invoice_id", "account_id", "total",
		"currency", "period_start", "period_end", "invoice_number",
	}).AddRow(status, source, settled, accountID, total, "USD",
		start, end, "INV-000042")
}

// errNoVerifiedCardSQL is what the card-resolution query returns when the
// account has no verified card — defaultChargeCard maps sql.ErrNoRows to
// errNoVerifiedCard.
func errNoVerifiedCardSQL() error { return sql.ErrNoRows }

// A fully credited (or sub-minimum) invoice settles with NO gateway call.
func TestChargeInvoice_FullyCreditedSettlesWithoutGateway(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	gw := &fakeGateway{}
	s := NewService(db).WithStripe(gw)
	acct := uuid.New()
	inv := uuid.New()

	// Load: $10 total, open usage, not settled.
	mock.ExpectQuery(`SELECT status, source, stripe_invoice_id`).
		WithArgs(inv).
		WillReturnRows(invoiceLoadRows("open", "usage", nil, acct, 10.0))
	// Credit applied for the period == $10 (fully covered).
	mock.ExpectQuery(`SELECT COALESCE\(-SUM\(ct\.amount\), 0\)`).
		WithArgs(acct, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"applied"}).AddRow(10.0))
	// MarkInvoicePaid: tx, update RETURNING account_id, other-unpaid check, clear clock, commit.
	mock.ExpectBegin()
	mock.ExpectQuery(`UPDATE billing\.invoices\s+SET status = 'paid'`).
		WithArgs("credit", inv).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(acct))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM billing\.invoices`).
		WithArgs(acct, inv).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`UPDATE auth\.accounts SET payment_failed_at = NULL`).
		WithArgs(acct).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := s.ChargeInvoice(context.Background(), inv); err != nil {
		t.Fatalf("ChargeInvoice: %v", err)
	}
	if gw.chargeCalls != 0 {
		t.Errorf("gateway was called %d times; a fully-credited invoice must not charge a card", gw.chargeCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Net-of-credits: $10 total, $4 credit ⇒ charges $6 (600 cents), succeeds.
func TestChargeInvoice_NetOfCreditsCharged(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	gw := &fakeGateway{retStatus: "succeeded"}
	s := NewService(db).WithStripe(gw)
	acct := uuid.New()
	inv := uuid.New()

	mock.ExpectQuery(`SELECT status, source, stripe_invoice_id`).
		WithArgs(inv).
		WillReturnRows(invoiceLoadRows("open", "usage", nil, acct, 10.0))
	mock.ExpectQuery(`SELECT COALESCE\(-SUM\(ct\.amount\), 0\)`).
		WithArgs(acct, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"applied"}).AddRow(4.0))
	// Resolve card.
	mock.ExpectQuery(`SELECT stripe_customer_id, stripe_payment_method_id`).
		WithArgs(acct).
		WillReturnRows(sqlmock.NewRows([]string{"stripe_customer_id", "stripe_payment_method_id"}).
			AddRow("cus_test", "pm_test"))
	// Pre-bump attempt.
	mock.ExpectExec(`UPDATE billing\.invoices\s+SET charge_attempts = charge_attempts \+ 1`).
		WithArgs(inv).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Record the pi id.
	mock.ExpectExec(`UPDATE billing\.invoices SET stripe_payment_intent_id`).
		WithArgs("pi_test", inv).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Optimistic settle via MarkInvoicePaid.
	mock.ExpectBegin()
	mock.ExpectQuery(`UPDATE billing\.invoices\s+SET status = 'paid'`).
		WithArgs("pi_test", inv).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(acct))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM billing\.invoices`).
		WithArgs(acct, inv).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`UPDATE auth\.accounts SET payment_failed_at = NULL`).
		WithArgs(acct).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := s.ChargeInvoice(context.Background(), inv); err != nil {
		t.Fatalf("ChargeInvoice: %v", err)
	}
	if gw.chargeCalls != 1 {
		t.Fatalf("gateway called %d times, want 1", gw.chargeCalls)
	}
	if gw.lastAmount != 600 {
		t.Errorf("charged %d cents, want 600 ($10 - $4 credit)", gw.lastAmount)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An already-settled invoice (stripe_invoice_id present) is a no-op — the
// gateway is never called. Covers webhook-replay / racing-sweep idempotency.
func TestChargeInvoice_AlreadySettledIsNoOp(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	gw := &fakeGateway{}
	s := NewService(db).WithStripe(gw)
	acct := uuid.New()
	inv := uuid.New()

	mock.ExpectQuery(`SELECT status, source, stripe_invoice_id`).
		WithArgs(inv).
		WillReturnRows(invoiceLoadRows("paid", "usage", "pi_prev", acct, 10.0))

	if err := s.ChargeInvoice(context.Background(), inv); err != nil {
		t.Fatalf("ChargeInvoice: %v", err)
	}
	if gw.chargeCalls != 0 {
		t.Errorf("gateway called on an already-settled invoice")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A manual invoice is never auto-charged: ChargeInvoice refuses it.
func TestChargeInvoice_ManualNotChargeable(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	gw := &fakeGateway{}
	s := NewService(db).WithStripe(gw)
	acct := uuid.New()
	inv := uuid.New()

	mock.ExpectQuery(`SELECT status, source, stripe_invoice_id`).
		WithArgs(inv).
		WillReturnRows(invoiceLoadRows("open", "manual", nil, acct, 10.0))

	if err := s.ChargeInvoice(context.Background(), inv); err == nil {
		t.Fatalf("ChargeInvoice accepted a manual invoice; want refusal")
	}
	if gw.chargeCalls != 0 {
		t.Errorf("gateway called on a manual invoice")
	}
}

// No verified card: the attempt is recorded and, when it is the LAST attempt
// (attempt 3 of 3), the grace clock is armed.
func TestChargeInvoice_NoCardArmsClockOnLastAttempt(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	gw := &fakeGateway{}
	s := NewService(db).WithStripe(gw)
	acct := uuid.New()
	inv := uuid.New()

	mock.ExpectQuery(`SELECT status, source, stripe_invoice_id`).
		WithArgs(inv).
		WillReturnRows(invoiceLoadRows("open", "usage", nil, acct, 10.0))
	mock.ExpectQuery(`SELECT COALESCE\(-SUM\(ct\.amount\), 0\)`).
		WithArgs(acct, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"applied"}).AddRow(0.0))
	// No verified card.
	mock.ExpectQuery(`SELECT stripe_customer_id, stripe_payment_method_id`).
		WithArgs(acct).
		WillReturnError(errNoVerifiedCardSQL())
	// recordChargeFailure with bump=true; RETURNING attempts=3 (== cap).
	mock.ExpectBegin()
	mock.ExpectQuery(`UPDATE billing\.invoices\s+SET last_charge_error`).
		WithArgs("no verified payment method on file", true, inv).
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "charge_attempts"}).AddRow(acct, 3))
	// Exhausted ⇒ arm the grace clock.
	mock.ExpectExec(`UPDATE auth\.accounts\s+SET payment_failed_at = COALESCE`).
		WithArgs(acct).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := s.ChargeInvoice(context.Background(), inv); err != nil {
		t.Fatalf("ChargeInvoice: %v", err)
	}
	if gw.chargeCalls != 0 {
		t.Errorf("gateway called despite no card")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Attempts 1 and 2 must NOT arm the clock — only the exhausting attempt does.
func TestChargeInvoice_EarlyAttemptDoesNotArmClock(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	gw := &fakeGateway{}
	s := NewService(db).WithStripe(gw)
	acct := uuid.New()
	inv := uuid.New()

	mock.ExpectQuery(`SELECT status, source, stripe_invoice_id`).
		WithArgs(inv).
		WillReturnRows(invoiceLoadRows("open", "usage", nil, acct, 10.0))
	mock.ExpectQuery(`SELECT COALESCE\(-SUM\(ct\.amount\), 0\)`).
		WithArgs(acct, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"applied"}).AddRow(0.0))
	mock.ExpectQuery(`SELECT stripe_customer_id, stripe_payment_method_id`).
		WithArgs(acct).
		WillReturnError(errNoVerifiedCardSQL())
	mock.ExpectBegin()
	// RETURNING attempts=1 (below cap) — clock must NOT be armed, so no
	// auth.accounts UPDATE is expected before commit.
	mock.ExpectQuery(`UPDATE billing\.invoices\s+SET last_charge_error`).
		WithArgs("no verified payment method on file", true, inv).
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "charge_attempts"}).AddRow(acct, 1))
	mock.ExpectCommit()

	if err := s.ChargeInvoice(context.Background(), inv); err != nil {
		t.Fatalf("ChargeInvoice: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (an early attempt armed the clock?): %v", err)
	}
}

// SettleInvoiceByPaymentIntent flips open→paid; a replay on a paid invoice
// is a no-op (no MarkInvoicePaid write).
func TestSettleInvoiceByPaymentIntent_ReplayNoOp(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := NewService(db)
	inv := uuid.New()

	mock.ExpectQuery(`SELECT id, status FROM billing\.invoices WHERE stripe_payment_intent_id`).
		WithArgs("pi_test").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(inv, "paid"))

	if err := s.SettleInvoiceByPaymentIntent(context.Background(), "pi_test"); err != nil {
		t.Fatalf("SettleInvoiceByPaymentIntent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
