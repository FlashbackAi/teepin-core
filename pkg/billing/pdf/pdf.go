// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package pdf renders a billing invoice into a print-ready PDF document.
//
// The output is the exact artifact a customer hands to their accounts
// department, so it is deliberately a fixed, self-contained layout — no
// external fonts, no network, no headless browser. It takes a fully
// populated *billing.Invoice (line items and bill-to snapshot already
// resolved) and returns bytes; it performs no I/O of its own, which keeps
// it a pure function and unit-testable without AWS or a database.
//
// Rendered ONCE, at issue time, and stored verbatim (see
// INVOICE-DESIGN.md): if this template changes later, invoices already
// sent keep the bytes they were sent with. Never re-render a stored
// invoice on demand.
package pdf

import (
	"bytes"
	_ "embed"
	"fmt"

	"github.com/go-pdf/fpdf"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

// The Teepin brand mark. The console ships two, one per theme; a PDF is
// always on white, so the dark-letterform asset is the correct one. A
// copy lives here rather than a reference across into the console because
// go:embed cannot reach outside its own directory tree.
//
//go:embed assets/logo-black.png
var logoPNG []byte

// Issuer identity — Teepin's own "from" details, the same on every
// invoice, so they are compiled in rather than carried per-invoice.
// (The bill-to side IS per-invoice and snapshotted onto the row.) A
// mailing address and tax/registration number were not supplied; the
// layout omits those lines rather than printing empty labels.
const (
	issuerMark    = "TEEPIN"
	issuerEntity  = "Flashback Labs, Inc"
	issuerContact = "contact@flashbacklabs.com"
)

// Page geometry, in millimetres (A4). Kept as named constants so the
// three columns of the totals block and the line-item table stay in
// register when any one margin is tweaked.
const (
	pageWidth  = 210.0
	marginX    = 18.0
	contentW   = pageWidth - 2*marginX // usable width between margins
	logoDrawW  = 83.0                  // logo width; height follows its aspect
	logoAspect = 120.0 / 744.0         // height / width of logo-black.png (TeepinWordmark's viewBox)
	dateLayout = "Jan 2, 2006"
)

// Greys used for secondary text and table rules. The document is
// monochrome by intent — an invoice earns trust by looking plain and
// official, not colourful.
var (
	inkColor  = [3]int{17, 17, 17}    // near-black body text
	muteColor = [3]int{110, 110, 110} // labels, secondary text
	ruleColor = [3]int{224, 224, 224} // hairline table rules
)

// Render produces the PDF bytes for one issued invoice.
//
// inv must be fully populated — line items loaded and bill-to fields
// snapshotted (i.e. an invoice returned by Service.GetInvoice). The
// currency symbol is taken from inv.Currency; anything other than USD
// falls back to the ISO code as a prefix so an unexpected currency never
// renders a wrong symbol.
func Render(inv *billing.Invoice) ([]byte, error) {
	if inv == nil {
		return nil, fmt.Errorf("pdf: nil invoice")
	}

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(marginX, 16, marginX)
	pdf.SetAutoPageBreak(true, 18)
	pdf.AddPage()

	money := moneyFormatter(inv.Currency)

	drawLetterhead(pdf)
	drawInvoiceMeta(pdf, inv)
	drawBillTo(pdf, inv)
	drawLineItems(pdf, inv, money)
	drawTotals(pdf, inv, money)
	drawFooter(pdf, inv)

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("pdf: render invoice %s: %w", inv.InvoiceNumber, err)
	}
	return buf.Bytes(), nil
}

// drawLetterhead: logo on the left, issuer identity beneath it, and a
// large "INVOICE" word on the right so the document announces itself at
// a glance.
func drawLetterhead(pdf *fpdf.Fpdf) {
	// Register the embedded PNG from memory (no temp file).
	opt := fpdf.ImageOptions{ImageType: "PNG", ReadDpi: false}
	pdf.RegisterImageOptionsReader("logo", opt, bytes.NewReader(logoPNG))

	logoH := logoDrawW * logoAspect
	pdf.ImageOptions("logo", marginX, 16, logoDrawW, logoH, false, opt, 0, "")

	// Issuer block under the logo.
	pdf.SetXY(marginX, 16+logoH+2)
	setColor(pdf, muteColor)
	pdf.SetFont("Helvetica", "", 9)
	pdf.CellFormat(90, 4.5, issuerEntity, "", 2, "L", false, 0, "")
	pdf.CellFormat(90, 4.5, issuerContact, "", 2, "L", false, 0, "")

	// "INVOICE" title, right-aligned on the same top line as the logo.
	pdf.SetXY(pageWidth-marginX-70, 18)
	setColor(pdf, inkColor)
	pdf.SetFont("Helvetica", "B", 26)
	pdf.CellFormat(70, 12, "INVOICE", "", 0, "R", false, 0, "")
}

