// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newTopUpMock(t *testing.T) (*Service, sqlmock.Sqlmock, *fakeGateway) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	gw := &fakeGateway{}
	return NewService(db).WithStripe(gw), mock, gw
}

// The amount rules are the customer-facing contract of a top-up, so the
// boundaries are pinned exactly: whole cents only, inclusive of $20 and
// $1,000.
func TestTopUpAmountCents(t *testing.T) {
	cases := []struct {
		amount float64
		want   int64
		ok     bool
	}{
		{20, 2000, true},
		{1000, 100000, true},
		{25.5, 2550, true},
		{99.99, 9999, true},
		{19.99, 0, false},
		{1000.01, 0, false},
		{0, 0, false},
		{-20, 0, false},
		{20.005, 0, false}, // not a whole cent: never silently rounded
		{math.NaN(), 0, false},
		{math.Inf(1), 0, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.amount), func(t *testing.T) {
			got, err := topUpAmountCents(tc.amount)
			if tc.ok && (err != nil || got != tc.want) {
				t.Errorf("topUpAmountCents(%v) = %d, %v; want %d, nil", tc.amount, got, err, tc.want)
			}
			if !tc.ok && !errors.Is(err, ErrTopUpAmount) {
				t.Errorf("topUpAmountCents(%v) err = %v, want ErrTopUpAmount", tc.amount, err)
			}
		})
	}
}

func TestCreateTopUp_NotConfigured(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	if _, err := NewService(db).CreateTopUp(context.Background(), uuid.New(), 50); !errors.Is(err, ErrPaymentsNotConfigured) {
		t.Errorf("got %v, want ErrPaymentsNotConfigured", err)
	}
}

