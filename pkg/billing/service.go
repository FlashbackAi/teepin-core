// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// PDFStore is the subset of object storage the billing service needs:
// write a rendered invoice document at issue time, and mint a short-lived
// read URL for a customer download. Defined here as an interface (rather
// than importing the concrete s3 package) so billing has no dependency on
// AWS — main injects a *storage/s3.Client, tests inject a fake, and
// neither creates an import cycle.
type PDFStore interface {
	PutObject(ctx context.Context, key string, body []byte, contentType string) error
	PresignGetObject(ctx context.Context, key, filename string, ttl time.Duration) (string, error)
}

// PDFRenderer turns an issued invoice into PDF bytes. The concrete
// implementation lives in pkg/billing/pdf; billing takes it as a
// function value so it does not import that package (pdf imports
// billing for the Invoice type — importing back would be a cycle).
type PDFRenderer func(inv *Invoice) ([]byte, error)

// Service handles billing and usage tracking
type Service struct {
	db      *sql.DB
	pricing *ResourcePricing

	// pdfStore and renderInvoicePDF are OPTIONAL. When either is nil,
	// IssueInvoice still issues the invoice but stores no document — the
	// path taken in local development, which has no AWS. Both are wired
	// together at the composition root (main) or left nil.
	pdfStore         PDFStore
	renderInvoicePDF PDFRenderer

	// stripe is OPTIONAL. Nil in local dev (no Stripe keys): CreateSetupIntent
	// errors, and AccountCanProvision still works off the DB (it never
	// calls Stripe). Wired at the composition root via WithStripe.
	stripe StripeGateway
}

// NewService creates a new billing service
func NewService(db *sql.DB) *Service {
	return &Service{
		db:      db,
		pricing: DefaultPricing(),
	}
}

// WithPDFStorage enables invoice-document generation at issue time. Both
// a renderer and a store are required for storage to happen; passing a
// nil either leaves the feature off. Returns the same *Service for
// chaining at construction. Kept separate from NewService so every
// existing NewService(db) call site — and all sqlmock tests — compile
// unchanged.
func (s *Service) WithPDFStorage(render PDFRenderer, store PDFStore) *Service {
	s.renderInvoicePDF = render
	s.pdfStore = store
	return s
}

// RecordUsage records a usage event for billing.
//
// SubjectType/SubjectID default to "instance"/InstanceID when the caller
// leaves SubjectType empty — see the UsageRecord doc comment. instance_id
// on the row is populated ONLY when the subject actually is an instance;
// a non-instance subject (e.g. an inference session) leaves it NULL rather
// than writing a value that would misrepresent what the row is about.
func (s *Service) RecordUsage(ctx context.Context, record *UsageRecord) error {
	subjectType := record.SubjectType
	subjectID := record.SubjectID
	if subjectType == "" {
		subjectType = "instance"
		subjectID = record.InstanceID
	}

	var instanceID any
	if subjectType == "instance" {
		instanceID = subjectID
	}

	query := `
		INSERT INTO billing.usage_records
		(account_id, project_id, instance_id, subject_type, subject_id,
		 cost_basis, provider, resource_type, quantity, unit,
		 unit_price, total_cost, start_time, end_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id, created_at
	`

	err := s.db.QueryRowContext(
		ctx, query,
		record.AccountID,
		record.ProjectID,
		instanceID,
		subjectType,
		subjectID,
		record.CostBasis,
		nullIfEmpty(record.Provider),
		record.ResourceType,
		record.Quantity,
		record.Unit,
		record.UnitPrice,
		record.TotalCost,
		record.StartTime,
		record.EndTime,
	).Scan(&record.ID, &record.CreatedAt)

	if err != nil {
		return fmt.Errorf("failed to record usage: %w", err)
	}

	return nil
}

// GetUsageRecords retrieves usage records for a project
func (s *Service) GetUsageRecords(ctx context.Context, projectID uuid.UUID, start, end time.Time) ([]UsageRecord, error) {
	// COALESCE instance_id: after migration 024 a usage record's subject is
	// not always an instance (a Kumbha inference session, e.g.), so the
	// column is nullable — scanning a NULL straight into UsageRecord's
	// plain string field would error.
	query := `
		SELECT id, project_id, COALESCE(instance_id, ''),
		       COALESCE(subject_type, ''), COALESCE(subject_id, ''),
		       resource_type, quantity, unit,
		       unit_price, total_cost, start_time, end_time, created_at
		FROM billing.usage_records
		WHERE project_id = $1 AND start_time >= $2 AND end_time <= $3
		ORDER BY start_time DESC
	`

	rows, err := s.db.QueryContext(ctx, query, projectID, start, end)
	if err != nil {
		return nil, fmt.Errorf("failed to query usage records: %w", err)
	}
	defer rows.Close()

	var records []UsageRecord
	for rows.Next() {
		var r UsageRecord
		err := rows.Scan(
			&r.ID, &r.ProjectID, &r.InstanceID, &r.SubjectType, &r.SubjectID,
			&r.ResourceType, &r.Quantity, &r.Unit, &r.UnitPrice, &r.TotalCost,
			&r.StartTime, &r.EndTime, &r.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan usage record: %w", err)
		}
		records = append(records, r)
	}

	return records, nil
}

