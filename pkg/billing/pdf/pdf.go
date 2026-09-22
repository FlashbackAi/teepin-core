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
//
// The layout follows the shape customers already know from cloud
// providers' invoices: an identity header, a summary box, a total-due
// banner, a summary of charges/credits/tax, then detail grouped by service
// with per-service subtotals. Service names and line descriptions come
// from the billing service catalog, never from raw internal identifiers.
package pdf

import (
	"bytes"
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"sync"

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

// Issuer is the legal entity the invoice is issued by. Configured once at
// startup (SetIssuer); the same on every invoice, so it is not stored per
// invoice. Empty fields are omitted rather than printed as blank labels.
type Issuer struct {
	LegalName string   // "Flashback Labs, Inc"
	Address   []string // one entry per printed line
	TaxIDs    []string // "EIN 32-0780503", a GST/VAT registration, ...
	Email     string
	Website   string
}

var (
	issuerMu sync.RWMutex
	issuer   = Issuer{
		LegalName: "Flashback Labs, Inc",
		TaxIDs:    []string{"EIN 32-0780503"},
		Email:     "contact@flashbacklabs.com",
	}
)

// SetIssuer replaces the issuer identity printed on new invoices.
func SetIssuer(i Issuer) {
	issuerMu.Lock()
	defer issuerMu.Unlock()
	issuer = i
}

func currentIssuer() Issuer {
	issuerMu.RLock()
	defer issuerMu.RUnlock()
	return issuer
}

// Page geometry, in millimetres (A4).
const (
	pageWidth  = 210.0
	marginX    = 16.0
	contentW   = pageWidth - 2*marginX // usable width between margins
	logoDrawW  = 62.0                  // logo width; height follows its aspect
	logoAspect = 120.0 / 744.0         // height / width of logo-black.png
	dateLayout = "January 2, 2006"
)

// The document is monochrome by intent: an invoice earns trust by looking
// plain and official, not colourful. One light grey fills section headers.
var (
	inkColor   = [3]int{20, 20, 20}
	muteColor  = [3]int{105, 105, 105}
	ruleColor  = [3]int{218, 218, 218}
	fillColor  = [3]int{243, 243, 243}
	bannerFill = [3]int{28, 28, 28}
)

// Line-item table columns (sum == contentW). Description takes the slack.
var (
	colProject = 30.0
	colQty     = 28.0
	colRate    = 32.0
	colAmount  = 24.0
	colDesc    = contentW - colProject - colQty - colRate - colAmount
)

// Render produces the PDF bytes for one issued invoice.
//
// inv must be fully populated — line items loaded and bill-to fields
// snapshotted (i.e. an invoice returned by Service.GetInvoice).
func Render(inv *billing.Invoice) ([]byte, error) {
	if inv == nil {
		return nil, fmt.Errorf("pdf: nil invoice")
	}

	d := &doc{
		pdf:    fpdf.New("P", "mm", "A4", ""),
		inv:    inv,
		issuer: currentIssuer(),
		money:  moneyFormatter(inv.Currency),
		prefix: currencyPrefix(inv.Currency),
	}
	d.tr = d.pdf.UnicodeTranslatorFromDescriptor("")

	d.pdf.SetCompression(compressPDF)
	d.pdf.SetMargins(marginX, 14, marginX)
	d.pdf.SetAutoPageBreak(true, 26)
	d.pdf.AliasNbPages("")
	d.pdf.SetHeaderFunc(d.pageHeader)
	d.pdf.SetFooterFunc(d.pageFooter)
	d.pdf.AddPage()

	d.letterhead()
	d.parties()
	d.dueBanner()
	d.summary()
	d.detail()
	d.taxes()
	d.paymentAndNotes()

	var buf bytes.Buffer
	if err := d.pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("pdf: render invoice %s: %w", inv.InvoiceNumber, err)
	}
	return buf.Bytes(), nil
}

type doc struct {
	pdf    *fpdf.Fpdf
	inv    *billing.Invoice
	issuer Issuer
	money  func(float64) string
	prefix string
	tr     func(string) string
}

