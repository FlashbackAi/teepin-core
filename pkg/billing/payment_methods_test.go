// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// AccountCanProvision is the gate's single source of truth, so its truth
// table is worth pinning exactly: active + a verified card opens the
// gate; everything else keeps it shut.
func TestAccountCanProvision(t *testing.T) {
	cases := []struct {
		name          string
		status        string
		verifiedCards int
		wantOK        bool
	}{
		{"active with verified card", "active", 1, true},
		{"active no card", "active", 0, false},
		{"active card not yet verified", "active", 0, false},
		{"suspended with card", "suspended", 1, false},
		{"closed with card", "closed", 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()
			s := NewService(db)
			account := uuid.New()

			mock.ExpectQuery(`SELECT a\.status,.*FROM auth\.accounts`).
				WithArgs(account).
				WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).
					AddRow(tc.status, tc.verifiedCards))

			ok, reason, err := s.AccountCanProvision(context.Background(), account)
			if err != nil {
				t.Fatalf("AccountCanProvision: %v", err)
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v (reason %q), want %v", ok, reason, tc.wantOK)
			}
			if !ok && reason == "" {
				t.Error("gate closed with no customer-facing reason")
			}
		})
	}
}

// The billing check must fail CLOSED — an error looking up eligibility
// returns a non-nil error so the caller denies, never opens the gate on
// a database blip.
func TestAccountCanProvision_ErrorFailsClosed(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := NewService(db)
	account := uuid.New()

	mock.ExpectQuery(`FROM auth\.accounts`).
		WithArgs(account).
		WillReturnError(context.DeadlineExceeded)

	ok, _, err := s.AccountCanProvision(context.Background(), account)
	if err == nil {
		t.Error("expected an error so the caller fails closed")
	}
	if ok {
		t.Error("gate opened despite a lookup error")
	}
}

// Removing the only verified card must be refused with
// ErrLastVerifiedCard — an account with resources must never be left
// without a means of payment.
func TestRemovePaymentMethod_LastVerifiedIs409(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := NewService(db)
	account := uuid.New()
	pmID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, stripe_payment_method_id.*FOR UPDATE`).
		WithArgs(pmID, account).
		WillReturnRows(sqlmock.NewRows([]string{"status", "stripe_payment_method_id"}).
			AddRow("verified", "pm_123"))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM billing\.payment_methods`).
		WithArgs(account).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// No UPDATE expected — it must bail before removing.
	mock.ExpectRollback()

	err = s.RemovePaymentMethod(context.Background(), account, pmID)
	if err != ErrLastVerifiedCard {
		t.Errorf("got %v, want ErrLastVerifiedCard", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected queries: %v", err)
	}
}

// Removing one of several verified cards succeeds.
func TestRemovePaymentMethod_OneOfManySucceeds(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	// No Stripe wired: detach is skipped, removal still succeeds.
	s := NewService(db)
	account := uuid.New()
	pmID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, stripe_payment_method_id.*FOR UPDATE`).
		WithArgs(pmID, account).
		WillReturnRows(sqlmock.NewRows([]string{"status", "stripe_payment_method_id"}).
			AddRow("verified", "pm_123"))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM billing\.payment_methods`).
		WithArgs(account).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectExec(`UPDATE billing\.payment_methods\s+SET status = 'removed'`).
		WithArgs(pmID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := s.RemovePaymentMethod(context.Background(), account, pmID); err != nil {
		t.Fatalf("RemovePaymentMethod: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
