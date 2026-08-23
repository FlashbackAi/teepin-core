// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// BillingHandler handles billing-related requests
type BillingHandler struct {
	billingService *billing.Service
	authService    *auth.Service
}

// NewBillingHandler creates a new billing handler
func NewBillingHandler(billingService *billing.Service, authService *auth.Service) *BillingHandler {
	return &BillingHandler{
		billingService: billingService,
		authService:    authService,
	}
}

// GetPricing returns the platform's current per-resource rates. Public
// (any authenticated caller, not admin-only) — a customer can already see
// what they're paying on every invoice, so exposing the live rate table
// is transparency, not a leak. This is what the Kumbha MCP tool server's
// present_deployment_plan verb reads to build the itemized cost table a
// customer approves before real infrastructure is provisioned
// (KUMBHA-DESIGN.md's pre-deploy cost approval gate) — the same numbers
// GET /v1/admin/pricing shows an operator, never a separate estimate.
// GET /v1/billing/pricing
func (h *BillingHandler) GetPricing(c *gin.Context) {
	info, err := h.billingService.GetPricing(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, info)
}

// resolveProjectID resolves the project ID from context, query param, or user's first project
func (h *BillingHandler) resolveProjectID(c *gin.Context) (uuid.UUID, error) {
	// Try to get project from context (set by API key middleware)
	projectID, exists := auth.GetProjectID(c)
	if exists {
		return projectID, nil
	}

	// Try to get from query parameter
	projectIDStr := c.Query("project_id")
	if projectIDStr != "" {
		projectID, err := uuid.Parse(projectIDStr)
		if err != nil {
			return uuid.Nil, fmt.Errorf("invalid project_id format")
		}

		// Tenancy: the project must belong to the caller's account.
		// Without this an attacker could read another customer's
		// billing simply by passing their project_id.
		accountID, exists := auth.GetAccountID(c)
		if !exists {
			return uuid.Nil, fmt.Errorf("authentication required")
		}

		if _, err := h.authService.GetProject(c.Request.Context(), accountID, projectID); err != nil {
			// Deliberately indistinguishable from a nonexistent project.
			return uuid.Nil, fmt.Errorf("project not found")
		}

		return projectID, nil
	}

	// Fall back to the account's first project.
	accountID, exists := auth.GetAccountID(c)
	if !exists {
		return uuid.Nil, fmt.Errorf("project_id required (provide in query param or use API key)")
	}

	projects, err := h.authService.ListProjects(c.Request.Context(), accountID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to list projects: %w", err)
	}

	if len(projects) == 0 {
		return uuid.Nil, fmt.Errorf("no projects found - please create a project first")
	}

	// Return first project
	return projects[0].ID, nil
}

// GetUsageSummary returns usage summary for a project
// GET /v1/billing/usage
// Supports project resolution via: API key context, ?project_id=xxx, or user's first project
func (h *BillingHandler) GetUsageSummary(c *gin.Context) {
	projectID, err := h.resolveProjectID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Parse date range
	startStr := c.Query("start")
	endStr := c.Query("end")

	var start, end time.Time

	if startStr != "" {
		start, err = time.Parse(time.RFC3339, startStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start date format (use RFC3339)"})
			return
		}
	} else {
		// Default: start of current month
		now := time.Now()
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}

	if endStr != "" {
		end, err = time.Parse(time.RFC3339, endStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid end date format (use RFC3339)"})
			return
		}
	} else {
		// Default: now
		end = time.Now()
	}

	summary, err := h.billingService.GetUsageSummary(c.Request.Context(), projectID, start, end)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, summary)
}

