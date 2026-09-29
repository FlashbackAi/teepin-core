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

// Numbers are per series and calendar year, gapless within each, and
// formatted with the series and year so a customer can tell an invoice from
// a receipt and date either at a glance.
func TestAllocateDocumentNumber_PerSeriesAndYear(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO billing\.invoice_counters .* ON CONFLICT \(series, year\) DO UPDATE`).
		WithArgs("INV", 2026).
		WillReturnRows(sqlmock.NewRows([]string{"last_number"}).AddRow(int64(42)))
	mock.ExpectQuery(`INSERT INTO billing\.invoice_counters`).
		WithArgs("INV", 2027).
		WillReturnRows(sqlmock.NewRows([]string{"last_number"}).AddRow(int64(1))) // a new year restarts at 1
	mock.ExpectQuery(`INSERT INTO billing\.invoice_counters`).
		WithArgs("RCT", 2026).
		WillReturnRows(sqlmock.NewRows([]string{"last_number"}).AddRow(int64(7))) // receipts count on their own
	mock.ExpectRollback()

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		series string
		year   int
		want   string
	}{
		{seriesInvoice, 2026, "INV-2026-000042"},
		{seriesInvoice, 2027, "INV-2027-000001"},
		{seriesReceipt, 2026, "RCT-2026-000007"},
	} {
		got, err := allocateDocumentNumber(context.Background(), tx, tc.series, tc.year)
		if err != nil || got != tc.want {
			t.Fatalf("%s %d = %q, %v, want %s", tc.series, tc.year, got, err, tc.want)
		}
	}
	_ = tx.Rollback()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A configured tax policy flows through invoice creation: the customer country
// from the account decides the tax, the total includes it, and the itemised
// lines are kept on the invoice.
func TestCreateAccountUsageInvoice_AppliesConfiguredTax(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	policy, err := ParseTaxRules(`[{"country":"IN","name":"IGST","rate":0.18,"code":"998315"}]`)
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(db).WithTaxPolicy(policy)

	account, proj := uuid.New(), uuid.New()
	start, end := time.Now().AddDate(0, 0, -30), time.Now().AddDate(0, 0, -1)

	mock.ExpectQuery(`SELECT a\.id, a\.account_number.*FROM auth\.accounts`).
		WithArgs(account).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "account_number", "legal_name", "display_name", "billing_email", "billing_address", "tax_id", "country"},
		).AddRow(account, "1234567890", "Acme India Pvt Ltd", "Acme", "b@acme.test", "", "", "IN"))
	mock.ExpectQuery(`FROM billing\.usage_records u\s+JOIN auth\.projects p`).
		WithArgs(account, start, end).
		WillReturnRows(sqlmock.NewRows(
			[]string{"project_id", "name", "resource_type", "unit", "quantity", "total_cost"},
		).AddRow(proj, "prod", "inference/teepin/x:output", "tokens", 1000000.0, 100.0))
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO billing\.invoice_counters`).
		WillReturnRows(sqlmock.NewRows([]string{"last_number"}).AddRow(int64(1)))
	mock.ExpectQuery(`INSERT INTO billing\.invoices`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).AddRow(uuid.New(), time.Now(), time.Now()))
	mock.ExpectExec(`INSERT INTO billing\.invoice_line_items`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	inv, err := s.CreateAccountUsageInvoice(context.Background(), account, start, end)
	if err != nil {
		t.Fatalf("CreateAccountUsageInvoice: %v", err)
	}
	if inv.Subtotal != 100 || inv.Tax != 18 || inv.Total != 118 {
		t.Errorf("subtotal/tax/total = %v/%v/%v, want 100/18/118", inv.Subtotal, inv.Tax, inv.Total)
	}
	if len(inv.TaxDetails) != 1 || inv.TaxDetails[0].Name != "IGST" || inv.TaxDetails[0].Code != "998315" {
		t.Errorf("tax details = %+v", inv.TaxDetails)
	}
	if inv.BillToCountry != "IN" {
		t.Errorf("country = %q", inv.BillToCountry)
	}
	if line := inv.LineItems[0]; line.Service != "Inference" || line.Description != "teepin/x — output tokens" {
		t.Errorf("line = %q in %q, want the catalog presentation", line.Description, line.Service)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
