// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package pdf

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

// usageInvoice mirrors the real INV-000001 that prompted the redesign: the
// same usage, but built the way CreateAccountUsageInvoice now builds it (catalog
// names and services), so the tests describe what a customer will actually see.
func usageInvoice() *billing.Invoice {
	proj := uuid.New()
	other := uuid.New()
	issued := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	item := func(resource string, project uuid.UUID, name string, qty float64, unit string, unitPrice, amount float64) billing.InvoiceLineItem {
		p := billing.Classify(resource)
		return billing.InvoiceLineItem{
			Description: p.Title, Service: p.Service, ProjectID: &project, ProjectName: name,
			Quantity: qty, Unit: unit, UnitPrice: unitPrice, Amount: amount,
		}
	}

	return &billing.Invoice{
		ID:            uuid.New(),
		AccountID:     uuid.New(),
		InvoiceNumber: "INV-2026-000001",
		PeriodStart:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:     time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
		CreatedAt:     issued,
		Subtotal:      10.67,
		Total:         10.67,
		Status:        "open",
		Source:        "usage",
		Currency:      "USD",
		BillToName:    "Flashback Labs, Inc",
		BillToEmail:   "billing@flashbacklabs.com",
		BillToAddress: `"11618 Cedar Chase Road\nHerndon, Virginia 20170\nUS"`,
		BillToAccount: "9615937949",
		PaymentTerms:  billing.UsagePaymentTerms,
		LineItems: []billing.InvoiceLineItem{
			item("cpu.home", proj, "Teepin Scrapper", 176.077, "hours", 0.003, 0.53),
			item("cpu.home", other, "Stevie Intel", 71.2641, "hours", 0.008, 0.57),
			item("kumbha/teepin/fast:input", proj, "Teepin Scrapper", 45010939, "tokens", 0.000000175, 7.89),
			item("kumbha/teepin/fast:output", proj, "Teepin Scrapper", 2072459, "tokens", 0.00000081, 1.68),
			// The row that printed with a blank description before the catalog.
			item("", proj, "Teepin Scrapper", 3, "hours", 0, 0.0),
		},
	}
}

// render turns compression off so assertions can read the page text.
func render(t *testing.T, inv *billing.Invoice) string {
	t.Helper()
	compressPDF = false
	defer func() { compressPDF = true }()
	b, err := Render(inv)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.HasPrefix(b, []byte("%PDF-")) {
		t.Fatal("not a PDF")
	}
	// PDF text streams escape parentheses; undo that so assertions read the
	// text as a customer sees it.
	return strings.NewReplacer(`\(`, "(", `\)`, ")").Replace(string(b))
}

func mustContain(t *testing.T, doc string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(doc, w) {
			t.Errorf("invoice does not contain %q", w)
		}
	}
}

func mustNotContain(t *testing.T, doc string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(doc, u) {
			t.Errorf("invoice unexpectedly contains %q", u)
		}
	}
}

// The regression the redesign exists for: customers read service names and
// plain descriptions, never internal resource identifiers or blank rows.
func TestRender_UsageInvoiceShowsCatalogNamesNotRawTypes(t *testing.T) {
	doc := render(t, usageInvoice())

	mustContain(t, doc,
		"CPU compute", "Kumbha",
		"teepin/fast", "input tokens", "output tokens",
		"45.01M tokens", "2.07M tokens", "176.08 hours",
	)
	mustNotContain(t, doc,
		"cpu.home", "kumbha/teepin/fast:input", "kumbha/teepin/fast:output",
		"45010939", // raw, unformatted token count
	)
}

func TestRender_HasTheFieldsAnAccountsDepartmentNeeds(t *testing.T) {
	doc := render(t, usageInvoice())

	mustContain(t, doc,
		"Invoice", "INV-2026-000001",
		"September 1, 2026", // invoice date
		"On receipt",        // due date from the payment terms
		"TOTAL AMOUNT DUE ON RECEIPT",
		"$10.67",
		"Flashback Labs, Inc", "EIN 32-0780503", // seller identity
		"BILL TO", "billing@flashbacklabs.com", "9615937949",
		"Charged automatically to the card on file",
		"All amounts are in US Dollars (USD)",
		"No tax has been charged on this invoice",
		"Summary", "Detail",
		"Page 1 of", // page numbering
	)
	// The stored address was a quoted JSON string; it must print as lines.
	mustContain(t, doc, "11618 Cedar Chase Road", "Herndon, Virginia 20170")
	mustNotContain(t, doc, `\n`, `"11618`)
}

func TestRender_SummaryGroupsByServiceWithSubtotals(t *testing.T) {
	doc := render(t, usageInvoice())
	// Two CPU lines (0.53 + 0.57) subtotal to $1.10 under one heading.
	mustContain(t, doc, "$1.10", "$9.57") // CPU compute and Kumbha subtotals
}

