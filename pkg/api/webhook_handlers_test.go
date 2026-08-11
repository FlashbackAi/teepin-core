// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

// fakeVerifier stands in for the Stripe adapter: it returns a canned
// event, or an error to simulate a bad signature.
type fakeVerifier struct {
	event   *WebhookEvent
	err     error
	card    *CardDetails
	cardErr error
}

func (f *fakeVerifier) VerifyWebhook([]byte, string) (*WebhookEvent, error) {
	return f.event, f.err
}
func (f *fakeVerifier) GetCard(string) (*CardDetails, error) {
	return f.card, f.cardErr
}

func postWebhook(h *WebhookHandler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe",
		strings.NewReader(`{}`))
	c.Request.Header.Set("Stripe-Signature", "t=1,v1=whatever")
	h.HandleStripe(c)
	return w
}

// A body that fails signature verification is rejected with 400 and the
// billing service is never touched — a forged event must not mark a card
// verified.
func TestWebhook_BadSignatureRejected(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	// No queries expected at all.
	h := NewWebhookHandler(&fakeVerifier{err: errors.New("bad sig")}, billing.NewService(db))

	if w := postWebhook(h); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("billing was queried on a bad signature: %v", err)
	}
}

// setup_intent.succeeded fetches the card and marks the pending row
// verified — the event that opens the provisioning gate.
func TestWebhook_SetupIntentSucceededVerifies(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	v := &fakeVerifier{
		event: &WebhookEvent{Type: "setup_intent.succeeded", SetupIntentID: "seti_1", PaymentMethodID: "pm_1"},
		card:  &CardDetails{Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030, PaymentMethodID: "pm_1"},
	}
	h := NewWebhookHandler(v, billing.NewService(db))

	// MarkPaymentMethodVerified: tx → find pending row → update → clear
	// grace clock → commit.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT account_id,.*FROM billing\.payment_methods\s+WHERE stripe_setup_intent_id`).
		WithArgs("seti_1").
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "count"}).
			AddRow("00000000-0000-0000-0000-0000000000aa", 0))
	mock.ExpectExec(`UPDATE billing\.payment_methods\s+SET status = 'verified'`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE auth\.accounts SET payment_failed_at = NULL`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if w := postWebhook(h); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A replayed event whose pending row is already gone is a no-op that
// still returns 200 — webhooks must be idempotent (Stripe retries).
func TestWebhook_ReplayIsIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	v := &fakeVerifier{
		event: &WebhookEvent{Type: "setup_intent.succeeded", SetupIntentID: "seti_1", PaymentMethodID: "pm_1"},
		card:  &CardDetails{Brand: "visa", Last4: "4242", PaymentMethodID: "pm_1"},
	}
	h := NewWebhookHandler(v, billing.NewService(db))

	// No matching pending row (already processed) → rollback, no error.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT account_id,.*FROM billing\.payment_methods`).
		WithArgs("seti_1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	if w := postWebhook(h); w.Code != http.StatusOK {
		t.Errorf("replay status = %d, want 200", w.Code)
	}
}

// payment_intent.succeeded settles the invoice tied to the pi — matched by
// the pi id, flipping open→paid.
func TestWebhook_PaymentIntentSucceededSettles(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	v := &fakeVerifier{event: &WebhookEvent{Type: "payment_intent.succeeded", PaymentIntentID: "pi_1"}}
	h := NewWebhookHandler(v, billing.NewService(db))

	acct := "00000000-0000-0000-0000-0000000000aa"
	inv := "00000000-0000-0000-0000-0000000000bb"
	// Find the open invoice by pi id, then MarkInvoicePaid (tx).
	mock.ExpectQuery(`SELECT id, status FROM billing\.invoices WHERE stripe_payment_intent_id`).
		WithArgs("pi_1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(inv, "open"))
	mock.ExpectBegin()
	mock.ExpectQuery(`UPDATE billing\.invoices\s+SET status = 'paid'`).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(acct))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM billing\.invoices`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`UPDATE auth\.accounts SET payment_failed_at = NULL`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if w := postWebhook(h); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// payment_intent.payment_failed records the failure against the invoice.
func TestWebhook_PaymentIntentFailedRecords(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	v := &fakeVerifier{event: &WebhookEvent{
		Type: "payment_intent.payment_failed", PaymentIntentID: "pi_1", FailureReason: "card declined",
	}}
	h := NewWebhookHandler(v, billing.NewService(db))

	acct := "00000000-0000-0000-0000-0000000000aa"
	inv := "00000000-0000-0000-0000-0000000000bb"
	mock.ExpectQuery(`SELECT id, status FROM billing\.invoices WHERE stripe_payment_intent_id`).
		WithArgs("pi_1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(inv, "open"))
	// recordChargeFailure with bump=false; attempts below cap ⇒ no clock arm.
	mock.ExpectBegin()
	mock.ExpectQuery(`UPDATE billing\.invoices\s+SET last_charge_error`).
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "charge_attempts"}).AddRow(acct, 1))
	mock.ExpectCommit()

	if w := postWebhook(h); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An unknown event type is acknowledged with 200 and touches nothing.
func TestWebhook_UnknownEventAcknowledged(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	h := NewWebhookHandler(&fakeVerifier{event: &WebhookEvent{Type: "charge.refunded"}}, billing.NewService(db))

	if w := postWebhook(h); w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected queries for unknown event: %v", err)
	}
}