// GetUsageSummary gets aggregated usage for a project
func (s *Service) GetUsageSummary(ctx context.Context, projectID uuid.UUID, start, end time.Time) (*UsageSummary, error) {
	query := `
		SELECT
			resource_type,
			COALESCE(instance_id, ''),
			SUM(total_cost) as cost
		FROM billing.usage_records
		WHERE project_id = $1 AND start_time >= $2 AND end_time <= $3
		GROUP BY resource_type, instance_id
	`

	rows, err := s.db.QueryContext(ctx, query, projectID, start, end)
	if err != nil {
		return nil, fmt.Errorf("failed to query usage summary: %w", err)
	}
	defer rows.Close()

	summary := &UsageSummary{
		ProjectID:   projectID,
		PeriodStart: start,
		PeriodEnd:   end,
		ByResource:  make(map[string]float64),
		ByInstance:  make(map[string]float64),
	}

	for rows.Next() {
		var resourceType, instanceID string
		var cost float64
		if err := rows.Scan(&resourceType, &instanceID, &cost); err != nil {
			return nil, fmt.Errorf("failed to scan summary: %w", err)
		}

		summary.ByResource[resourceType] += cost
		summary.ByInstance[instanceID] += cost
		summary.TotalCost += cost
	}

	return summary, nil
}

// accountUsageLine is one aggregated (project, resource) charge across a
// billing period, the raw material for a usage invoice's line items.
type accountUsageLine struct {
	ProjectID    uuid.UUID
	ProjectName  string
	ResourceType string
	Unit         string
	Quantity     float64 // SUM over the period (e.g. total GPU-hours)
	TotalCost    float64 // SUM over the period — authoritative amount
}

// accountUsageByProjectResource aggregates an account's metered usage
// across ALL its projects, grouped by (project, resource_type), for the
// billing period. One row per project+resource becomes one invoice line.
//
// It groups by project AND resource so the invoice reads the way a
// customer expects — "Project X → GPU 20GB: 120h, CPU: 96h / Project Y →
// …" — the same shape as an AWS bill breaking services out under one
// account. unit_price is deliberately NOT summed: it is derived per line
// as SUM(total_cost)/SUM(quantity) (a blended rate) at build time, so a
// mid-period rate change does not corrupt the figure; total_cost stays
// the authoritative amount.
func (s *Service) accountUsageByProjectResource(ctx context.Context, accountID uuid.UUID, start, end time.Time) ([]accountUsageLine, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.project_id, COALESCE(p.name, ''), u.resource_type,
		       COALESCE(MAX(u.unit), ''),
		       SUM(u.quantity), SUM(u.total_cost)
		FROM billing.usage_records u
		JOIN auth.projects p ON p.id = u.project_id
		WHERE p.account_id = $1
		  AND u.start_time >= $2 AND u.end_time <= $3
		GROUP BY u.project_id, p.name, u.resource_type
		HAVING SUM(u.total_cost) <> 0
		ORDER BY p.name, u.resource_type
	`, accountID, start, end)
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate account usage: %w", err)
	}
	defer rows.Close()

	var lines []accountUsageLine
	for rows.Next() {
		var l accountUsageLine
		if err := rows.Scan(&l.ProjectID, &l.ProjectName, &l.ResourceType,
			&l.Unit, &l.Quantity, &l.TotalCost); err != nil {
			return nil, fmt.Errorf("failed to scan usage line: %w", err)
		}
		lines = append(lines, l)
	}
	return lines, rows.Err()
}

// CreateAccountUsageInvoice generates a DRAFT usage invoice covering an
// entire account's metered activity for a period, with one line item per
// (project, resource) so the customer sees exactly what each project's
// resources cost — the account-level equivalent of the manual path, and
// the same per-project breakdown a customer relies on for tax purposes.
//
// Created as a draft on purpose: like a manual invoice, a usage invoice
// is reviewed and issued as a deliberate second step (which is also when
// its PDF is rendered), never sent to a customer by the act of
// generation.
func (s *Service) CreateAccountUsageInvoice(ctx context.Context, accountID uuid.UUID, periodStart, periodEnd time.Time) (*Invoice, error) {
	if err := validateInvoicePeriod(periodStart, periodEnd); err != nil {
		return nil, err
	}

	// Snapshot the customer identity before opening the transaction — a
	// bad account id should fail without consuming an invoice number.
	bill, err := s.resolveBillToByAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}

	usage, err := s.accountUsageByProjectResource(ctx, accountID, periodStart, periodEnd)
	if err != nil {
		return nil, err
	}
	if len(usage) == 0 {
		return nil, fmt.Errorf("no billable usage for this account in the period")
	}

	// Build line items. Amount is the authoritative summed cost; quantity
	// and the blended unit price are context.
	var subtotal float64
	lineItems := make([]InvoiceLineItem, 0, len(usage))
	for _, u := range usage {
		unitPrice := 0.0
		if u.Quantity != 0 {
			unitPrice = u.TotalCost / u.Quantity // blended rate over the period
		}
		projectID := u.ProjectID
		lineItems = append(lineItems, InvoiceLineItem{
			Description:  u.ResourceType,
			ProjectID:    &projectID,
			ProjectName:  u.ProjectName,
			Quantity:     u.Quantity,
			Unit:         u.Unit,
			UnitPrice:    unitPrice,
			Amount:       u.TotalCost,
			ResourceType: u.ResourceType,
		})
		subtotal += u.TotalCost
	}

	tax := 0.0 // no tax engine yet — see ROADMAP B6
	total := subtotal + tax

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	invoiceNumber, err := s.nextInvoiceNumber(ctx, tx)
	if err != nil {
		return nil, err
	}

	invoice := &Invoice{
		AccountID:     accountID,
		InvoiceNumber: invoiceNumber,
		PeriodStart:   periodStart,
		PeriodEnd:     periodEnd,
		Subtotal:      subtotal,
		Tax:           tax,
		Total:         total,
		Status:        "draft",
		Source:        "usage",
		Currency:      "USD",
		BillToName:    bill.Name,
		BillToEmail:   bill.Email,
		BillToAddress: bill.Address,
		BillToTaxID:   bill.TaxID,
		BillToAccount: bill.AccountNumber,
	}

	// account-level: project_id on the INVOICE stays null; the breakdown
	// lives on the line items, exactly like the manual path.
	err = tx.QueryRowContext(ctx, `
		INSERT INTO billing.invoices
		(account_id, invoice_number, period_start, period_end,
		 subtotal, tax, total, status, source, currency,
		 bill_to_name, bill_to_email, bill_to_address, bill_to_tax_id,
		 bill_to_account_number)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'draft','usage',$8,$9,$10,$11,$12,$13)
		RETURNING id, created_at, updated_at
	`,
		invoice.AccountID, invoice.InvoiceNumber,
		invoice.PeriodStart, invoice.PeriodEnd,
		invoice.Subtotal, invoice.Tax, invoice.Total, invoice.Currency,
		nullIfEmpty(invoice.BillToName), nullIfEmpty(invoice.BillToEmail),
		nullIfEmpty(invoice.BillToAddress), nullIfEmpty(invoice.BillToTaxID),
		nullIfEmpty(invoice.BillToAccount),
	).Scan(&invoice.ID, &invoice.CreatedAt, &invoice.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to create usage invoice: %w", err)
	}

	for i, item := range lineItems {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing.invoice_line_items
			(invoice_id, project_id, description, quantity, unit, unit_price, amount, position)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		`, invoice.ID, item.ProjectID, item.Description,
			nullIfZero(item.Quantity), nullIfEmpty(item.Unit),
			nullIfZero(item.UnitPrice), item.Amount, i,
		); err != nil {
			return nil, fmt.Errorf("failed to create usage line item %d: %w", i+1, err)
		}
		item.SortOrder = i
		invoice.LineItems = append(invoice.LineItems, item)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit usage invoice: %w", err)
	}
	return invoice, nil
}