func TestRender_TaxLinesAndReverseCharge(t *testing.T) {
	inv := usageInvoice()
	inv.Tax = 1.92
	inv.Total = 12.59
	inv.TaxDetails = []billing.TaxLine{{Name: "IGST", Jurisdiction: "IN", Rate: 0.18, Amount: 1.92, Code: "998315"}}
	doc := render(t, inv)
	mustContain(t, doc, "Tax detail", "IGST", "18%", "998315", "$12.59", "Tax (IGST)")
	mustNotContain(t, doc, "No tax has been charged")

	inv.TaxDetails[0].ReverseCharge = true
	inv.Tax, inv.Total = 0, 10.67
	doc = render(t, inv)
	mustContain(t, doc, "reverse charge", "accounted for by the recipient")
}

func TestRender_PayLinkOnlyWhileUnpaid(t *testing.T) {
	inv := usageInvoice()
	inv.PayURL = "https://pay.teepin.com/i/abc123"
	mustContain(t, render(t, inv), "Pay online:", "https://pay.teepin.com/i/abc123")

	paid := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	inv.Status, inv.PaidAt = "paid", &paid
	doc := render(t, inv)
	mustNotContain(t, doc, "Pay online:")
	mustContain(t, doc, "PAID ON SEPTEMBER 2, 2026")
}

func TestRender_CreditsShowSeparately(t *testing.T) {
	inv := usageInvoice()
	inv.LineItems = append(inv.LineItems, billing.InvoiceLineItem{Description: "Goodwill credit", Amount: -5, Service: "Charges"})
	inv.Total = 5.67
	doc := render(t, inv)
	mustContain(t, doc, "Credits and adjustments", "Goodwill credit", "-$5.00")
}

// An invoice issued before service grouping existed (no recorded service, raw
// resource types as descriptions) must still read cleanly when rendered.
func TestRender_LegacyUsageInvoiceIsClassifiedOnTheFly(t *testing.T) {
	inv := usageInvoice()
	for i := range inv.LineItems {
		inv.LineItems[i].Service = ""
	}
	inv.LineItems[0].Description = "cpu.home"
	inv.LineItems[2].Description = "kumbha/teepin/fast:input"
	doc := render(t, inv)
	mustContain(t, doc, "CPU compute", "Kumbha")
	mustNotContain(t, doc, "cpu.home", "kumbha/teepin/fast:input")
}

// A hand-written manual invoice keeps its own wording.
func TestRender_ManualInvoiceKeepsItsOwnDescriptions(t *testing.T) {
	inv := sampleInvoice()
	doc := render(t, inv)
	mustContain(t, doc, "Platform onboarding fee", "Goodwill credit (partial August outage)", "Charges")
}

func TestRender_LongInvoiceFlowsOntoContinuationPages(t *testing.T) {
	inv := usageInvoice()
	proj := uuid.New()
	for i := 0; i < 80; i++ {
		p := billing.Classify(fmt.Sprintf("inference/teepin/model-%02d:input", i))
		inv.LineItems = append(inv.LineItems, billing.InvoiceLineItem{
			Description: p.Title, Service: p.Service, ProjectID: &proj, ProjectName: "Big Project",
			Quantity: 1000000, Unit: "tokens", UnitPrice: 0.0000002, Amount: 0.2,
		})
	}
	doc := render(t, inv)
	mustContain(t, doc, "Page 2 of", "Page 3 of", "Invoice INV-2026-000001") // running header on later pages
}

func TestMoneyFormatter(t *testing.T) {
	m := moneyFormatter("USD")
	cases := map[float64]string{0: "$0.00", 7.5: "$7.50", 1234.5: "$1,234.50", 1234567.891: "$1,234,567.89", -1234.5: "-$1,234.50"}
	for in, want := range cases {
		if got := m(in); got != want {
			t.Errorf("money(%v) = %q, want %q", in, got, want)
		}
	}
	if got := moneyFormatter("EUR")(5); got != "EUR 5.00" {
		t.Errorf("non-USD must carry its ISO code, got %q", got)
	}
}

func TestSetIssuer_ChangesWhatIsPrinted(t *testing.T) {
	orig := currentIssuer()
	defer SetIssuer(orig)

	SetIssuer(Issuer{LegalName: "Acme Cloud LLC", Address: []string{"1 Main St", "Austin, TX"}, TaxIDs: []string{"EIN 11-2233445"}, Email: "billing@acme.test"})
	doc := render(t, usageInvoice())
	mustContain(t, doc, "Acme Cloud LLC", "1 Main St", "Austin, TX", "EIN 11-2233445")
	mustNotContain(t, doc, "Flashback Labs, Inc\\", "EIN 32-0780503") // the previous issuer is gone
}

// TestRender_WriteUsageSample writes the realistic invoice to disk when
// TEEPIN_PDF_OUT_USAGE is set, for eyeballing the layout.
func TestRender_WriteUsageSample(t *testing.T) {
	out := os.Getenv("TEEPIN_PDF_OUT_USAGE")
	if out == "" {
		t.Skip("set TEEPIN_PDF_OUT_USAGE to write a sample PDF")
	}
	inv := usageInvoice()
	inv.PayURL = "https://pay.teepin.com/i/8f3a-example"
	b, err := Render(inv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d bytes to %s", len(b), out)
}