// ---------------------------------------------------------------------
// page furniture
// ---------------------------------------------------------------------

// pageHeader draws a slim running header on continuation pages only; the
// first page has the full letterhead instead.
func (d *doc) pageHeader() {
	if d.pdf.PageNo() == 1 {
		return
	}
	d.pdf.SetY(10)
	d.color(muteColor)
	d.pdf.SetFont("Helvetica", "", 8)
	d.pdf.CellFormat(contentW/2, 5, d.tr("TEEPIN · Invoice "+d.inv.InvoiceNumber), "", 0, "L", false, 0, "")
	d.pdf.CellFormat(contentW/2, 5, d.tr(d.issuer.LegalName), "", 1, "R", false, 0, "")
	d.rule(marginX, contentW)
	d.pdf.SetY(20)
}

func (d *doc) pageFooter() {
	d.pdf.SetY(-20)
	d.color(muteColor)
	d.pdf.SetFont("Helvetica", "", 7)
	d.pdf.CellFormat(contentW, 4, d.tr(d.finePrint()), "", 1, "L", false, 0, "")
	d.pdf.SetFont("Helvetica", "", 7.5)

	left := []string{d.issuer.LegalName}
	left = append(left, d.issuer.TaxIDs...)
	if d.issuer.Email != "" {
		left = append(left, d.issuer.Email)
	}
	d.pdf.CellFormat(contentW-30, 4, d.tr(strings.Join(left, "  ·  ")), "", 0, "L", false, 0, "")
	d.pdf.CellFormat(30, 4, fmt.Sprintf("Page %d of {nb}", d.pdf.PageNo()), "", 0, "R", false, 0, "")
}

// letterhead: logo, the INVOICE title and the summary box of identifiers.
func (d *doc) letterhead() {
	opt := fpdf.ImageOptions{ImageType: "PNG", ReadDpi: false}
	d.pdf.RegisterImageOptionsReader("logo", opt, bytes.NewReader(logoPNG))
	logoH := logoDrawW * logoAspect
	d.pdf.ImageOptions("logo", marginX, 14, logoDrawW, logoH, false, opt, 0, "")

	// Title, right-aligned on the logo's line.
	d.color(inkColor)
	d.pdf.SetFont("Helvetica", "B", 24)
	d.pdf.SetXY(pageWidth-marginX-80, 13)
	d.pdf.CellFormat(80, 11, "Invoice", "", 0, "R", false, 0, "")

	// Summary box: label/value pairs under the title.
	rows := [][2]string{
		{"Invoice number", d.inv.InvoiceNumber},
		{"Invoice date", d.inv.CreatedAt.Format(dateLayout)},
		{"Billing period", d.inv.PeriodStart.Format("Jan 2") + " - " + d.inv.PeriodEnd.Format("Jan 2, 2006")},
		{"Due date", d.dueText()},
		{"Status", humanStatus(d.inv.Status)},
	}
	y := 27.0
	for _, r := range rows {
		d.pdf.SetXY(pageWidth-marginX-80, y)
		d.color(muteColor)
		d.pdf.SetFont("Helvetica", "", 8.5)
		d.pdf.CellFormat(30, 5, r[0], "", 0, "L", false, 0, "")
		d.color(inkColor)
		d.pdf.SetFont("Helvetica", "B", 8.5)
		d.pdf.CellFormat(50, 5, d.tr(r[1]), "", 0, "R", false, 0, "")
		y += 5
	}
	d.pdf.SetY(logoH + 18)
}

// dueText: an explicit due date if one was set, "On receipt" for terms that
// say so, otherwise a dash.
func (d *doc) dueText() string {
	if d.inv.DueDate != nil {
		return d.inv.DueDate.Format(dateLayout)
	}
	if strings.HasPrefix(strings.ToLower(d.inv.PaymentTerms), "due on receipt") {
		return "On receipt"
	}
	return "-"
}