// drawInvoiceMeta: number, dates, status — a compact right-aligned label/
// value stack beneath the "INVOICE" title.
func drawInvoiceMeta(pdf *fpdf.Fpdf, inv *billing.Invoice) {
	rows := [][2]string{
		{"Invoice", inv.InvoiceNumber},
		{"Issued", inv.CreatedAt.Format(dateLayout)},
		{"Period", inv.PeriodStart.Format(dateLayout) + " - " + inv.PeriodEnd.Format(dateLayout)},
	}
	if inv.DueDate != nil {
		rows = append(rows, [2]string{"Due", inv.DueDate.Format(dateLayout)})
	}
	rows = append(rows, [2]string{"Status", humanStatus(inv.Status)})

	y := 34.0
	for _, r := range rows {
		pdf.SetXY(pageWidth-marginX-70, y)
		setColor(pdf, muteColor)
		pdf.SetFont("Helvetica", "", 8.5)
		pdf.CellFormat(30, 5, r[0], "", 0, "R", false, 0, "")
		setColor(pdf, inkColor)
		pdf.SetFont("Helvetica", "B", 8.5)
		pdf.CellFormat(40, 5, r[1], "", 0, "R", false, 0, "")
		y += 5
	}
}

// drawBillTo: the snapshotted customer identity. Absent lines are
// skipped rather than printed blank — a personal account may have no
// legal name, address or tax ID.
func drawBillTo(pdf *fpdf.Fpdf, inv *billing.Invoice) {
	y := 44.0
	pdf.SetXY(marginX, y)
	setColor(pdf, muteColor)
	pdf.SetFont("Helvetica", "B", 8)
	pdf.CellFormat(90, 5, "BILL TO", "", 2, "L", false, 0, "")

	setColor(pdf, inkColor)
	if inv.BillToName != "" {
		pdf.SetFont("Helvetica", "B", 11)
		pdf.CellFormat(90, 5.5, inv.BillToName, "", 2, "L", false, 0, "")
	}
	pdf.SetFont("Helvetica", "", 9.5)
	setColor(pdf, muteColor)
	for _, line := range []string{inv.BillToAddress, inv.BillToEmail} {
		if line != "" {
			pdf.CellFormat(90, 5, line, "", 2, "L", false, 0, "")
		}
	}
	if inv.BillToTaxID != "" {
		pdf.CellFormat(90, 5, "Tax ID: "+inv.BillToTaxID, "", 2, "L", false, 0, "")
	}
	if inv.BillToAccount != "" {
		pdf.CellFormat(90, 5, "Account "+inv.BillToAccount, "", 2, "L", false, 0, "")
	}
}

// Line-item table column widths (sum == contentW). Description takes the
// slack; the three right-hand columns are fixed so the numbers align in
// a clean stack.
var (
	colProject = 34.0
	colQty     = 30.0
	colAmount  = 28.0
	colDesc    = contentW - colProject - colQty - colAmount
)

func drawLineItems(pdf *fpdf.Fpdf, inv *billing.Invoice, money func(float64) string) {
	pdf.SetY(78)

	// Header row.
	setColor(pdf, muteColor)
	pdf.SetFont("Helvetica", "B", 8)
	pdf.CellFormat(colDesc, 7, "DESCRIPTION", "", 0, "L", false, 0, "")
	pdf.CellFormat(colProject, 7, "PROJECT", "", 0, "L", false, 0, "")
	pdf.CellFormat(colQty, 7, "QUANTITY", "", 0, "R", false, 0, "")
	pdf.CellFormat(colAmount, 7, "AMOUNT", "", 1, "R", false, 0, "")
	hairline(pdf)

	pdf.SetFont("Helvetica", "", 9.5)
	for _, li := range inv.LineItems {
		setColor(pdf, inkColor)
		// Description can wrap; measure its height and keep the row's
		// other cells aligned to the same block.
		x, y := pdf.GetXY()
		pdf.MultiCell(colDesc, 5.5, li.Description, "", "L", false)
		rowH := pdf.GetY() - y
		if rowH < 6 {
			rowH = 6
		}
		pdf.SetXY(x+colDesc, y)

		project := li.ProjectName
		if project == "" {
			project = "-" // account-wide line, not tied to one project
		}
		setColor(pdf, muteColor)
		pdf.CellFormat(colProject, rowH, project, "", 0, "L", false, 0, "")
		pdf.CellFormat(colQty, rowH, quantityText(li), "", 0, "R", false, 0, "")
		setColor(pdf, inkColor)
		pdf.CellFormat(colAmount, rowH, money(li.Amount), "", 1, "R", false, 0, "")
	}
	hairline(pdf)
}

