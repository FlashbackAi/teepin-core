// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// invoiceRowColumns mirrors the SELECT order of invoiceColumns, so a
// mocked GetInvoice row stays in step with the scan when columns are
// added (as pdf_s3_key / pdf_generated_at were in migration 013).
var invoiceRowColumns = []string{
	"id", "project_id", "account_id", "invoice_number", "period_start", "period_end",
	"subtotal", "tax", "total", "status", "stripe_invoice_id", "paid_at",
	"created_at", "updated_at", "source", "currency", "due_date", "issued_by", "notes",
	"bill_to_name", "bill_to_email", "bill_to_address", "bill_to_tax_id", "bill_to_account_number",
	"pdf_s3_key", "pdf_generated_at",
}

// oneInvoiceRow builds a single-row result for GetInvoice's SELECT with
// the given id/account/number/status and null PDF columns.
func oneInvoiceRow(id, account uuid.UUID, number, status string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows(invoiceRowColumns).AddRow(
		id, nil, account, number, now, now,
		100.0, 0.0, 100.0, status, nil, nil,
		now, now, "manual", "USD", nil, nil, "",
		"Acme Inc", "billing@acme.test", "", "", "1234567890",
		nil, nil,
	)
}

// fakePDFStore records what was uploaded, for asserting the store path.
type fakePDFStore struct {
	putKey  string
	putBody []byte
	err     error
}

func (f *fakePDFStore) PutObject(_ context.Context, key string, body []byte, _ string) error {
	if f.err != nil {
		return f.err
	}
	f.putKey = key
	f.putBody = body
	return nil
}

func (f *fakePDFStore) PresignGetObject(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	return "https://example.test/" + key + "?sig=fake", nil
}

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

// A billing period cannot end in the future — it would claim to cover
// usage that has not happened. The due date is exempt (see the guard).
func TestCreateManualInvoice_RejectsFuturePeriodEnd(t *testing.T) {
	s := NewService(nil)

	_, err := s.CreateManualInvoice(context.Background(), ManualInvoiceRequest{
		AccountID:   uuid.New(),
		PeriodStart: time.Now().AddDate(0, 0, -3),
		PeriodEnd:   time.Now().AddDate(0, 0, 30), // a month out
		LineItems:   []InvoiceLineItem{{Description: "x", Amount: 10}},
	})
	if err == nil {
		t.Error("future period end accepted, want a validation error")
	}
}