// parties: the issuer on the left, the customer beneath it.
func (d *doc) parties() {
	// Below the letterhead and its identifier box (which ends near y=52).
	y := 60.0
	half := contentW/2 - 4

	d.pdf.SetXY(marginX, y)
	d.label("FROM")
	d.pdf.SetX(marginX)
	d.color(inkColor)
	d.pdf.SetFont("Helvetica", "B", 10)
	d.pdf.CellFormat(half, 5, d.tr(d.issuer.LegalName), "", 2, "L", false, 0, "")
	d.pdf.SetFont("Helvetica", "", 9)
	d.color(muteColor)
	for _, l := range d.issuer.Address {
		d.pdf.CellFormat(half, 4.6, d.tr(l), "", 2, "L", false, 0, "")
	}
	for _, id := range d.issuer.TaxIDs {
		d.pdf.CellFormat(half, 4.6, d.tr(id), "", 2, "L", false, 0, "")
	}
	if d.issuer.Email != "" {
		d.pdf.CellFormat(half, 4.6, d.tr(d.issuer.Email), "", 2, "L", false, 0, "")
	}
	fromBottom := d.pdf.GetY()

	// Bill to, right column.
	x := marginX + contentW/2 + 4
	d.pdf.SetXY(x, y)
	d.label("BILL TO")
	d.pdf.SetX(x)
	d.color(inkColor)
	if d.inv.BillToName != "" {
		d.pdf.SetFont("Helvetica", "B", 10)
		d.pdf.CellFormat(half, 5, d.tr(d.inv.BillToName), "", 2, "L", false, 0, "")
	}
	d.pdf.SetFont("Helvetica", "", 9)
	d.color(muteColor)
	for _, l := range strings.Split(billing.FormatAddress(d.inv.BillToAddress), "\n") {
		if l != "" {
			d.pdf.SetX(x)
			d.pdf.CellFormat(half, 4.6, d.tr(l), "", 2, "L", false, 0, "")
		}
	}
	if d.inv.BillToEmail != "" {
		d.pdf.SetX(x)
		d.pdf.CellFormat(half, 4.6, d.tr(d.inv.BillToEmail), "", 2, "L", false, 0, "")
	}
	if d.inv.BillToTaxID != "" {
		d.pdf.SetX(x)
		d.pdf.CellFormat(half, 4.6, d.tr("Tax ID: "+d.inv.BillToTaxID), "", 2, "L", false, 0, "")
	}
	if d.inv.BillToAccount != "" {
		d.pdf.SetX(x)
		d.pdf.CellFormat(half, 4.6, d.tr("Account number: "+d.inv.BillToAccount), "", 2, "L", false, 0, "")
	}

	if fromBottom > d.pdf.GetY() {
		d.pdf.SetY(fromBottom)
	}
	d.pdf.Ln(4)
}

// dueBanner: the one number a customer looks for.
func (d *doc) dueBanner() {
	y := d.pdf.GetY()
	label, amount := "TOTAL AMOUNT DUE", d.inv.Total
	switch d.inv.Status {
	case "paid":
		label = "PAID"
		if d.inv.PaidAt != nil {
			label = "PAID ON " + strings.ToUpper(d.inv.PaidAt.Format("January 2, 2006"))
		}
	case "void":
		label = "VOID"
	default:
		if d.inv.DueDate != nil {
			label = "TOTAL AMOUNT DUE ON " + strings.ToUpper(d.inv.DueDate.Format("January 2, 2006"))
		} else if strings.HasPrefix(strings.ToLower(d.inv.PaymentTerms), "due on receipt") {
			label = "TOTAL AMOUNT DUE ON RECEIPT"
		}
	}

	d.pdf.SetFillColor(bannerFill[0], bannerFill[1], bannerFill[2])
	d.pdf.Rect(marginX, y, contentW, 13, "F")
	d.pdf.SetTextColor(255, 255, 255)
	d.pdf.SetFont("Helvetica", "B", 10)
	d.pdf.SetXY(marginX+4, y)
	d.pdf.CellFormat(contentW-60, 13, d.tr(label), "", 0, "L", false, 0, "")
	d.pdf.SetFont("Helvetica", "B", 14)
	d.pdf.CellFormat(52, 13, d.tr(d.money(amount)), "", 1, "R", false, 0, "")
	d.pdf.SetY(y + 13 + 3)
	d.paymentLine()
	d.pdf.Ln(4)
}

