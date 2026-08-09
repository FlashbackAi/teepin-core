// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Service handles billing and usage tracking
type Service struct {
	db      *sql.DB
	pricing *ResourcePricing
}

// NewService creates a new billing service
func NewService(db *sql.DB) *Service {
	return &Service{
		db:      db,
		pricing: DefaultPricing(),
	}
}

// RecordUsage records a usage event for billing
func (s *Service) RecordUsage(ctx context.Context, record *UsageRecord) error {
	query := `
		INSERT INTO billing.usage_records
		(account_id, project_id, instance_id, resource_type, quantity, unit,
		 unit_price, total_cost, start_time, end_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, created_at
	`

	err := s.db.QueryRowContext(
		ctx, query,
		record.AccountID,
		record.ProjectID,
		record.InstanceID,
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
	query := `
		SELECT id, project_id, instance_id, resource_type, quantity, unit,
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
			&r.ID, &r.ProjectID, &r.InstanceID, &r.ResourceType,
			&r.Quantity, &r.Unit, &r.UnitPrice, &r.TotalCost,
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
			instance_id,
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
	return s.GetInvoice(ctx, invoiceID)
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
	COALESCE(bill_to_account_number,'')`

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

// ListAccountInvoices lists every invoice belonging to an account —
// both project-anchored (usage) and account-level (manual) invoices.
// This is the list a customer or operator actually wants: "everything
// this account owes", the same way one AWS account has one bill list
// regardless of how many services appear on it.
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

// MarkInvoicePaid marks an invoice as paid
func (s *Service) MarkInvoicePaid(ctx context.Context, invoiceID uuid.UUID, stripeInvoiceID string) error {
	query := `
		UPDATE billing.invoices
		SET status = 'paid',
		    stripe_invoice_id = $1,
		    paid_at = NOW(),
		    updated_at = NOW()
		WHERE id = $2
	`

	result, err := s.db.ExecContext(ctx, query, stripeInvoiceID, invoiceID)
	if err != nil {
		return fmt.Errorf("failed to mark invoice as paid: %w", err)
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
