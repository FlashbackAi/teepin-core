// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestCalculateVRAMCost(t *testing.T) {
	// nil DB → compiled-in default rate ($0.10/GB-hour).
	s := NewService(nil)
	ctx := context.Background()

	cases := []struct {
		vramGB int
		hours  float64
		want   float64
	}{
		{10, 1, 1.00},
		{20, 1, 2.00},
		{25, 1, 2.50}, // custom size — the old rate table returned $0 here
		{25, 2, 5.00},
		{80, 0.5, 4.00},
		{0, 10, 0.00}, // CPU-only: no GPU cost
	}
	for _, tc := range cases {
		got := s.CalculateVRAMCost(ctx, tc.vramGB, tc.hours)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("CalculateVRAMCost(%d, %.1f) = %.4f, want %.4f",
				tc.vramGB, tc.hours, got, tc.want)
		}
	}
}

func TestVRAMUnitPrice(t *testing.T) {
	s := NewService(nil)
	if got := s.VRAMUnitPrice(context.Background(), 25); math.Abs(got-2.50) > 1e-9 {
		t.Errorf("VRAMUnitPrice(25) = %.4f, want 2.50", got)
	}
}

func TestVRAMPricePerGBHour_ReadsLiveRate(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := NewService(db)

	// Admin raised the rate to $0.25/GB-hour — allocations must see it.
	mock.ExpectQuery(`SELECT vram_price_per_gb_hour FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"vram_price_per_gb_hour"}).AddRow(0.25))

	if got := s.VRAMPricePerGBHour(context.Background()); math.Abs(got-0.25) > 1e-9 {
		t.Errorf("VRAMPricePerGBHour() = %.4f, want 0.25", got)
	}

	// A second call must hit the database again (no caching).
	mock.ExpectQuery(`SELECT vram_price_per_gb_hour FROM billing\.pricing`).
		WillReturnRows(sqlmock.NewRows([]string{"vram_price_per_gb_hour"}).AddRow(0.30))

	if got := s.VRAMPricePerGBHour(context.Background()); math.Abs(got-0.30) > 1e-9 {
		t.Errorf("VRAMPricePerGBHour() second read = %.4f, want 0.30", got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestVRAMPricePerGBHour_FallsBackOnError(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := NewService(db)

	// Table missing / query failure must never break allocation —
	// fall back to the compiled-in default.
	mock.ExpectQuery(`SELECT vram_price_per_gb_hour FROM billing\.pricing`).
		WillReturnError(context.DeadlineExceeded)

	if got := s.VRAMPricePerGBHour(context.Background()); math.Abs(got-0.10) > 1e-9 {
		t.Errorf("VRAMPricePerGBHour() on error = %.4f, want default 0.10", got)
	}
}

func TestSetVRAMPricePerGBHour_RejectsNonPositive(t *testing.T) {
	s := NewService(nil)
	for _, price := range []float64{0, -0.10} {
		if err := s.SetVRAMPricePerGBHour(context.Background(), price, "test"); err == nil {
			t.Errorf("SetVRAMPricePerGBHour(%.2f) accepted, want error", price)
		}
	}
}

// CreateManualInvoice guards below pin the account-level invoicing
// design (migration 012): an invoice belongs to an ACCOUNT, and each
// line item may optionally attribute itself to one of that account's
// projects for the per-project breakdown. Neither guard talks to the
// database beyond the validation query itself — no transaction is
// opened when validation fails, which these tests confirm implicitly by
// only stubbing the validation query.

func TestCreateManualInvoice_RejectsNoLineItems(t *testing.T) {
	s := NewService(nil)

	_, err := s.CreateManualInvoice(context.Background(), ManualInvoiceRequest{
		AccountID:   uuid.New(),
		PeriodStart: time.Now(),
		PeriodEnd:   time.Now(),
	})
	if err != ErrNoLineItems {
		t.Errorf("empty line items: got %v, want ErrNoLineItems", err)
	}
}

func TestCreateManualInvoice_RejectsBlankDescription(t *testing.T) {
	s := NewService(nil)

	_, err := s.CreateManualInvoice(context.Background(), ManualInvoiceRequest{
		AccountID:   uuid.New(),
		PeriodStart: time.Now(),
		PeriodEnd:   time.Now(),
		LineItems:   []InvoiceLineItem{{Description: "   ", Amount: 10}},
	})
	if err == nil {
		t.Error("blank description accepted, want a validation error")
	}
}

// A line item naming another account's project must be rejected before
// any row is written. Without this check, an operator issuing an
// invoice for Account A could attribute a charge to Account B's
// project — corrupting BOTH accounts' project-level billing breakdowns,
// on a document customers rely on for tax purposes.
func TestCreateManualInvoice_RejectsLineItemFromOtherAccount(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	s := NewService(db)
	accountID := uuid.New()
	otherAccountsProject := uuid.New()

	// resolveBillToByAccount succeeds — the account itself is real.
	mock.ExpectQuery(`SELECT a\.id, a\.account_number.*FROM auth\.accounts`).
		WithArgs(accountID).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "account_number", "legal_name", "display_name", "billing_email", "billing_address", "tax_id"},
		).AddRow(accountID, "1234567890", "", "Acme Inc", "", "", ""))

	// verifyProjectsBelongToAccount finds ZERO matching rows: the
	// referenced project belongs to someone else.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM auth\.projects`).
		WithArgs(accountID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	_, err = s.CreateManualInvoice(context.Background(), ManualInvoiceRequest{
		AccountID:   accountID,
		PeriodStart: time.Now(),
		PeriodEnd:   time.Now(),
		LineItems: []InvoiceLineItem{
			{Description: "GPU compute", Amount: 50, ProjectID: &otherAccountsProject},
		},
	})
	if err == nil {
		t.Fatal("line item referencing another account's project was accepted")
	}

	// No transaction should have been opened — BeginTx would show up as
	// an unmet expectation if the code proceeded past validation.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected additional queries after validation failed: %v", err)
	}
}