// paymentLine: how and when this is paid, and the pay link while it is unpaid.
func (d *doc) paymentLine() {
	if d.inv.PaymentTerms != "" {
		d.pdf.SetX(marginX + 4)
		d.color(inkColor)
		d.pdf.SetFont("Helvetica", "", 9)
		d.pdf.MultiCell(contentW-8, 4.8, d.tr(d.inv.PaymentTerms), "", "L", false)
	}
	if d.inv.PayURL != "" && d.inv.Status != "paid" && d.inv.Status != "void" {
		d.pdf.SetX(marginX + 4)
		d.color(inkColor)
		d.pdf.SetFont("Helvetica", "B", 9)
		d.pdf.CellFormat(24, 6, "Pay online:", "", 0, "L", false, 0, "")
		d.pdf.SetFont("Helvetica", "U", 9)
		d.pdf.SetTextColor(0, 70, 160)
		d.pdf.CellFormat(contentW-30, 6, d.tr(d.inv.PayURL), "", 1, "L", false, 0, d.inv.PayURL)
	}
}

// ---------------------------------------------------------------------
// summary and detail
// ---------------------------------------------------------------------

// section is one service on the invoice: its lines and subtotal.
type section struct {
	name  string
	lines []billing.InvoiceLineItem
	total float64
}

// sections groups the invoice lines by service, credits last. A line's
// service comes from what was recorded at issue time; an older usage invoice
// with no recorded service is classified from its (then raw) description so
// it still reads cleanly, while a hand-written manual line keeps its own
// wording under a plain "Charges" heading.
func (d *doc) sections() (services []section, credits section) {
	byName := map[string]*section{}
	credits = section{name: "Credits and adjustments"}

	for _, li := range d.inv.LineItems {
		li = d.normalise(li)
		if li.Amount < 0 {
			credits.lines = append(credits.lines, li)
			credits.total += li.Amount
			continue
		}
		s := byName[li.Service]
		if s == nil {
			s = &section{name: li.Service}
			byName[li.Service] = s
		}
		s.lines = append(s.lines, li)
		s.total += li.Amount
	}

	for _, s := range byName {
		sort.SliceStable(s.lines, func(i, j int) bool {
			if s.lines[i].ProjectName != s.lines[j].ProjectName {
				return s.lines[i].ProjectName < s.lines[j].ProjectName
			}
			return s.lines[i].Description < s.lines[j].Description
		})
		services = append(services, *s)
	}
	// Alphabetical, with the catch-all buckets last.
	sort.SliceStable(services, func(i, j int) bool {
		ri, rj := sortRank(services[i].name), sortRank(services[j].name)
		if ri != rj {
			return ri < rj
		}
		return services[i].name < services[j].name
	})
	return services, credits
}

func sortRank(name string) int {
	switch name {
	case "Charges", "Other charges":
		return 1
	}
	return 0
}

// normalise fills in a line's service and readable description.
func (d *doc) normalise(li billing.InvoiceLineItem) billing.InvoiceLineItem {
	if li.Service != "" {
		return li
	}
	if d.inv.Source == "usage" {
		p := billing.Classify(li.Description)
		li.Service = p.Service
		li.Description = p.Title
		return li
	}
	li.Service = "Charges"
	if strings.TrimSpace(li.Description) == "" {
		li.Description = "Charge"
	}
	return li
}

func (d *doc) summary() {
	services, credits := d.sections()

	d.header("Summary")
	var charges float64
	for _, s := range services {
		charges += s.total
	}
	d.summaryRow("Charges", d.money(charges), false, false)
	for _, s := range services {
		d.summaryRow(s.name, d.money(s.total), true, false)
	}
	if len(credits.lines) > 0 {
		d.summaryRow("Credits", d.money(credits.total), false, false)
	}
	d.summaryRow(d.taxLabel(), d.money(d.inv.Tax), false, false)
	d.summaryRow("Total for this invoice", d.money(d.inv.Total), false, true)
	d.pdf.Ln(6)
}