func drawTotals(pdf *fpdf.Fpdf, inv *billing.Invoice, money func(float64) string) {
	// Right-aligned totals stack, occupying the two rightmost columns.
	labelW := colQty
	valueW := colAmount
	xLabel := pageWidth - marginX - labelW - valueW

	line := func(label, value string, bold bool) {
		pdf.SetX(xLabel)
		style := ""
		if bold {
			style = "B"
		}
		setColor(pdf, muteColor)
		pdf.SetFont("Helvetica", style, 9.5)
		pdf.CellFormat(labelW, 6, label, "", 0, "R", false, 0, "")
		if bold {
			setColor(pdf, inkColor)
		}
		pdf.SetFont("Helvetica", style, 9.5)
		pdf.CellFormat(valueW, 6, value, "", 1, "R", false, 0, "")
	}

	pdf.Ln(1)
	line("Subtotal", money(inv.Subtotal), false)
	line("Tax", money(inv.Tax), false)
	// A thin rule above the grand total.
	pdf.SetX(xLabel)
	drawRule(pdf, xLabel, labelW+valueW)
	line("Total", money(inv.Total), true)
}

func drawFooter(pdf *fpdf.Fpdf, inv *billing.Invoice) {
	pdf.SetY(-24)
	setColor(pdf, muteColor)
	pdf.SetFont("Helvetica", "", 8)
	msg := fmt.Sprintf("%s - Invoice %s. Questions? %s",
		issuerMark, inv.InvoiceNumber, issuerContact)
	pdf.CellFormat(contentW, 5, msg, "", 0, "C", false, 0, "")
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------
//

func hairline(pdf *fpdf.Fpdf) {
	y := pdf.GetY()
	drawRule(pdf, marginX, contentW)
	pdf.SetY(y + 1)
}

func drawRule(pdf *fpdf.Fpdf, x, w float64) {
	y := pdf.GetY()
	pdf.SetDrawColor(ruleColor[0], ruleColor[1], ruleColor[2])
	pdf.SetLineWidth(0.2)
	pdf.Line(x, y, x+w, y)
}

func setColor(pdf *fpdf.Fpdf, c [3]int) {
	pdf.SetTextColor(c[0], c[1], c[2])
}

// quantityText renders the optional "120.5 GPU-hours" context, or empty
// for a flat-price line that has no meaningful quantity.
func quantityText(li billing.InvoiceLineItem) string {
	if li.Quantity == 0 {
		return "-"
	}
	if li.Unit != "" {
		return fmt.Sprintf("%s %s", trimFloat(li.Quantity), li.Unit)
	}
	return trimFloat(li.Quantity)
}

// trimFloat prints a quantity without trailing zero noise: "2" not
// "2.0000", but "120.5" kept.
func trimFloat(f float64) string {
	s := fmt.Sprintf("%.4f", f)
	// strip trailing zeros then a dangling dot
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}

// moneyFormatter returns a currency formatter. USD gets a "$" prefix;
// any other currency is prefixed with its ISO code so an unexpected
// value never renders a misleading symbol. Negative amounts (credits)
// render as "-$5.00".
func moneyFormatter(currency string) func(float64) string {
	prefix := currency + " "
	if currency == "USD" || currency == "" {
		prefix = "$"
	}
	return func(v float64) string {
		if v < 0 {
			return "-" + prefix + fmt.Sprintf("%.2f", -v)
		}
		return prefix + fmt.Sprintf("%.2f", v)
	}
}

func humanStatus(s string) string {
	switch s {
	case "open":
		return "Open"
	case "paid":
		return "Paid"
	case "void":
		return "Void"
	case "uncollectible":
		return "Uncollectible"
	case "draft":
		return "Draft"
	default:
		return s
	}
}
