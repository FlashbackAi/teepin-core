// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/gpu"
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
		(project_id, instance_id, resource_type, quantity, unit, unit_price, total_cost, start_time, end_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at
	`

	err := s.db.QueryRowContext(
		ctx, query,
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
// platform's linear pricing ($0.10 per GB-hour). This is model-agnostic
// and covers every instance type, including custom sizes
// (gpu.h100.custom-25gb) that a static rate table cannot enumerate.
func (s *Service) CalculateVRAMCost(vramGB int, hours float64) float64 {
	return float64(vramGB) * gpu.PricePerGBHour * hours
}

// VRAMUnitPrice returns the hourly rate for a VRAM allocation.
func (s *Service) VRAMUnitPrice(vramGB int) float64 {
	return float64(vramGB) * gpu.PricePerGBHour
}

// CreateInvoice generates an invoice for a billing period
func (s *Service) CreateInvoice(ctx context.Context, projectID uuid.UUID, periodStart, periodEnd time.Time) (*Invoice, error) {
	// Get usage summary for the period
	summary, err := s.GetUsageSummary(ctx, projectID, periodStart, periodEnd)
	if err != nil {
		return nil, fmt.Errorf("failed to get usage summary: %w", err)
	}

	// Generate invoice number (format: INV-YYYYMM-XXXXX)
	invoiceNumber := fmt.Sprintf("INV-%s-%s",
		periodStart.Format("200601"),
		uuid.New().String()[:8],
	)

	subtotal := summary.TotalCost
	tax := subtotal * 0.0 // No tax for now
	total := subtotal + tax

	invoice := &Invoice{
		ProjectID:     projectID,
		InvoiceNumber: invoiceNumber,
		PeriodStart:   periodStart,
		PeriodEnd:     periodEnd,
		Subtotal:      subtotal,
		Tax:           tax,
		Total:         total,
		Status:        "draft",
	}

	query := `
		INSERT INTO billing.invoices
		(project_id, invoice_number, period_start, period_end, subtotal, tax, total, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at, updated_at
	`

	err = s.db.QueryRowContext(
		ctx, query,
		invoice.ProjectID,
		invoice.InvoiceNumber,
		invoice.PeriodStart,
		invoice.PeriodEnd,
		invoice.Subtotal,
		invoice.Tax,
		invoice.Total,
		invoice.Status,
	).Scan(&invoice.ID, &invoice.CreatedAt, &invoice.UpdatedAt)

	if err != nil {
		return nil, fmt.Errorf("failed to create invoice: %w", err)
	}

	return invoice, nil
}

// GetInvoice retrieves an invoice by ID
func (s *Service) GetInvoice(ctx context.Context, invoiceID uuid.UUID) (*Invoice, error) {
	query := `
		SELECT id, project_id, invoice_number, period_start, period_end,
		       subtotal, tax, total, status, stripe_invoice_id, paid_at,
		       created_at, updated_at
		FROM billing.invoices
		WHERE id = $1
	`

	var invoice Invoice
	err := s.db.QueryRowContext(ctx, query, invoiceID).Scan(
		&invoice.ID,
		&invoice.ProjectID,
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
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("invoice not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get invoice: %w", err)
	}

	return &invoice, nil
}

// ListInvoices lists all invoices for a project
func (s *Service) ListInvoices(ctx context.Context, projectID uuid.UUID) ([]Invoice, error) {
	query := `
		SELECT id, project_id, invoice_number, period_start, period_end,
		       subtotal, tax, total, status, stripe_invoice_id, paid_at,
		       created_at, updated_at
		FROM billing.invoices
		WHERE project_id = $1
		ORDER BY period_start DESC
	`

	rows, err := s.db.QueryContext(ctx, query, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to list invoices: %w", err)
	}
	defer rows.Close()

	var invoices []Invoice
	for rows.Next() {
		var inv Invoice
		err := rows.Scan(
			&inv.ID,
			&inv.ProjectID,
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
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan invoice: %w", err)
		}
		invoices = append(invoices, inv)
	}

	return invoices, nil
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