// taxLabel names the tax honestly: the specific tax(es) when itemised.
func (d *doc) taxLabel() string {
	if len(d.inv.TaxDetails) == 0 {
		return "Tax"
	}
	var names []string
	for _, t := range d.inv.TaxDetails {
		names = append(names, t.Name)
	}
	return "Tax (" + strings.Join(names, ", ") + ")"
}

func (d *doc) summaryRow(label, value string, indent, total bool) {
	if total {
		d.pdf.SetFillColor(fillColor[0], fillColor[1], fillColor[2])
	}
	d.color(inkColor)
	style := ""
	if total {
		style = "B"
	}
	d.pdf.SetFont("Helvetica", style, 9.5)
	x := 4.0
	if indent {
		x = 10
		d.color(muteColor)
		d.pdf.SetFont("Helvetica", "", 9)
	}
	d.pdf.SetX(marginX)
	d.pdf.CellFormat(x, 6.2, "", "", 0, "L", total, 0, "")
	d.pdf.CellFormat(contentW-x-40, 6.2, d.tr(label), "", 0, "L", total, 0, "")
	d.pdf.CellFormat(40, 6.2, d.tr(value)+"  ", "", 1, "R", total, 0, "")
	if !total {
		d.rule(marginX, contentW)
	}
}

func (d *doc) detail() {
	services, credits := d.sections()
	d.header("Detail")
	for _, s := range services {
		d.sectionBlock(s)
	}
	if len(credits.lines) > 0 {
		d.sectionBlock(credits)
	}
	d.pdf.Ln(4)
}

func (d *doc) sectionBlock(s section) {
	// Keep a section title with at least its first line and column heads.
	if d.pdf.GetY() > 297-22-32 {
		d.pdf.AddPage()
	}

	// Service header: name left, subtotal right.
	d.pdf.SetFillColor(fillColor[0], fillColor[1], fillColor[2])
	d.color(inkColor)
	d.pdf.SetFont("Helvetica", "B", 10)
	d.pdf.SetX(marginX)
	d.pdf.CellFormat(contentW-40, 7, "  "+d.tr(s.name), "", 0, "L", true, 0, "")
	d.pdf.CellFormat(40, 7, d.tr(d.money(s.total))+"  ", "", 1, "R", true, 0, "")

	// Column heads.
	d.color(muteColor)
	d.pdf.SetFont("Helvetica", "B", 7.5)
	d.pdf.SetX(marginX)
	d.pdf.CellFormat(colDesc, 6, "  DESCRIPTION", "", 0, "L", false, 0, "")
	d.pdf.CellFormat(colProject, 6, "PROJECT", "", 0, "L", false, 0, "")
	d.pdf.CellFormat(colQty, 6, "QUANTITY", "", 0, "R", false, 0, "")
	d.pdf.CellFormat(colRate, 6, "RATE", "", 0, "R", false, 0, "")
	d.pdf.CellFormat(colAmount, 6, "AMOUNT  ", "", 1, "R", false, 0, "")

	for _, li := range s.lines {
		d.lineRow(li)
	}
	d.pdf.Ln(3)
}

func (d *doc) lineRow(li billing.InvoiceLineItem) {
	pres := billing.Classify("")
	if d.inv.Source == "usage" || li.Service != "" {
		pres = presentationFor(li)
	}

	// Decide the page break from a conservative estimate of the wrapped
	// height (fpdf.SplitText mishandles translated non-ASCII text), so a row
	// never starts too close to the bottom to fit.
	d.pdf.SetFont("Helvetica", "", 9)
	if est := estimateHeight(li.Description, colDesc-2); d.pdf.GetY()+est > 297-22 {
		d.pdf.AddPage()
	}

	x, y := d.pdf.GetXY()
	d.color(inkColor)
	d.pdf.SetXY(x+2, y+0.6)
	d.pdf.MultiCell(colDesc-2, 4.8, d.tr(li.Description), "", "L", false)
	// The real height, measured after drawing, so the other cells align to it.
	rowH := d.pdf.GetY() - y + 0.6
	if rowH < 6 {
		rowH = 6
	}
	d.pdf.SetXY(x+colDesc, y)

	project := li.ProjectName
	if project == "" {
		project = "-"
	}
	d.color(muteColor)
	d.pdf.SetFont("Helvetica", "", 8.5)
	d.pdf.CellFormat(colProject, rowH, d.tr(truncate(project, 18)), "", 0, "L", false, 0, "")
	d.pdf.CellFormat(colQty, rowH, d.tr(billing.FormatQuantity(li.Quantity, li.Unit)), "", 0, "R", false, 0, "")
	d.pdf.CellFormat(colRate, rowH, d.tr(shortRate(billing.FormatUnitPrice(li.UnitPrice, pres, d.prefix))), "", 0, "R", false, 0, "")
	d.color(inkColor)
	d.pdf.SetFont("Helvetica", "", 9)
	d.pdf.CellFormat(colAmount, rowH, d.tr(d.money(li.Amount))+"  ", "", 1, "R", false, 0, "")
	d.rule(marginX, contentW)
}

