// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"time"

	"github.com/google/uuid"
)

// UsageRecord represents a metered usage record
type UsageRecord struct {
	ID           uuid.UUID `json:"id"`
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
	ID              uuid.UUID  `json:"id"`
	ProjectID       uuid.UUID  `json:"project_id"`
	InvoiceNumber   string     `json:"invoice_number"`
	PeriodStart     time.Time  `json:"period_start"`
	PeriodEnd       time.Time  `json:"period_end"`
	Subtotal        float64    `json:"subtotal"`
	Tax             float64    `json:"tax"`
	Total           float64    `json:"total"`
	Status          string     `json:"status"` // draft, open, paid, void
	StripeInvoiceID *string    `json:"stripe_invoice_id,omitempty"`
	PaidAt          *time.Time `json:"paid_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// InvoiceLineItem represents a line item in an invoice
type InvoiceLineItem struct {
	ResourceType string  `json:"resource_type"`
	Description  string  `json:"description"`
	Quantity     float64 `json:"quantity"`
	Unit         string  `json:"unit"`
	UnitPrice    float64 `json:"unit_price"`
	TotalCost    float64 `json:"total_cost"`
}

// PaymentMethod represents a stored payment method
type PaymentMethod struct {
	ID                     uuid.UUID `json:"id"`
	ProjectID              uuid.UUID `json:"project_id"`
	StripeCustomerID       string    `json:"stripe_customer_id"`
	StripePaymentMethodID  string    `json:"stripe_payment_method_id"`
	Type                   string    `json:"type"` // card, bank_account
	Last4                  *string   `json:"last4,omitempty"`
	Brand                  *string   `json:"brand,omitempty"`
	ExpMonth               *int      `json:"exp_month,omitempty"`
	ExpYear                *int      `json:"exp_year,omitempty"`
	IsDefault              bool      `json:"is_default"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// ResourcePricing defines pricing for different resource types
type ResourcePricing struct {
	GPU10GB  float64 // H100 MIG 10GB per hour
	GPU20GB  float64 // H100 MIG 20GB per hour
	GPU40GB  float64 // H100 MIG 40GB per hour
	GPU80GB  float64 // H100 Full GPU per hour
	CPUCore  float64 // Per vCPU per hour
	MemoryGB float64 // Per GB RAM per hour
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