// GetUsageRecords returns detailed usage records
// GET /v1/billing/usage/records
// Supports project resolution via: API key context, ?project_id=xxx, or user's first project
func (h *BillingHandler) GetUsageRecords(c *gin.Context) {
	projectID, err := h.resolveProjectID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Parse date range (same as GetUsageSummary)
	startStr := c.Query("start")
	endStr := c.Query("end")

	var start, end time.Time

	if startStr != "" {
		start, err = time.Parse(time.RFC3339, startStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start date format"})
			return
		}
	} else {
		now := time.Now()
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}

	if endStr != "" {
		end, err = time.Parse(time.RFC3339, endStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid end date format"})
			return
		}
	} else {
		end = time.Now()
	}

	records, err := h.billingService.GetUsageRecords(c.Request.Context(), projectID, start, end)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"records": records,
		"count":   len(records),
	})
}

// ListInvoices lists every invoice the caller's ACCOUNT owns —
// project-anchored usage invoices and account-level manual invoices
// together. Account-scoped rather than project-scoped since migration
// 012: a manual invoice covering several projects (or none) has no
// single project to filter by, and a customer thinks in terms of "what
// do I owe", not "what does this one project owe".
//
// GET /v1/billing/invoices
// Works with either a JWT or a project API key — both carry an
// account_id (ValidateAPIKey resolves it from the key's project), so
// either credential resolves the account this lists invoices for.
func (h *BillingHandler) ListInvoices(c *gin.Context) {
	accountID, ok := auth.GetAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account authentication required"})
		return
	}

	// ...ForCustomer, not ListAccountInvoices: a draft is an operator's
	// in-progress document (see INVOICE-DESIGN.md "Lifecycle" — creation
	// and issuing are deliberately separate so a mistyped amount is
	// correctable BEFORE a customer holds a copy). The control centre
	// uses the unfiltered ListAccountInvoices; this is the customer path.
	invoices, err := h.billingService.ListAccountInvoicesForCustomer(c.Request.Context(), accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"invoices": invoices,
		"count":    len(invoices),
	})
}

// GetInvoice retrieves a specific invoice
// GET /v1/billing/invoices/:id
func (h *BillingHandler) GetInvoice(c *gin.Context) {
	invoiceIDStr := c.Param("id")
	invoiceID, err := uuid.Parse(invoiceIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid invoice ID"})
		return
	}

	invoice, err := h.billingService.GetInvoice(c.Request.Context(), invoiceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "invoice not found"})
		return
	}

	// Tenancy check is now ACCOUNT-based, not project-based: an invoice
	// may be account-level (ProjectID nil — a manual invoice, or one
	// covering several projects) and still belongs to exactly one
	// account. Checking project_id here would make an account-level
	// invoice unreachable through this endpoint for every legitimate
	// caller, while checking account_id covers both cases correctly.
	//
	// 404 rather than 403: confirming an invoice ID exists for another
	// customer's account leaks information a billing endpoint must not
	// leak.
	accountID, ok := auth.GetAccountID(c)
	if !ok || invoice.AccountID != accountID {
		c.JSON(http.StatusNotFound, gin.H{"error": "invoice not found"})
		return
	}

	// Same reasoning as ListInvoices: a draft is not yet a fact the
	// customer should see, even by direct ID — an operator may have
	// shared a link, or a customer may have seen the ID in a webhook
	// payload before the invoice was ready. 404, not 403, for the same
	// non-leaking reason as the account check above.
	if invoice.Status == "draft" {
		c.JSON(http.StatusNotFound, gin.H{"error": "invoice not found"})
		return
	}

	c.JSON(http.StatusOK, invoice)
}