// presentationFor recovers how a stored line was presented, for its rate.
// Stored lines keep the customer-facing description, not the original
// resource type, so the rate style is chosen from the unit.
func presentationFor(li billing.InvoiceLineItem) billing.Presentation {
	switch strings.ToLower(li.Unit) {
	case "tokens":
		return billing.Presentation{Rate: "/ 1M tokens", Scale: 1e6}
	case "hours", "hour":
		return billing.Presentation{Rate: "/ hour", Scale: 1}
	case "gb":
		return billing.Presentation{Rate: "/ GB", Scale: 1}
	}
	return billing.Presentation{Scale: 1}
}

// shortRate keeps the rate column narrow: "$0.20 per 1M tokens" reads fine,
// but the table cell is small, so the long words are abbreviated.
func shortRate(s string) string {
	s = strings.ReplaceAll(s, " per ", " / ")
	return s
}

// ---------------------------------------------------------------------
// tax, payment, notes
// ---------------------------------------------------------------------

func (d *doc) taxes() {
	if len(d.inv.TaxDetails) == 0 {
		return
	}
	d.header("Tax detail")
	d.color(muteColor)
	d.pdf.SetFont("Helvetica", "B", 7.5)
	d.pdf.SetX(marginX)
	d.pdf.CellFormat(50, 6, "  TAX", "", 0, "L", false, 0, "")
	d.pdf.CellFormat(30, 6, "JURISDICTION", "", 0, "L", false, 0, "")
	d.pdf.CellFormat(24, 6, "RATE", "", 0, "R", false, 0, "")
	d.pdf.CellFormat(36, 6, "CODE", "", 0, "R", false, 0, "")
	d.pdf.CellFormat(contentW-140, 6, "AMOUNT  ", "", 1, "R", false, 0, "")

	reverse := false
	for _, t := range d.inv.TaxDetails {
		d.color(inkColor)
		d.pdf.SetFont("Helvetica", "", 9)
		d.pdf.SetX(marginX)
		name := t.Name
		if t.ReverseCharge {
			name += " (reverse charge)"
			reverse = true
		}
		d.pdf.CellFormat(50, 6.2, "  "+d.tr(name), "", 0, "L", false, 0, "")
		d.pdf.CellFormat(30, 6.2, d.tr(t.Jurisdiction), "", 0, "L", false, 0, "")
		d.pdf.CellFormat(24, 6.2, fmt.Sprintf("%s%%", trimFloat(t.Rate*100)), "", 0, "R", false, 0, "")
		d.pdf.CellFormat(36, 6.2, d.tr(t.Code), "", 0, "R", false, 0, "")
		d.pdf.CellFormat(contentW-140, 6.2, d.tr(d.money(t.Amount))+"  ", "", 1, "R", false, 0, "")
		d.rule(marginX, contentW)
	}
	if reverse {
		d.pdf.Ln(1)
		d.note("Reverse charge: the tax marked above is to be accounted for by the recipient and is not included in the total.")
	}
	d.pdf.Ln(4)
}