// A period that runs backwards is nonsense and must be rejected before
// any row is written.
func TestCreateManualInvoice_RejectsBackwardsPeriod(t *testing.T) {
	s := NewService(nil)

	_, err := s.CreateManualInvoice(context.Background(), ManualInvoiceRequest{
		AccountID:   uuid.New(),
		PeriodStart: time.Now().AddDate(0, 0, -1),
		PeriodEnd:   time.Now().AddDate(0, 0, -10), // end before start
		LineItems:   []InvoiceLineItem{{Description: "x", Amount: 10}},
	})
	if err == nil {
		t.Error("backwards period accepted, want a validation error")
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

// ListAccountInvoicesForCustomer must exclude drafts. A draft is an
// operator's in-progress document — the whole reason creation and
// issuing are separate steps (see INVOICE-DESIGN.md "Lifecycle") is so a
// mistyped amount is correctable BEFORE a customer holds a copy. A
// customer-facing query that includes drafts defeats that separation.
func TestListAccountInvoicesForCustomer_ExcludesDrafts(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	s := NewService(db)
	accountID := uuid.New()

	mock.ExpectQuery(`SELECT .+ FROM billing\.invoices\s+WHERE account_id = \$1 AND status != 'draft'`).
		WithArgs(accountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "account_id", "invoice_number", "period_start", "period_end",
			"subtotal", "tax", "total", "status", "stripe_invoice_id", "paid_at",
			"created_at", "updated_at", "source", "currency", "due_date", "issued_by", "notes",
			"bill_to_name", "bill_to_email", "bill_to_address", "bill_to_tax_id", "bill_to_account_number",
		}))

	if _, err := s.ListAccountInvoicesForCustomer(context.Background(), accountID); err != nil {
		t.Fatalf("ListAccountInvoicesForCustomer: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("query did not filter out drafts as expected: %v", err)
	}
}

// A usage invoice for an account produces one line per (project,
// resource), attributed to its project, with the summed cost as the
// authoritative amount and a blended unit price as context. This pins
// the account-level, per-resource breakdown the whole feature exists for.
func TestCreateAccountUsageInvoice_BuildsPerResourceLines(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	s := NewService(db)
	account := uuid.New()
	projA := uuid.New()
	projB := uuid.New()
	start := time.Now().AddDate(0, 0, -30)
	end := time.Now().AddDate(0, 0, -1)

	// resolveBillToByAccount
	mock.ExpectQuery(`SELECT a\.id, a\.account_number.*FROM auth\.accounts`).
		WithArgs(account).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "account_number", "legal_name", "display_name", "billing_email", "billing_address", "tax_id"},
		).AddRow(account, "1234567890", "Acme Inc", "Acme", "billing@acme.test", "", ""))

	// accountUsageByProjectResource: two projects, one resource each.
	mock.ExpectQuery(`FROM billing\.usage_records u\s+JOIN auth\.projects p`).
		WithArgs(account, start, end).
		WillReturnRows(sqlmock.NewRows(
			[]string{"project_id", "name", "resource_type", "unit", "quantity", "total_cost"},
		).
			AddRow(projA, "inference-prod", "gpu.h100.mig-2g", "hours", 120.0, 240.0).
			AddRow(projB, "batch-jobs", "gpu.h100.mig-1g", "hours", 50.0, 50.0))

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT nextval`).
		WillReturnRows(sqlmock.NewRows([]string{"nextval"}).AddRow(int64(7)))
	mock.ExpectQuery(`INSERT INTO billing\.invoices`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).
			AddRow(uuid.New(), time.Now(), time.Now()))
	// One line-item insert per aggregated row.
	mock.ExpectExec(`INSERT INTO billing\.invoice_line_items`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO billing\.invoice_line_items`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	inv, err := s.CreateAccountUsageInvoice(context.Background(), account, start, end)
	if err != nil {
		t.Fatalf("CreateAccountUsageInvoice: %v", err)
	}
	if inv.Source != "usage" {
		t.Errorf("source = %q, want usage", inv.Source)
	}
	if inv.ProjectID != nil {
		t.Error("account-level usage invoice should have nil ProjectID")
	}
	if got := len(inv.LineItems); got != 2 {
		t.Fatalf("line items = %d, want 2", got)
	}
	if inv.Subtotal != 290.0 {
		t.Errorf("subtotal = %.2f, want 290.00", inv.Subtotal)
	}
	// First line: 240 over 120h → $2.00/h blended, attributed to projA.
	l0 := inv.LineItems[0]
	if l0.ProjectID == nil || *l0.ProjectID != projA {
		t.Error("first line not attributed to project A")
	}
	if l0.Amount != 240.0 || l0.UnitPrice != 2.0 {
		t.Errorf("first line amount/unit = %.2f/%.2f, want 240.00/2.00", l0.Amount, l0.UnitPrice)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An account with no metered usage in the period yields no invoice
// rather than an empty one — an invoice with no lines is not a document.
func TestCreateAccountUsageInvoice_NoUsageIsError(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	s := NewService(db)
	account := uuid.New()
	start := time.Now().AddDate(0, 0, -30)
	end := time.Now().AddDate(0, 0, -1)

	mock.ExpectQuery(`SELECT a\.id, a\.account_number.*FROM auth\.accounts`).
		WithArgs(account).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "account_number", "legal_name", "display_name", "billing_email", "billing_address", "tax_id"},
		).AddRow(account, "1234567890", "Acme Inc", "Acme", "", "", ""))
	mock.ExpectQuery(`FROM billing\.usage_records u\s+JOIN auth\.projects p`).
		WithArgs(account, start, end).
		WillReturnRows(sqlmock.NewRows(
			[]string{"project_id", "name", "resource_type", "unit", "quantity", "total_cost"}))

	if _, err := s.CreateAccountUsageInvoice(context.Background(), account, start, end); err == nil {
		t.Error("no-usage period produced an invoice, want an error")
	}
	// No transaction should have opened.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected queries: %v", err)
	}
}