// CalculateVRAMCost calculates GPU cost from allocated VRAM using the
// platform's linear pricing at the current admin-configured rate. This
// is model-agnostic and covers every instance type, including custom
// sizes (gpu.h100.custom-25gb) that a static rate table cannot
// enumerate.
func (s *Service) CalculateVRAMCost(ctx context.Context, vramGB int, hours float64) float64 {
	return float64(vramGB) * s.VRAMPricePerGBHour(ctx) * hours
}

// VRAMUnitPrice returns the hourly rate for a VRAM allocation at the
// current admin-configured rate.
func (s *Service) VRAMUnitPrice(ctx context.Context, vramGB int) float64 {
	return float64(vramGB) * s.VRAMPricePerGBHour(ctx)
}

// CreateInvoice generates a usage-based invoice for a billing period.
func (s *Service) CreateInvoice(ctx context.Context, projectID uuid.UUID, periodStart, periodEnd time.Time) (*Invoice, error) {
	// Get usage summary for the period
	summary, err := s.GetUsageSummary(ctx, projectID, periodStart, periodEnd)
	if err != nil {
		return nil, fmt.Errorf("failed to get usage summary: %w", err)
	}

	// AccountID is now the required primary owner of every invoice (see
	// the ProjectID comment on Invoice) — resolved from the project the
	// same way the manual path does, since a single-project usage
	// invoice still belongs to that project's account.
	bill, err := s.resolveBillTo(ctx, projectID)
	if err != nil {
		return nil, err
	}

	subtotal := summary.TotalCost
	tax := subtotal * 0.0 // No tax for now
	total := subtotal + tax

	invoice := &Invoice{
		AccountID:   bill.AccountID,
		ProjectID:   &projectID,
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		Subtotal:    subtotal,
		Tax:         tax,
		Total:       total,
		Status:      "draft",
		Source:      "usage",
		Currency:    "USD",
	}

	// A transaction so the number allocation and the insert either both
	// land or neither does — a claimed number with no matching row would
	// itself be a gap, which is the exact thing sequential numbering
	// exists to rule out.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Shares nextInvoiceNumber with CreateManualInvoice. Two separate
	// numbering schemes previously existed here — this path generated
	// "INV-<period>-<random hex>" while manual invoices used a gapless
	// sequence — which defeated the sequential guarantee the moment both
	// paths were used on the same account, and most tax authorities
	// require gapless sequential numbering across ALL invoices, not per
	// code path.
	invoiceNumber, err := s.nextInvoiceNumber(ctx, tx)
	if err != nil {
		return nil, err
	}
	invoice.InvoiceNumber = invoiceNumber

	query := `
		INSERT INTO billing.invoices
		(account_id, project_id, invoice_number, period_start, period_end, subtotal, tax, total, status, source, currency)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, created_at, updated_at
	`

	err = tx.QueryRowContext(
		ctx, query,
		invoice.AccountID,
		invoice.ProjectID,
		invoice.InvoiceNumber,
		invoice.PeriodStart,
		invoice.PeriodEnd,
		invoice.Subtotal,
		invoice.Tax,
		invoice.Total,
		invoice.Status,
		invoice.Source,
		invoice.Currency,
	).Scan(&invoice.ID, &invoice.CreatedAt, &invoice.UpdatedAt)

	if err != nil {
		return nil, fmt.Errorf("failed to create invoice: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit invoice: %w", err)
	}

	return invoice, nil
}

