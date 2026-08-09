// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"time"

	"github.com/google/uuid"
)

// UsageRecord represents a metered usage record
type UsageRecord struct {
	ID uuid.UUID `json:"id"`
	// AccountID is the billed tenant. NOT NULL in the schema: a usage
	// record with no account cannot be invoiced, so the insert must
	// always carry it.
	AccountID    uuid.UUID `json:"account_id"`
	ProjectID    uuid.UUID `json:"project_id"`
	InstanceID   string    `json:"instance_id"`
	ResourceType string    `json:"resource_type"`
	Quantity     float64   `json:"quantity"`
	Unit         string    `json:"unit"`
	UnitPrice    float64   `json:"unit_price"`
	TotalCost    float64   `json:"total_cost"`
	StartTime    time.Time `json:"start_time"`
	EndTime      time.Time `json:"end_time"`
	CreatedAt    time.Time `json:"created_at"`
}

// Invoice represents a billing invoice
type Invoice struct {
	ID uuid.UUID `json:"id"`
	// AccountID is the BILLING ENTITY — who owes the money, and the
	// primary owner of every invoice. An invoice covers an account's
	// activity across however many projects it has, the same way one
	// AWS bill covers every service under one account rather than a
	// separate bill per service.
	AccountID uuid.UUID `json:"account_id"`
	// ProjectID is set ONLY for an invoice generated against a single
	// project (the pre-account-level usage path). Account-level and
	// manual invoices leave this nil and carry their breakdown on
	// individual LineItems instead — see InvoiceLineItem.ProjectID.
	ProjectID       *uuid.UUID `json:"project_id,omitempty"`
	InvoiceNumber   string     `json:"invoice_number"`
	PeriodStart     time.Time  `json:"period_start"`
	PeriodEnd       time.Time  `json:"period_end"`
	Subtotal        float64    `json:"subtotal"`
	Tax             float64    `json:"tax"`
	Total           float64    `json:"total"`
	Status          string     `json:"status"` // draft, open, paid, void, uncollectible
	StripeInvoiceID *string    `json:"stripe_invoice_id,omitempty"`
	PaidAt          *time.Time `json:"paid_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`

	// Source distinguishes an invoice a human issued ("manual") from one
	// the meter produced ("usage"). Automated billing must never modify
	// or supersede a negotiated invoice.
	Source string `json:"source"`
	// Currency the invoice was ISSUED in. Stored per-invoice because a
	// financial record must not be reinterpreted later through a global
	// setting that has since changed.
	Currency string     `json:"currency"`
	DueDate  *time.Time `json:"due_date,omitempty"`
	IssuedBy *uuid.UUID `json:"issued_by,omitempty"`
	Notes    string     `json:"notes,omitempty"`

	// Bill-to details SNAPSHOTTED at issue time. A customer who renames
	// their company next year must not retroactively alter invoices
	// already sent — that is what makes an invoice a record rather than
	// a rendering of current state.
	BillToName    string `json:"bill_to_name,omitempty"`
	BillToEmail   string `json:"bill_to_email,omitempty"`
	BillToAddress string `json:"bill_to_address,omitempty"`
	BillToTaxID   string `json:"bill_to_tax_id,omitempty"`
	BillToAccount string `json:"bill_to_account_number,omitempty"`

	// LineItems is populated by GetInvoice, not by list queries — a list
	// of fifty invoices does not need every line of every one.
	LineItems []InvoiceLineItem `json:"line_items,omitempty"`
}

// InvoiceLineItem is one line on an invoice.
//
// Used both for persisted lines (manual invoices) and for the computed
// breakdown of a usage invoice, which is why Amount is authoritative
// rather than derived: a negotiated flat price or a credit has an amount
// with no meaningful quantity.
type InvoiceLineItem struct {
	ID          uuid.UUID `json:"id,omitempty"`
	Description string    `json:"description"`

	// ProjectID attributes this line to one project, for the
	// project-level breakdown a customer sees the same way an AWS bill
	// breaks down by service. Nil for account-level lines that are not
	// tied to any one project's usage — a platform fee, a one-time setup
	// charge, a credit.
	ProjectID *uuid.UUID `json:"project_id,omitempty"`
	// ProjectName is populated on read only, for display — never
	// written. Denormalising the name onto the line item itself would
	// let it drift from the project's actual current name; joining at
	// read time keeps exactly one place that name is stored.
	ProjectName string `json:"project_name,omitempty"`

	// Quantity/Unit/UnitPrice are optional context — "120.5 GPU-hours
	// at $1.00" — and may be absent on a flat-price line.
	Quantity  float64 `json:"quantity,omitempty"`
	Unit      string  `json:"unit,omitempty"`
	UnitPrice float64 `json:"unit_price,omitempty"`
	// Amount may be NEGATIVE: credits and refunds are line items, not a
	// separate concept.
	Amount    float64 `json:"amount"`
	SortOrder int     `json:"sort_order"`

	// ResourceType is set on computed usage breakdowns only.
	ResourceType string `json:"resource_type,omitempty"`
}

// PaymentMethod represents a stored payment method
type PaymentMethod struct {
	ID                    uuid.UUID `json:"id"`
	ProjectID             uuid.UUID `json:"project_id"`
	StripeCustomerID      string    `json:"stripe_customer_id"`
	StripePaymentMethodID string    `json:"stripe_payment_method_id"`
	Type                  string    `json:"type"` // card, bank_account
	Last4                 *string   `json:"last4,omitempty"`
	Brand                 *string   `json:"brand,omitempty"`
	ExpMonth              *int      `json:"exp_month,omitempty"`
	ExpYear               *int      `json:"exp_year,omitempty"`
	IsDefault             bool      `json:"is_default"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// ResourcePricing defines pricing for different resource types
type ResourcePricing struct {
	GPU10GB   float64 // H100 MIG 10GB per hour
	GPU20GB   float64 // H100 MIG 20GB per hour
	GPU40GB   float64 // H100 MIG 40GB per hour
	GPU80GB   float64 // H100 Full GPU per hour
	CPUCore   float64 // Per vCPU per hour
	MemoryGB  float64 // Per GB RAM per hour
	StorageGB float64 // Per GB storage per month
}

// DefaultPricing returns the default pricing model
func DefaultPricing() *ResourcePricing {
	return &ResourcePricing{
		GPU10GB:   1.00, // $1/hour for 10GB VRAM
		GPU20GB:   2.00, // $2/hour for 20GB VRAM
		GPU40GB:   4.00, // $4/hour for 40GB VRAM
		GPU80GB:   8.00, // $8/hour for 80GB VRAM
		CPUCore:   0.05, // $0.05/hour per vCPU
		MemoryGB:  0.01, // $0.01/hour per GB RAM
		StorageGB: 0.10, // $0.10/month per GB storage
	}
}

// UsageSummary provides aggregated usage for a project
type UsageSummary struct {
	ProjectID   uuid.UUID          `json:"project_id"`
	PeriodStart time.Time          `json:"period_start"`
	PeriodEnd   time.Time          `json:"period_end"`
	TotalCost   float64            `json:"total_cost"`
	ByResource  map[string]float64 `json:"by_resource"` // resource_type -> cost
	ByInstance  map[string]float64 `json:"by_instance"` // instance_id -> cost
}