// notes prints the invoice free-text notes, if any. The page-footer fine print
// (currency, tax statement, contact) is drawn by pageFooter on every page.
func (d *doc) paymentAndNotes() {
	if strings.TrimSpace(d.inv.Notes) == "" {
		return
	}
	if d.pdf.GetY() > 297-26-24 {
		d.pdf.AddPage()
	}
	d.header("Notes")
	d.note(d.inv.Notes)
}

// finePrint is the standing statement printed in every page footer.
func (d *doc) finePrint() string {
	parts := []string{fmt.Sprintf("All amounts are in %s.", currencyName(d.inv.Currency))}
	if len(d.inv.TaxDetails) == 0 {
		parts = append(parts, "No tax has been charged on this invoice.")
	}
	parts = append(parts, "Questions? "+d.contactEmail()+" (quote the invoice number).")
	return strings.Join(parts, " ")
}

func (d *doc) contactEmail() string {
	if d.issuer.Email != "" {
		return d.issuer.Email
	}
	return "support"
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func (d *doc) header(title string) {
	d.pdf.SetFillColor(fillColor[0], fillColor[1], fillColor[2])
	d.color(inkColor)
	d.pdf.SetFont("Helvetica", "B", 10.5)
	d.pdf.SetX(marginX)
	d.pdf.CellFormat(contentW, 7.5, "  "+d.tr(title), "", 1, "L", true, 0, "")
	d.pdf.Ln(0.5)
}

func (d *doc) label(s string) {
	d.color(muteColor)
	d.pdf.SetFont("Helvetica", "B", 7.5)
	d.pdf.CellFormat(60, 5, s, "", 2, "L", false, 0, "")
}

func (d *doc) note(s string) {
	d.color(inkColor)
	d.pdf.SetFont("Helvetica", "", 9)
	d.pdf.SetX(marginX + 4)
	d.pdf.MultiCell(contentW-8, 4.8, d.tr(s), "", "L", false)
}

func (d *doc) rule(x, w float64) {
	y := d.pdf.GetY()
	d.pdf.SetDrawColor(ruleColor[0], ruleColor[1], ruleColor[2])
	d.pdf.SetLineWidth(0.2)
	d.pdf.Line(x, y, x+w, y)
}

func (d *doc) color(c [3]int) { d.pdf.SetTextColor(c[0], c[1], c[2]) }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// trimFloat prints a number without trailing zero noise: "18" not "18.00".
func trimFloat(f float64) string {
	s := fmt.Sprintf("%.4f", f)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

func currencyPrefix(currency string) string {
	if currency == "USD" || currency == "" {
		return "$"
	}
	return currency + " "
}

func currencyName(currency string) string {
	if currency == "" {
		return "US Dollars (USD)"
	}
	if currency == "USD" {
		return "US Dollars (USD)"
	}
	return currency
}

// moneyFormatter returns a currency formatter with thousands separators.
// USD gets a "$" prefix; any other currency is prefixed with its ISO code so
// an unexpected value never renders a misleading symbol. Negative amounts
// (credits) render as "-$5.00".
func moneyFormatter(currency string) func(float64) string {
	prefix := currencyPrefix(currency)
	return func(v float64) string {
		neg := v < 0
		if neg {
			v = -v
		}
		s := prefix + groupThousands(fmt.Sprintf("%.2f", v))
		if neg {
			return "-" + s
		}
		return s
	}
}

// groupThousands inserts commas into the integer part of "1234567.89".
func groupThousands(s string) string {
	intPart, frac := s, ""
	if i := strings.Index(s, "."); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	var out []byte
	for i, c := range []byte(intPart) {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out) + frac
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

// compressPDF is on in production. Tests turn it off so the text a customer
// would read can be asserted on directly in the raw bytes.
var compressPDF = true

// estimateHeight over-estimates the height of text wrapped to width mm at
// 9pt Helvetica (about 1.95mm per character), for page-break decisions only.
func estimateHeight(text string, width float64) float64 {
	perLine := int(width / 1.95)
	if perLine < 1 {
		perLine = 1
	}
	lines := (len([]rune(text)) + perLine - 1) / perLine
	if lines < 1 {
		lines = 1
	}
	return float64(lines)*4.8 + 1.2
}