// ManualInvoiceRequest describes an invoice a human is issuing.
//
// Every amount is supplied rather than metered: this is for negotiated
// prices, setup fees, support retainers and credits — the things that
// exist before, alongside, or instead of usage-based billing.
//
// Account-scoped, not project-scoped: one invoice can cover an account's
// activity across every project it has, the way one AWS bill covers
// every service under an account. A per-project breakdown is still
// available — each LineItem carries its own optional ProjectID — but the
// invoice as a whole belongs to the account.
type ManualInvoiceRequest struct {
	AccountID   uuid.UUID
	PeriodStart time.Time
	PeriodEnd   time.Time
	DueDate     *time.Time
	Currency    string
	Notes       string
	IssuedBy    *uuid.UUID
	LineItems   []InvoiceLineItem
}

// billTo is the customer identity captured onto an invoice.
//
// Snapshotted, never referenced: if the customer changes their legal
// name or address later, an already-issued invoice must still show what
// was true when it was issued.
type billTo struct {
	AccountID     uuid.UUID
	AccountNumber string
	Name          string
	Email         string
	Address       string
	TaxID         string
}

// resolveBillTo reads the billing entity behind a project.
//
// Falls back through legal_name → display_name and billing_email →
// owner's email, because a personal account has no legal name and an
// invoice with a blank addressee is not sendable.
func (s *Service) resolveBillTo(ctx context.Context, projectID uuid.UUID) (*billTo, error) {
	var (
		b        billTo
		legal    sql.NullString
		display  string
		email    sql.NullString
		address  sql.NullString
		taxID    sql.NullString
		ownerEml sql.NullString
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT a.id, a.account_number, a.legal_name, a.display_name,
		       a.billing_email, a.billing_address::text, a.tax_id, u.email
		FROM auth.projects p
		JOIN auth.accounts a ON a.id = p.account_id
		LEFT JOIN auth.users u ON u.id = p.owner_id
		WHERE p.id = $1
	`, projectID).Scan(&b.AccountID, &b.AccountNumber, &legal, &display,
		&email, &address, &taxID, &ownerEml)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("project not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to resolve billing account: %w", err)
	}

	b.Name = legal.String
	if b.Name == "" {
		b.Name = display
	}
	b.Email = email.String
	if b.Email == "" {
		b.Email = ownerEml.String
	}
	b.Address = address.String
	b.TaxID = taxID.String

	return &b, nil
}

// resolveBillToByAccount reads the billing entity directly, for
// account-level invoices that are not anchored to any single project.
//
// Duplicates most of resolveBillTo rather than making that function take
// an interface{} or a "kind" flag: the two queries differ only in their
// FROM clause (accounts directly vs. joined through projects), and a
// shared abstraction over two three-line queries would cost more
// readability than the duplication does.
func (s *Service) resolveBillToByAccount(ctx context.Context, accountID uuid.UUID) (*billTo, error) {
	var (
		b       billTo
		legal   sql.NullString
		display string
		email   sql.NullString
		address sql.NullString
		taxID   sql.NullString
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT a.id, a.account_number, a.legal_name, a.display_name,
		       a.billing_email, a.billing_address::text, a.tax_id
		FROM auth.accounts a
		WHERE a.id = $1
	`, accountID).Scan(&b.AccountID, &b.AccountNumber, &legal, &display,
		&email, &address, &taxID)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("account not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to resolve billing account: %w", err)
	}

	b.Name = legal.String
	if b.Name == "" {
		b.Name = display
	}
	// No owner-email fallback here, unlike resolveBillTo: an account has
	// no single "owner user" row to join through at this query, and an
	// account-level invoice with a blank email is still sendable via
	// billing_email once set — this is a display detail, not a hard
	// requirement to issue.
	b.Email = email.String
	b.Address = address.String
	b.TaxID = taxID.String

	return &b, nil
}