// Issuing an invoice must succeed even when no PDF storage is wired —
// the local-dev path, which has no AWS. The status flip is the actual
// act of issuing; the document is a separate, optional artifact.
func TestIssueInvoice_SucceedsWithoutPDFStorage(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	s := NewService(db) // no WithPDFStorage
	id := uuid.New()
	account := uuid.New()

	mock.ExpectExec(`UPDATE billing\.invoices SET status = 'open'`).
		WithArgs(id).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT .+ FROM billing\.invoices\s+WHERE id = \$1`).
		WithArgs(id).
		WillReturnRows(oneInvoiceRow(id, account, "INV-000009", "open"))
	// GetInvoice also loads line items.
	mock.ExpectQuery(`FROM billing\.invoice_line_items`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "name", "description", "quantity", "unit", "unit_price", "amount", "position",
		}))

	inv, err := s.IssueInvoice(context.Background(), id)
	if err != nil {
		t.Fatalf("IssueInvoice: %v", err)
	}
	if inv.Status != "open" {
		t.Errorf("status = %q, want open", inv.Status)
	}
	if inv.PDFAvailable() {
		t.Error("PDFAvailable true with no storage configured")
	}
	// No UPDATE ... pdf_s3_key expectation was registered — if the code
	// tried to record a key, ExpectationsWereMet would flag the stray
	// query.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected queries: %v", err)
	}
}

// With storage wired, issuing renders + uploads the document and records
// its key at {account_id}/{invoice_number}.pdf.
func TestIssueInvoice_RendersAndStoresPDF(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	store := &fakePDFStore{}
	render := func(inv *Invoice) ([]byte, error) { return []byte("%PDF-fake"), nil }
	s := NewService(db).WithPDFStorage(render, store)

	id := uuid.New()
	account := uuid.New()

	mock.ExpectExec(`UPDATE billing\.invoices SET status = 'open'`).
		WithArgs(id).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT .+ FROM billing\.invoices\s+WHERE id = \$1`).
		WithArgs(id).
		WillReturnRows(oneInvoiceRow(id, account, "INV-000010", "open"))
	mock.ExpectQuery(`FROM billing\.invoice_line_items`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "name", "description", "quantity", "unit", "unit_price", "amount", "position",
		}))
	// After a successful upload, the key is recorded on the row.
	wantKey := account.String() + "/INV-000010.pdf"
	mock.ExpectExec(`UPDATE billing\.invoices\s+SET pdf_s3_key = \$1, pdf_generated_at = NOW\(\)`).
		WithArgs(wantKey, id).
		WillReturnResult(sqlmock.NewResult(0, 1))

	inv, err := s.IssueInvoice(context.Background(), id)
	if err != nil {
		t.Fatalf("IssueInvoice: %v", err)
	}
	if store.putKey != wantKey {
		t.Errorf("uploaded key = %q, want %q", store.putKey, wantKey)
	}
	if !inv.PDFAvailable() {
		t.Error("PDFAvailable false after successful store")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A store failure after issuance must NOT fail the issue: the invoice is
// already open, and the document can be regenerated. The key is left
// unrecorded (no UPDATE), so the download endpoint reports "not yet
// available" until a backfill.
func TestIssueInvoice_StoreFailureDoesNotFailIssue(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	store := &fakePDFStore{err: errors.New("s3 down")}
	render := func(inv *Invoice) ([]byte, error) { return []byte("%PDF-fake"), nil }
	s := NewService(db).WithPDFStorage(render, store)

	id := uuid.New()
	account := uuid.New()

	mock.ExpectExec(`UPDATE billing\.invoices SET status = 'open'`).
		WithArgs(id).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT .+ FROM billing\.invoices\s+WHERE id = \$1`).
		WithArgs(id).
		WillReturnRows(oneInvoiceRow(id, account, "INV-000011", "open"))
	mock.ExpectQuery(`FROM billing\.invoice_line_items`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "name", "description", "quantity", "unit", "unit_price", "amount", "position",
		}))
	// No pdf_s3_key UPDATE expected — the upload failed before it.

	inv, err := s.IssueInvoice(context.Background(), id)
	if err != nil {
		t.Fatalf("IssueInvoice returned error on store failure: %v", err)
	}
	if inv.Status != "open" {
		t.Errorf("status = %q, want open", inv.Status)
	}
	if inv.PDFAvailable() {
		t.Error("PDFAvailable true despite store failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected queries after store failure: %v", err)
	}
}
