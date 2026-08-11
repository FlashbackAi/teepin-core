// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

// The download endpoint is the only customer-reachable path to an
// invoice document, so its tenancy and lifecycle gates are exactly the
// boundary worth pinning: another account must get 404 (not the file, not
// a 403 that confirms existence), a draft must get 404, and an invoice
// with no stored document must get 404 rather than a broken redirect.

// invoiceCols mirrors billing.invoiceColumns' SELECT order (26 columns
// incl. the migration-013 pdf columns), so a mocked GetInvoice row scans
// cleanly.
var invoiceCols = []string{
	"id", "project_id", "account_id", "invoice_number", "period_start", "period_end",
	"subtotal", "tax", "total", "status", "stripe_invoice_id", "paid_at",
	"created_at", "updated_at", "source", "currency", "due_date", "issued_by", "notes",
	"bill_to_name", "bill_to_email", "bill_to_address", "bill_to_tax_id", "bill_to_account_number",
	"pdf_s3_key", "pdf_generated_at",
}

// mockInvoice queues a GetInvoice result (invoice row + empty line items)
// for one download request.
func mockInvoice(mock sqlmock.Sqlmock, id, account uuid.UUID, status string, pdfKey interface{}) {
	now := time.Now()
	mock.ExpectQuery(`SELECT .+ FROM billing\.invoices\s+WHERE id = \$1`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows(invoiceCols).AddRow(
			id, nil, account, "INV-000042", now, now,
			100.0, 0.0, 100.0, status, nil, nil,
			now, now, "manual", "USD", nil, nil, "",
			"Acme Inc", "", "", "", "",
			pdfKey, nil,
		))
	mock.ExpectQuery(`FROM billing\.invoice_line_items`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "name", "description", "quantity", "unit", "unit_price", "amount", "position",
		}))
}

// presignStore satisfies billing.PDFStore for the happy-path presign.
type presignStore struct{}

func (presignStore) PutObject(context.Context, string, []byte, string) error { return nil }
func (presignStore) PresignGetObject(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	return "https://invoices.example.test/" + key + "?sig=fake", nil
}

func downloadReq(h *BillingHandler, id, account uuid.UUID) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Params = gin.Params{{Key: "id", Value: id.String()}}
	if account != uuid.Nil {
		c.Set(string(auth.AccountIDKey), account)
	}
	h.DownloadInvoicePDF(c)
	return w
}

func TestDownloadInvoicePDF_CrossTenantIs404(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	key := "acct/INV-000042.pdf"
	owner, attacker := uuid.New(), uuid.New()
	invoiceID := uuid.New()

	svc := billing.NewService(db).WithPDFStorage(nil, presignStore{})
	h := NewBillingHandler(svc, nil)

	mockInvoice(mock, invoiceID, owner, "open", key)

	if w := downloadReq(h, invoiceID, attacker); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant status = %d, want 404", w.Code)
	}
}

func TestDownloadInvoicePDF_DraftIs404(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	account := uuid.New()
	invoiceID := uuid.New()

	svc := billing.NewService(db).WithPDFStorage(nil, presignStore{})
	h := NewBillingHandler(svc, nil)

	// A draft even with a key set must not be downloadable.
	mockInvoice(mock, invoiceID, account, "draft", "acct/INV-000042.pdf")

	if w := downloadReq(h, invoiceID, account); w.Code != http.StatusNotFound {
		t.Errorf("draft status = %d, want 404", w.Code)
	}
}

func TestDownloadInvoicePDF_NoDocumentIs404(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	account := uuid.New()
	invoiceID := uuid.New()

	svc := billing.NewService(db).WithPDFStorage(nil, presignStore{})
	h := NewBillingHandler(svc, nil)

	// Open invoice, but pdf_s3_key is NULL — issued before storage, or a
	// generation that has not been backfilled.
	mockInvoice(mock, invoiceID, account, "open", nil)

	if w := downloadReq(h, invoiceID, account); w.Code != http.StatusNotFound {
		t.Errorf("missing-document status = %d, want 404", w.Code)
	}
}

func TestDownloadInvoicePDF_OwnerGetsPresignedURL(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	account := uuid.New()
	invoiceID := uuid.New()

	svc := billing.NewService(db).WithPDFStorage(nil, presignStore{})
	h := NewBillingHandler(svc, nil)

	mockInvoice(mock, invoiceID, account, "open", "acct/INV-000042.pdf")

	w := downloadReq(h, invoiceID, account)
	if w.Code != http.StatusOK {
		t.Fatalf("owner status = %d, want 200", w.Code)
	}
	// The body carries the presigned URL the client will navigate to;
	// a redirect would be a cross-origin fetch the browser blocks.
	var body struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.URL == "" {
		t.Error("response had no url")
	}
}