// nextInvoiceNumber allocates a sequential number.
//
// Sequential and gapless, from a database sequence: most tax authorities
// require it, and a gap is meant to be evidence that an invoice was
// removed. The previous scheme used 8 random hex characters, which was
// neither sequential nor collision-proof by construction.
func (s *Service) nextInvoiceNumber(ctx context.Context, tx *sql.Tx) (string, error) {
	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT nextval('billing.invoice_number_seq')`).Scan(&n); err != nil {
		return "", fmt.Errorf("failed to allocate invoice number: %w", err)
	}
	return fmt.Sprintf("INV-%06d", n), nil
}

// ErrNoLineItems is returned for an invoice with nothing on it.
var ErrNoLineItems = errors.New("an invoice needs at least one line item")

// CreateManualInvoice issues an invoice with explicit line items.
//
// validateInvoicePeriod rejects a billing period that runs backwards or
// extends past today. A billing period covers activity that has already
// happened; an end date in the future would bill for usage that does not
// exist yet. Compared against the end of the current UTC day so that
// "period ends today" is always valid regardless of the caller's clock
// time — invoice periods are dates, not instants.
func validateInvoicePeriod(start, end time.Time) error {
	if end.Before(start) {
		return fmt.Errorf("period end cannot be before period start")
	}
	// End of today (UTC): the last instant a period may legitimately end.
	now := time.Now().UTC()
	endOfToday := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, time.UTC)
	if end.After(endOfToday) {
		return fmt.Errorf("period end cannot be in the future")
	}
	return nil
}

// The total is the SUM of the lines, never a separately supplied
// figure. Accepting both would allow a total that disagrees with its own
// breakdown — which is the one thing an invoice must never do, since the
// customer will add up the lines.
//
// Created as a draft: issuing is a deliberate second step, so an invoice
// is never sent to a customer by a typo in an amount.
func (s *Service) CreateManualInvoice(ctx context.Context, req ManualInvoiceRequest) (*Invoice, error) {
	if len(req.LineItems) == 0 {
		return nil, ErrNoLineItems
	}
	for i, item := range req.LineItems {
		if strings.TrimSpace(item.Description) == "" {
			return nil, fmt.Errorf("line item %d has no description", i+1)
		}
	}

	// The billing PERIOD cannot run backwards or into the future: an
	// invoice bills for activity that has already happened, and a period
	// ending tomorrow would claim to cover usage that does not exist yet.
	// The DUE date is deliberately NOT constrained this way — "due in 30
	// days" is a future date by design.
	if err := validateInvoicePeriod(req.PeriodStart, req.PeriodEnd); err != nil {
		return nil, err
	}

	var subtotal float64
	for _, item := range req.LineItems {
		subtotal += item.Amount
	}

	currency := req.Currency
	if currency == "" {
		currency = "USD"
	}

	// Tax is zero until there is a tax engine. Stated explicitly rather
	// than omitted so it is obvious this is a decision, not an oversight
	// — see ROADMAP B6.
	tax := 0.0
	total := subtotal + tax

	// Resolve the billing entity BEFORE opening the transaction: a bad
	// account id should fail without consuming an invoice number.
	bill, err := s.resolveBillToByAccount(ctx, req.AccountID)
	if err != nil {
		return nil, err
	}

	// Any line item naming a project must belong to THIS account — an
	// operator (or a bug) attributing a charge to another customer's
	// project would misrepresent both invoices' project breakdowns, and
	// on a document customers use for tax purposes that is not a cosmetic
	// error.
	lineProjectIDs := make([]uuid.UUID, 0, len(req.LineItems))
	for _, item := range req.LineItems {
		if item.ProjectID != nil {
			lineProjectIDs = append(lineProjectIDs, *item.ProjectID)
		}
	}
	if len(lineProjectIDs) > 0 {
		if err := s.verifyProjectsBelongToAccount(ctx, req.AccountID, lineProjectIDs); err != nil {
			return nil, err
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	invoiceNumber, err := s.nextInvoiceNumber(ctx, tx)
	if err != nil {
		return nil, err
	}

	invoice := &Invoice{
		AccountID:     bill.AccountID,
		InvoiceNumber: invoiceNumber,
		PeriodStart:   req.PeriodStart,
		PeriodEnd:     req.PeriodEnd,
		Subtotal:      subtotal,
		Tax:           tax,
		Total:         total,
		Status:        "draft",
		Source:        "manual",
		Currency:      currency,
		DueDate:       req.DueDate,
		IssuedBy:      req.IssuedBy,
		Notes:         req.Notes,
		BillToName:    bill.Name,
		BillToEmail:   bill.Email,
		BillToAddress: bill.Address,
		BillToTaxID:   bill.TaxID,
		BillToAccount: bill.AccountNumber,
	}

	// project_id on the INVOICE itself is intentionally not set here: a
	// manual invoice is account-level by construction. Per-project
	// attribution lives on the line items inserted below.
	err = tx.QueryRowContext(ctx, `
		INSERT INTO billing.invoices
		(account_id, invoice_number, period_start, period_end,
		 subtotal, tax, total, status, source, currency, due_date, issued_by, notes,
		 bill_to_name, bill_to_email, bill_to_address, bill_to_tax_id,
		 bill_to_account_number)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'draft','manual',$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING id, created_at, updated_at
	`,
		invoice.AccountID, invoice.InvoiceNumber,
		invoice.PeriodStart, invoice.PeriodEnd,
		invoice.Subtotal, invoice.Tax, invoice.Total,
		invoice.Currency, invoice.DueDate, invoice.IssuedBy, nullIfEmpty(invoice.Notes),
		nullIfEmpty(invoice.BillToName), nullIfEmpty(invoice.BillToEmail),
		nullIfEmpty(invoice.BillToAddress), nullIfEmpty(invoice.BillToTaxID),
		nullIfEmpty(invoice.BillToAccount),
	).Scan(&invoice.ID, &invoice.CreatedAt, &invoice.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to create invoice: %w", err)
	}

	// Line items in the same transaction: an invoice whose total has no
	// supporting lines is worse than no invoice at all.
	for i, item := range req.LineItems {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing.invoice_line_items
			(invoice_id, project_id, description, quantity, unit, unit_price, amount, position)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		`, invoice.ID, item.ProjectID, item.Description,
			nullIfZero(item.Quantity), nullIfEmpty(item.Unit),
			nullIfZero(item.UnitPrice), item.Amount, i,
		); err != nil {
			return nil, fmt.Errorf("failed to create line item %d: %w", i+1, err)
		}
		item.SortOrder = i
		invoice.LineItems = append(invoice.LineItems, item)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit invoice: %w", err)
	}
	return invoice, nil
}