// DownloadInvoicePDF returns a short-lived presigned URL for the
// invoice's stored PDF document.
// GET /v1/billing/invoices/:id/pdf  →  {"url": "https://...s3..."}
//
// Every failure below returns 404, never 403 — the same non-leaking
// discipline as GetInvoice. A customer of another account, a draft, and
// an invoice whose document has not been generated are all
// indistinguishable from "no such invoice".
//
// The endpoint hands back the presigned URL as JSON rather than a 302,
// and the client NAVIGATES to it (a link click). It deliberately does
// NOT redirect, because the console fetches this endpoint with the auth
// token, and a fetch that follows a redirect into S3 is a cross-origin
// fetch — which S3 blocks with no Access-Control-Allow-Origin. Handing
// back the URL lets the browser navigate to it instead (navigation is
// not subject to CORS), which is the intended use of a presigned URL.
// The API still never proxies the bytes, and the bucket stays private.
func (h *BillingHandler) DownloadInvoicePDF(c *gin.Context) {
	invoiceID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid invoice ID"})
		return
	}

	invoice, err := h.billingService.GetInvoice(c.Request.Context(), invoiceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "invoice not found"})
		return
	}

	// Account tenancy + draft checks, identical to GetInvoice.
	accountID, ok := auth.GetAccountID(c)
	if !ok || invoice.AccountID != accountID {
		c.JSON(http.StatusNotFound, gin.H{"error": "invoice not found"})
		return
	}
	if invoice.Status == "draft" {
		c.JSON(http.StatusNotFound, gin.H{"error": "invoice not found"})
		return
	}

	// No stored document yet — issued before PDF storage existed, or a
	// generation that failed and has not been backfilled. Distinct
	// message so the console can say "not yet available" rather than
	// implying the invoice itself is missing, but still a 404.
	if !invoice.PDFAvailable() {
		c.JSON(http.StatusNotFound, gin.H{"error": "invoice document not yet available"})
		return
	}

	// Short TTL: a download is immediate, and a URL that outlives the
	// click is a URL that can be forwarded.
	url, err := h.billingService.PresignInvoicePDF(c.Request.Context(), invoice, 5*time.Minute)
	if err != nil {
		// Storage unavailable (not configured) or a presign failure —
		// same 404 to the customer, logged server-side for the operator.
		c.JSON(http.StatusNotFound, gin.H{"error": "invoice document not yet available"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"url": url})
}

// CreateInvoice generates a new invoice for a period
// POST /v1/billing/invoices
// Supports project resolution via: API key context, ?project_id=xxx, or user's first project
func (h *BillingHandler) CreateInvoice(c *gin.Context) {
	projectID, err := h.resolveProjectID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var req struct {
		PeriodStart string `json:"period_start" binding:"required"`
		PeriodEnd   string `json:"period_end" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	periodStart, err := time.Parse(time.RFC3339, req.PeriodStart)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid period_start format"})
		return
	}

	periodEnd, err := time.Parse(time.RFC3339, req.PeriodEnd)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid period_end format"})
		return
	}

	invoice, err := h.billingService.CreateInvoice(c.Request.Context(), projectID, periodStart, periodEnd)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, invoice)
}

// GetCurrentMonthUsage returns current month usage (convenience endpoint)
// GET /v1/billing/current-month
// Supports project resolution via: API key context, ?project_id=xxx, or user's first project
func (h *BillingHandler) GetCurrentMonthUsage(c *gin.Context) {
	projectID, err := h.resolveProjectID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get current month start and end
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := now

	summary, err := h.billingService.GetUsageSummary(c.Request.Context(), projectID, start, end)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"summary":         summary,
		"period_start":    start,
		"period_end":      end,
		"days_in_month":   now.Day(),
		"projected_total": summary.TotalCost / float64(now.Day()) * float64(daysInMonth(now)),
	})
}

// daysInMonth returns the number of days in a given month
func daysInMonth(t time.Time) int {
	return time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// GetAccountSummary returns the account bill for a period, broken down
// project → service. Defaults to month-to-date, which is what the
// console dashboard shows.
// GET /v1/billing/summary?start=&end=
func (h *BillingHandler) GetAccountSummary(c *gin.Context) {
	accountID, exists := auth.GetAccountID(c)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	start, end := billing.CurrentMonthRange(time.Now())
	if v := c.Query("start"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start (expected RFC3339)"})
			return
		}
		start = parsed
	}
	if v := c.Query("end"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid end (expected RFC3339)"})
			return
		}
		end = parsed
	}
	if end.Before(start) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "end must be after start"})
		return
	}

	summary, err := h.billingService.GetAccountSummary(c.Request.Context(), accountID, start, end)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, summary)
}