// An invalid amount is refused before anything is written or any Stripe
// call is made.
func TestCreateTopUp_BadAmountTouchesNothing(t *testing.T) {
	s, mock, gw := newTopUpMock(t)
	if _, err := s.CreateTopUp(context.Background(), uuid.New(), 5); !errors.Is(err, ErrTopUpAmount) {
		t.Errorf("got %v, want ErrTopUpAmount", err)
	}
	if gw.topUpCalls != 0 {
		t.Errorf("Stripe called %d times for an invalid amount", gw.topUpCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func expectAccountForStripe(mock sqlmock.Sqlmock, account uuid.UUID, customerID any, status string) {
	mock.ExpectQuery(`SELECT stripe_customer_id, billing_email, account_number, display_name, legal_name, status`).
		WithArgs(account).
		WillReturnRows(sqlmock.NewRows(
			[]string{"stripe_customer_id", "billing_email", "account_number", "display_name", "legal_name", "status"},
		).AddRow(customerID, "a@example.com", "ACC-001", "Acme", nil, status))
}

// A top-up records a pending row, opens the PaymentIntent with an
// idempotency key derived from that row, and stores the intent id so the
// webhook can find it. No credit is added here.
func TestCreateTopUp_Success(t *testing.T) {
	s, mock, gw := newTopUpMock(t)
	account := uuid.New()
	topUpID := uuid.New()

	expectAccountForStripe(mock, account, "cus_existing", "active")
	mock.ExpectQuery(`INSERT INTO billing\.credit_topups`).
		WithArgs(account, 50.0, "USD").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(topUpID))
	mock.ExpectExec(`UPDATE billing\.credit_topups\s+SET stripe_payment_intent_id`).
		WithArgs(topUpID, "pi_topup").
		WillReturnResult(sqlmock.NewResult(0, 1))

	intent, err := s.CreateTopUp(context.Background(), account, 50)
	if err != nil {
		t.Fatalf("CreateTopUp: %v", err)
	}
	if intent.ClientSecret != "pi_topup_secret" || intent.TopUpID != topUpID || intent.Amount != 50 {
		t.Errorf("intent = %+v", intent)
	}
	if gw.lastAmount != 5000 {
		t.Errorf("charged %d cents, want 5000", gw.lastAmount)
	}
	if gw.lastIdempotencyKey != "topup-"+topUpID.String() {
		t.Errorf("idempotency key = %q, want one derived from the top-up id", gw.lastIdempotencyKey)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestCreateTopUp_ClosedAccountRefused(t *testing.T) {
	s, mock, gw := newTopUpMock(t)
	account := uuid.New()
	expectAccountForStripe(mock, account, "cus_existing", "closed")

	if _, err := s.CreateTopUp(context.Background(), account, 50); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("got %v, want ErrAccountClosed", err)
	}
	if gw.topUpCalls != 0 {
		t.Error("Stripe was called for a closed account")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// If Stripe refuses to open the intent, the attempt is recorded as failed
// rather than left looking like it still awaits payment.
func TestCreateTopUp_StripeErrorMarksFailed(t *testing.T) {
	s, mock, gw := newTopUpMock(t)
	gw.retErr = errors.New("stripe down")
	account := uuid.New()
	topUpID := uuid.New()

	expectAccountForStripe(mock, account, "cus_existing", "active")
	mock.ExpectQuery(`INSERT INTO billing\.credit_topups`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(topUpID))
	mock.ExpectExec(`UPDATE billing\.credit_topups\s+SET status = 'failed'`).
		WithArgs(topUpID, "stripe down").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := s.CreateTopUp(context.Background(), account, 50); err == nil {
		t.Fatal("expected the Stripe error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func expectSettlePreamble(mock sqlmock.Sqlmock, piID string, account uuid.UUID) {
	mock.ExpectQuery(`SELECT account_id FROM billing\.credit_topups WHERE stripe_payment_intent_id`).
		WithArgs(piID).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(account))
	mock.ExpectQuery(`SELECT a\.id, a\.account_number.*FROM auth\.accounts`).
		WithArgs(account).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "account_number", "legal_name", "display_name", "billing_email", "billing_address", "tax_id", "country"},
		).AddRow(account, "ACC-001", "Acme Inc", "Acme", "billing@acme.test", "", "", "US"))
	mock.ExpectBegin()
}

func lockedTopUpRow(topUpID uuid.UUID, amount float64, status string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "amount", "currency", "status"}).
		AddRow(topUpID, amount, "USD", status)
}

// A confirmed payment becomes spendable credit and a paid receipt, all in
// one transaction: the purchase ledger row names the receipt it came with.
func TestSettleTopUp_CreditsAndIssuesReceipt(t *testing.T) {
	s, mock, _ := newTopUpMock(t)
	account := uuid.New()
	topUpID := uuid.New()
	receiptID := uuid.New()
	year := time.Now().UTC().Year()

	expectSettlePreamble(mock, "pi_1", account)
	mock.ExpectQuery(`SELECT id, amount, currency, status\s+FROM billing\.credit_topups.*FOR UPDATE`).
		WithArgs("pi_1").
		WillReturnRows(lockedTopUpRow(topUpID, 50, "processing"))
	mock.ExpectQuery(`INSERT INTO billing\.invoice_counters`).
		WithArgs("RCT", year).
		WillReturnRows(sqlmock.NewRows([]string{"last_number"}).AddRow(int64(7)))
	mock.ExpectQuery(`INSERT INTO billing\.invoices.*'paid','credit_purchase'`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(receiptID))
	mock.ExpectExec(`INSERT INTO billing\.invoice_line_items`).
		WithArgs(receiptID, creditPurchaseDescription, 50.0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO billing\.credit_transactions.*'purchase'`).
		WithArgs(account, 50.0, fmt.Sprintf("Credit purchase (RCT-%d-000007)", year), topUpID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.credit_topups\s+SET status = 'succeeded'`).
		WithArgs(topUpID, "Visa ending 4242", receiptID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := s.SettleTopUpByPaymentIntent(context.Background(), "pi_1", 5000, "usd", "Visa ending 4242"); err != nil {
		t.Fatalf("SettleTopUpByPaymentIntent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Stripe retries webhooks: a replay for an already-settled top-up must add
// nothing and issue no second receipt.
func TestSettleTopUp_ReplayIsNoop(t *testing.T) {
	s, mock, _ := newTopUpMock(t)
	account := uuid.New()

	expectSettlePreamble(mock, "pi_1", account)
	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("pi_1").
		WillReturnRows(lockedTopUpRow(uuid.New(), 50, "succeeded"))
	mock.ExpectRollback()

	if err := s.SettleTopUpByPaymentIntent(context.Background(), "pi_1", 5000, "usd", ""); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// If Stripe reports collecting something other than what the top-up
// recorded, no credit is added and the top-up is marked failed for review.
func TestSettleTopUp_AmountMismatchCreditsNothing(t *testing.T) {
	s, mock, _ := newTopUpMock(t)
	account := uuid.New()
	topUpID := uuid.New()

	expectSettlePreamble(mock, "pi_1", account)
	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("pi_1").
		WillReturnRows(lockedTopUpRow(topUpID, 50, "pending"))
	mock.ExpectExec(`UPDATE billing\.credit_topups\s+SET status = 'failed'`).
		WithArgs(topUpID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := s.SettleTopUpByPaymentIntent(context.Background(), "pi_1", 4999, "usd", ""); err != nil {
		t.Fatalf("mismatch: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected no credit or receipt writes: %v", err)
	}
}

func TestSettleTopUp_UnknownPaymentIntent(t *testing.T) {
	s, mock, _ := newTopUpMock(t)
	mock.ExpectQuery(`SELECT account_id FROM billing\.credit_topups`).
		WithArgs("pi_unknown").
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}))

	err := s.SettleTopUpByPaymentIntent(context.Background(), "pi_unknown", 5000, "usd", "")
	if !errors.Is(err, ErrTopUpNotFound) {
		t.Errorf("got %v, want ErrTopUpNotFound", err)
	}
}

// A failure never downgrades a top-up that already succeeded, and a blank
// provider reason still leaves the customer an explanation.
func TestFailTopUp(t *testing.T) {
	s, mock, _ := newTopUpMock(t)
	mock.ExpectExec(`UPDATE billing\.credit_topups\s+SET status = 'failed'.*status != 'succeeded'`).
		WithArgs("pi_1", "the payment did not go through").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.FailTopUpByPaymentIntent(context.Background(), "pi_1", "  "); err != nil {
		t.Fatalf("FailTopUpByPaymentIntent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestMarkTopUpProcessing(t *testing.T) {
	s, mock, _ := newTopUpMock(t)
	mock.ExpectExec(`UPDATE billing\.credit_topups\s+SET status = 'processing'.*status IN \('pending', 'failed'\)`).
		WithArgs("pi_1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.MarkTopUpProcessing(context.Background(), "pi_1"); err != nil {
		t.Fatalf("MarkTopUpProcessing: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Another account's top-up is not found, never forbidden.
func TestGetTopUp_OtherAccountNotFound(t *testing.T) {
	s, mock, _ := newTopUpMock(t)
	account, id := uuid.New(), uuid.New()
	mock.ExpectQuery(`FROM billing\.credit_topups\s+WHERE id = \$1 AND account_id = \$2`).
		WithArgs(id, account).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	if _, err := s.GetTopUp(context.Background(), account, id); !errors.Is(err, ErrTopUpNotFound) {
		t.Errorf("got %v, want ErrTopUpNotFound", err)
	}
}