// verifyProjectsBelongToAccount rejects any project ID that is not owned
// by accountID — see the tenancy note at its call site.
func (s *Service) verifyProjectsBelongToAccount(ctx context.Context, accountID uuid.UUID, projectIDs []uuid.UUID) error {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM auth.projects
		WHERE account_id = $1 AND id = ANY($2) AND deleted_at IS NULL
	`, accountID, pq.Array(projectIDs)).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to verify line item projects: %w", err)
	}
	// Deduplicate: the same project may appear on several line items, and
	// the check must compare distinct projects against the count matched.
	distinct := map[uuid.UUID]struct{}{}
	for _, id := range projectIDs {
		distinct[id] = struct{}{}
	}
	if count != len(distinct) {
		return fmt.Errorf("one or more line items reference a project outside this account")
	}
	return nil
}

// IssueInvoice moves a draft to open, making it payable.
//
// Separate from creation so that writing an invoice and sending it are
// distinct acts — a mistyped amount can be corrected while it is still a
// draft, and only a draft may be deleted.
func (s *Service) IssueInvoice(ctx context.Context, invoiceID uuid.UUID) (*Invoice, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE billing.invoices SET status = 'open', updated_at = NOW()
		WHERE id = $1 AND status = 'draft'
	`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("failed to issue invoice: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		// Either it does not exist or it is not a draft. Re-issuing an
		// already-open invoice must not silently succeed.
		return nil, fmt.Errorf("invoice is not a draft, or does not exist")
	}

	invoice, err := s.GetInvoice(ctx, invoiceID)
	if err != nil {
		return nil, err
	}

	// The invoice is now issued (status committed as 'open'). Rendering
	// and storing the PDF is a SEPARATE, best-effort step: the document
	// is a derived artifact, and a failure to store it must not undo the
	// issuance — a half-rolled-back re-issue would be worse than a
	// missing PDF, and the document can be regenerated later. So any
	// error here is logged and swallowed; the issued invoice is still
	// returned.
	s.generateAndStorePDF(ctx, invoice)

	return invoice, nil
}

// generateAndStorePDF renders the invoice and writes it to object
// storage, then records the key on the row. Best-effort: see the caller.
// A no-op when PDF storage is not configured (local dev).
func (s *Service) generateAndStorePDF(ctx context.Context, invoice *Invoice) {
	if s.renderInvoicePDF == nil || s.pdfStore == nil {
		return // storage not wired (e.g. local dev with no AWS)
	}

	doc, err := s.renderInvoicePDF(invoice)
	if err != nil {
		log.Printf("WARN: invoice %s issued but PDF render failed: %v", invoice.InvoiceNumber, err)
		return
	}

	// {account_id}/{invoice_number}.pdf — the stable per-invoice key.
	key := fmt.Sprintf("%s/%s.pdf", invoice.AccountID, invoice.InvoiceNumber)
	if err := s.pdfStore.PutObject(ctx, key, doc, "application/pdf"); err != nil {
		log.Printf("WARN: invoice %s issued but PDF upload failed: %v", invoice.InvoiceNumber, err)
		return
	}

	if _, err := s.db.ExecContext(ctx, `
		UPDATE billing.invoices
		SET pdf_s3_key = $1, pdf_generated_at = NOW()
		WHERE id = $2
	`, key, invoice.ID); err != nil {
		// The object is stored but the row does not know its key. The
		// download endpoint will report "not yet available" until a
		// re-issue/backfill fixes it; that is recoverable and must not
		// fail the issuance.
		log.Printf("WARN: invoice %s PDF stored at %s but recording the key failed: %v",
			invoice.InvoiceNumber, key, err)
		return
	}

	// Reflect the stored document on the returned invoice so the caller
	// (and its JSON response) reports pdf_available immediately.
	invoice.PDFS3Key = &key
	now := time.Now()
	invoice.PDFGeneratedAt = &now
}

// ErrPDFUnavailable means no downloadable document exists for an invoice
// — it was never issued, was issued before PDF storage was configured,
// or storage is not wired at all. Callers translate this to a 404 so the
// absence of a document is indistinguishable from a missing invoice.
var ErrPDFUnavailable = errors.New("invoice document not available")

