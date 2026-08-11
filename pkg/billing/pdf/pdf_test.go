// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package pdf

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

// sampleInvoice is a deliberately awkward invoice: a wrapping
// description, a project-attributed usage line, an account-wide flat
// line (no project, no quantity), and a NEGATIVE credit line. If Render
// survives all four in one document, the ordinary cases are covered.
func sampleInvoice() *billing.Invoice {
	due := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	proj := uuid.New()
	return &billing.Invoice{
		ID:            uuid.New(),
		AccountID:     uuid.New(),
		InvoiceNumber: "INV-000004",
		PeriodStart:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:     time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC),
		Subtotal:      4750,
		Tax:           0,
		Total:         4750,
		Status:        "open",
		Source:        "manual",
		Currency:      "USD",
		DueDate:       &due,
		BillToName:    "Catering Company Inc",
		BillToEmail:   "billing@cateringcompany.com",
		BillToAccount: "2795049616",
		LineItems: []billing.InvoiceLineItem{
			{
				Description: "GPU compute - H100 80GB, negotiated flat rate for August across the inference workload",
				ProjectID:   &proj,
				ProjectName: "inference-prod",
				Quantity:    120.5,
				Unit:        "GPU-hours",
				Amount:      5000,
			},
			{
				Description: "Platform onboarding fee",
				Amount:      250, // account-wide: no project, no quantity
			},
			{
				Description: "Goodwill credit (partial August outage)",
				Amount:      -500, // credit: negative line
			},
		},
	}
}

func TestRender_ProducesPDF(t *testing.T) {
	got, err := Render(sampleInvoice())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Render returned no bytes")
	}
	// Every PDF begins with the %PDF- signature.
	if !bytes.HasPrefix(got, []byte("%PDF-")) {
		t.Errorf("output is not a PDF: first bytes = %q", got[:min(8, len(got))])
	}
}

func TestRender_NilInvoice(t *testing.T) {
	if _, err := Render(nil); err == nil {
		t.Error("Render(nil) returned no error")
	}
}

// TestRender_WriteSample writes a rendered PDF to disk when
// TEEPIN_PDF_OUT is set, so a human can eyeball the actual layout:
//
//	TEEPIN_PDF_OUT=C:\path\to\invoice.pdf go test ./pkg/billing/pdf/ -run WriteSample
//
// It is a no-op (skipped) in normal CI runs — a test must not litter the
// filesystem by default.
func TestRender_WriteSample(t *testing.T) {
	out := os.Getenv("TEEPIN_PDF_OUT")
	if out == "" {
		t.Skip("set TEEPIN_PDF_OUT to write a sample PDF")
	}
	b, err := Render(sampleInvoice())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Logf("wrote %d bytes to %s", len(b), out)
}