// PresignInvoicePDF returns a short-lived URL to download the given
// invoice's stored PDF. Returns ErrPDFUnavailable if PDF storage is not
// configured or no document was stored. The caller is responsible for
// the tenancy check (it already holds the invoice); this method only
// turns a stored key into a downloadable URL, so it never loads or leaks
// another account's row.
//
// The URL is built to force a "save as" under a friendly filename
// (INV-000005.pdf), not the raw S3 key.
func (s *Service) PresignInvoicePDF(ctx context.Context, invoice *Invoice, ttl time.Duration) (string, error) {
	if s.pdfStore == nil || !invoice.PDFAvailable() {
		return "", ErrPDFUnavailable
	}
	filename := invoice.InvoiceNumber + ".pdf"
	return s.pdfStore.PresignGetObject(ctx, *invoice.PDFS3Key, filename, ttl)
}

// VoidInvoice cancels an invoice.
//
// Void rather than delete, for anything already issued: an invoice that
// reached a customer is a financial record, and making it disappear
// leaves the customer holding a document the platform denies existing.
func (s *Service) VoidInvoice(ctx context.Context, invoiceID uuid.UUID) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE billing.invoices SET status = 'void', updated_at = NOW()
		WHERE id = $1 AND status IN ('draft', 'open')
	`, invoiceID)
	if err != nil {
		return fmt.Errorf("failed to void invoice: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return fmt.Errorf("only draft or open invoices can be voided")
	}
	return nil
}

// lineItemsFor loads an invoice's persisted lines in display order.
func (s *Service) lineItemsFor(ctx context.Context, invoiceID uuid.UUID) ([]InvoiceLineItem, error) {
	// LEFT JOIN, not JOIN: a line item's project_id is nullable by
	// design (account-level charges), and an inner join would silently
	// drop exactly those rows from the invoice.
	rows, err := s.db.QueryContext(ctx, `
		SELECT li.id, li.project_id, COALESCE(p.name, ''), li.description,
		       COALESCE(li.quantity,0), COALESCE(li.unit,''),
		       COALESCE(li.unit_price,0), li.amount, li.position
		FROM billing.invoice_line_items li
		LEFT JOIN auth.projects p ON p.id = li.project_id
		WHERE li.invoice_id = $1
		ORDER BY li.position, li.created_at
	`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []InvoiceLineItem
	for rows.Next() {
		var item InvoiceLineItem
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.ProjectName,
			&item.Description, &item.Quantity, &item.Unit, &item.UnitPrice,
			&item.Amount, &item.SortOrder); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfZero(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

// GetInvoice retrieves an invoice by ID
func (s *Service) GetInvoice(ctx context.Context, invoiceID uuid.UUID) (*Invoice, error) {
	query := `
		SELECT ` + invoiceColumns + `
		FROM billing.invoices
		WHERE id = $1
	`

	var invoice Invoice
	var notes sql.NullString
	err := s.db.QueryRowContext(ctx, query, invoiceID).Scan(
		&invoice.ID,
		&invoice.ProjectID,
		&invoice.AccountID,
		&invoice.InvoiceNumber,
		&invoice.PeriodStart,
		&invoice.PeriodEnd,
		&invoice.Subtotal,
		&invoice.Tax,
		&invoice.Total,
		&invoice.Status,
		&invoice.StripeInvoiceID,
		&invoice.PaidAt,
		&invoice.CreatedAt,
		&invoice.UpdatedAt,
		&invoice.Source,
		&invoice.Currency,
		&invoice.DueDate,
		&invoice.IssuedBy,
		&notes,
		&invoice.BillToName,
		&invoice.BillToEmail,
		&invoice.BillToAddress,
		&invoice.BillToTaxID,
		&invoice.BillToAccount,
		&invoice.PDFS3Key,
		&invoice.PDFGeneratedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("invoice not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get invoice: %w", err)
	}
	invoice.Notes = notes.String

	// Line items only on a single fetch — a list of invoices does not
	// need every line of every one.
	items, err := s.lineItemsFor(ctx, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("failed to load line items: %w", err)
	}
	invoice.LineItems = items

	return &invoice, nil
}

// invoiceColumns is the shared select list so every invoice query stays
// in step with the scan order.
const invoiceColumns = `id, project_id, account_id, invoice_number,
	period_start, period_end, subtotal, tax, total, status,
	stripe_invoice_id, paid_at, created_at, updated_at,
	source, currency, due_date, issued_by, notes,
	COALESCE(bill_to_name,''), COALESCE(bill_to_email,''),
	COALESCE(bill_to_address,''), COALESCE(bill_to_tax_id,''),
	COALESCE(bill_to_account_number,''),
	pdf_s3_key, pdf_generated_at`

// ListInvoices lists every invoice for a project.
//
// After account-level invoicing (migration 012) this only returns
// project-anchored invoices — the pre-account-level usage path, where
// ProjectID is still set. A manual, account-level invoice with charges
// against this project on a LINE ITEM will not appear here; use
// ListAccountInvoices for the full picture.
func (s *Service) ListInvoices(ctx context.Context, projectID uuid.UUID) ([]Invoice, error) {
	query := `
		SELECT ` + invoiceColumns + `
		FROM billing.invoices
		WHERE project_id = $1
		ORDER BY created_at DESC
	`

	rows, err := s.db.QueryContext(ctx, query, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to list invoices: %w", err)
	}
	defer rows.Close()

	return scanInvoices(rows)
}

// ListAccountInvoices lists EVERY invoice belonging to an account,
// including drafts — both project-anchored (usage) and account-level
// (manual) invoices. This is the operator's view (control centre): a
// draft is an in-progress document nobody outside the platform has
// reviewed yet, and an operator building one needs to see it to edit or
// discard it.
//
// Do not expose this to customers directly — see
// ListAccountInvoicesForCustomer for the view that excludes drafts.
func (s *Service) ListAccountInvoices(ctx context.Context, accountID uuid.UUID) ([]Invoice, error) {
	query := `
		SELECT ` + invoiceColumns + `
		FROM billing.invoices
		WHERE account_id = $1
		ORDER BY created_at DESC
	`

	rows, err := s.db.QueryContext(ctx, query, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to list invoices: %w", err)
	}
	defer rows.Close()

	return scanInvoices(rows)
}

// ListAccountInvoicesForCustomer lists an account's invoices EXCLUDING
// drafts.
//
// A draft is an operator's in-progress document — see the "Lifecycle"
// section of INVOICE-DESIGN.md: creation and issuing are deliberately
// separate precisely so a mistyped amount is correctable BEFORE a
// customer holds a copy. Showing a draft to the customer it is being
// prepared for defeats that separation; they would see a number that
// might still change.
func (s *Service) ListAccountInvoicesForCustomer(ctx context.Context, accountID uuid.UUID) ([]Invoice, error) {
	query := `
		SELECT ` + invoiceColumns + `
		FROM billing.invoices
		WHERE account_id = $1 AND status != 'draft'
		ORDER BY created_at DESC
	`

	rows, err := s.db.QueryContext(ctx, query, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to list invoices: %w", err)
	}
	defer rows.Close()

	return scanInvoices(rows)
}

func scanInvoices(rows *sql.Rows) ([]Invoice, error) {
	invoices := []Invoice{}
	for rows.Next() {
		var inv Invoice
		var notes sql.NullString
		err := rows.Scan(
			&inv.ID,
			&inv.ProjectID,
			&inv.AccountID,
			&inv.InvoiceNumber,
			&inv.PeriodStart,
			&inv.PeriodEnd,
			&inv.Subtotal,
			&inv.Tax,
			&inv.Total,
			&inv.Status,
			&inv.StripeInvoiceID,
			&inv.PaidAt,
			&inv.CreatedAt,
			&inv.UpdatedAt,
			&inv.Source,
			&inv.Currency,
			&inv.DueDate,
			&inv.IssuedBy,
			&notes,
			&inv.BillToName,
			&inv.BillToEmail,
			&inv.BillToAddress,
			&inv.BillToTaxID,
			&inv.BillToAccount,
			&inv.PDFS3Key,
			&inv.PDFGeneratedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan invoice: %w", err)
		}
		inv.Notes = notes.String
		invoices = append(invoices, inv)
	}

	return invoices, rows.Err()
}

// UpdateInvoiceStatus updates the status of an invoice
func (s *Service) UpdateInvoiceStatus(ctx context.Context, invoiceID uuid.UUID, status string) error {
	query := `
		UPDATE billing.invoices
		SET status = $1, updated_at = NOW()
		WHERE id = $2
	`

	result, err := s.db.ExecContext(ctx, query, status, invoiceID)
	if err != nil {
		return fmt.Errorf("failed to update invoice status: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("invoice not found")
	}

	return nil
}

// MarkInvoicePaid marks an invoice as paid and settles it: it records the
// id of the Stripe object that settled it (a pi_… from a card charge, or
// the sentinel "credit" when credit covered the whole amount), clears any
// stale charge error, and — if this account has no OTHER unpaid open usage
// invoice — clears the account's grace clock. A successful collection
// cancelling the grace clock is symmetric with a good card doing so
// (MarkPaymentMethodVerified): the account is once again paying, so it
// should not be suspended.
//
// Idempotent-friendly: called from both the optimistic path in ChargeInvoice
// and the payment_intent.succeeded webhook; the second run simply re-sets
// the same values.
func (s *Service) MarkInvoicePaid(ctx context.Context, invoiceID uuid.UUID, stripeInvoiceID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var accountID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		UPDATE billing.invoices
		SET status = 'paid',
		    stripe_invoice_id = $1,
		    last_charge_error = NULL,
		    paid_at = NOW(),
		    updated_at = NOW()
		WHERE id = $2
		RETURNING account_id
	`, stripeInvoiceID, invoiceID).Scan(&accountID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("invoice not found")
	}
	if err != nil {
		return fmt.Errorf("failed to mark invoice as paid: %w", err)
	}

	// Clear the grace clock only when nothing else is owed: another open
	// usage invoice still failing must keep the countdown running.
	var otherUnpaid int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM billing.invoices
		WHERE account_id = $1 AND source = 'usage' AND status = 'open' AND id != $2
	`, accountID, invoiceID).Scan(&otherUnpaid); err != nil {
		return fmt.Errorf("failed to check other unpaid invoices: %w", err)
	}
	if otherUnpaid == 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth.accounts SET payment_failed_at = NULL, updated_at = NOW()
			WHERE id = $1 AND payment_failed_at IS NOT NULL
		`, accountID); err != nil {
			return fmt.Errorf("failed to clear grace clock: %w", err)
		}
	}

	return tx.Commit()
}
