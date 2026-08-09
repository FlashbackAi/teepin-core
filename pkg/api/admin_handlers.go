// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

// AdminHandler serves the platform-operator API (pricing management).
// It is only registered when ADMIN_API_TOKEN is configured; the web
// console's admin UI will call these same endpoints.
type AdminHandler struct {
	billingService *billing.Service
	// authService backs the account/project lookups the invoicing UI
	// needs. May be nil when the platform runs without a database.
	authService *auth.Service
	token       string
}

// NewAdminHandler creates the admin API handler. token must be
// non-empty — main refuses to register admin routes without one.
func NewAdminHandler(billingService *billing.Service, authService *auth.Service, token string) *AdminHandler {
	return &AdminHandler{
		billingService: billingService,
		authService:    authService,
		token:          token,
	}
}

// RequireAdminToken authenticates requests with the operator token
// (Authorization: Bearer <ADMIN_API_TOKEN>). Constant-time comparison;
// admin auth is deliberately separate from customer JWT/API-key auth.
func (h *AdminHandler) RequireAdminToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		presented, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(h.token)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "admin authentication required"})
			return
		}
		c.Next()
	}
}

// GetPricing returns the live platform pricing configuration.
// GET /v1/admin/pricing
func (h *AdminHandler) GetPricing(c *gin.Context) {
	info, err := h.billingService.GetPricing(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, info)
}

// UpdatePricing sets the platform GPU VRAM rate. Applies to the next
// allocation and billing tick immediately; existing usage records keep
// the rate they were metered at.
// PUT /v1/admin/pricing
func (h *AdminHandler) UpdatePricing(c *gin.Context) {
	var req struct {
		VRAMPricePerGBHour float64 `json:"vram_price_per_gb_hour" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.billingService.SetVRAMPricePerGBHour(c.Request.Context(), req.VRAMPricePerGBHour, "admin-api"); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	info, err := h.billingService.GetPricing(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, info)
}

// ---------------------------------------------------------------------
// Manual invoicing
//
// For negotiated prices, setup fees, support retainers and credits —
// the billing that exists before, alongside, or instead of metered
// usage. Admin-only: these endpoints create financial documents, and the
// operator token is deliberately separate from customer auth so a
// compromised customer session can never reach them.
// ---------------------------------------------------------------------

// ListAccounts returns every account, for choosing who to invoice.
// GET /v1/admin/accounts
func (h *AdminHandler) ListAccounts(c *gin.Context) {
	if h.authService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service unavailable"})
		return
	}

	accounts, err := h.authService.ListAllAccounts(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"accounts": accounts, "count": len(accounts)})
}

// ListAccountProjects returns one account's projects.
// GET /v1/admin/accounts/:account_id/projects
func (h *AdminHandler) ListAccountProjects(c *gin.Context) {
	if h.authService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service unavailable"})
		return
	}

	accountID, err := uuid.Parse(c.Param("account_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account id"})
		return
	}

	projects, err := h.authService.ListProjects(c.Request.Context(), accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"projects": projects, "count": len(projects)})
}

// CreateManualInvoice issues an invoice with explicit line items.
// POST /v1/admin/invoices
//
// The total is computed from the lines and never accepted from the
// caller: an invoice whose total disagrees with its own breakdown is the
// one thing a customer will always notice.
func (h *AdminHandler) CreateManualInvoice(c *gin.Context) {
	var req struct {
		AccountID   string  `json:"account_id" binding:"required"`
		PeriodStart string  `json:"period_start" binding:"required"`
		PeriodEnd   string  `json:"period_end" binding:"required"`
		DueDate     string  `json:"due_date"`
		Currency    string  `json:"currency"`
		Notes       string  `json:"notes"`
		LineItems   []struct {
			Description string  `json:"description" binding:"required"`
			// ProjectID is optional per line: most charges are attributed
			// to the project that incurred them, but an account-wide
			// charge (platform fee, setup cost, credit) is not tied to
			// any one project.
			ProjectID string  `json:"project_id"`
			Quantity  float64 `json:"quantity"`
			Unit      string  `json:"unit"`
			UnitPrice float64 `json:"unit_price"`
			Amount    float64 `json:"amount" binding:"required"`
		} `json:"line_items" binding:"required,min=1"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	accountID, err := uuid.Parse(req.AccountID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account_id"})
		return
	}

	periodStart, err := time.Parse("2006-01-02", req.PeriodStart)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "period_start must be YYYY-MM-DD"})
		return
	}
	periodEnd, err := time.Parse("2006-01-02", req.PeriodEnd)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "period_end must be YYYY-MM-DD"})
		return
	}
	if periodEnd.Before(periodStart) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "period_end is before period_start"})
		return
	}

	var dueDate *time.Time
	if req.DueDate != "" {
		parsed, err := time.Parse("2006-01-02", req.DueDate)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "due_date must be YYYY-MM-DD"})
			return
		}
		dueDate = &parsed
	}

	items := make([]billing.InvoiceLineItem, 0, len(req.LineItems))
	for i, item := range req.LineItems {
		var lineProjectID *uuid.UUID
		if item.ProjectID != "" {
			parsed, err := uuid.Parse(item.ProjectID)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{
					"error": fmt.Sprintf("line item %d: invalid project_id", i+1),
				})
				return
			}
			lineProjectID = &parsed
		}

		items = append(items, billing.InvoiceLineItem{
			ProjectID:   lineProjectID,
			Description: item.Description,
			Quantity:    item.Quantity,
			Unit:        item.Unit,
			UnitPrice:   item.UnitPrice,
			Amount:      item.Amount,
		})
	}

	invoice, err := h.billingService.CreateManualInvoice(c.Request.Context(),
		billing.ManualInvoiceRequest{
			AccountID:   accountID,
			PeriodStart: periodStart,
			PeriodEnd:   periodEnd,
			DueDate:     dueDate,
			Currency:    req.Currency,
			Notes:       req.Notes,
			LineItems:   items,
		})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, invoice)
}

// IssueInvoice moves a draft to open, making it payable.
// POST /v1/admin/invoices/:id/issue
func (h *AdminHandler) IssueInvoice(c *gin.Context) {
	invoiceID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid invoice id"})
		return
	}

	invoice, err := h.billingService.IssueInvoice(c.Request.Context(), invoiceID)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, invoice)
}

// VoidInvoice cancels a draft or open invoice.
// POST /v1/admin/invoices/:id/void
func (h *AdminHandler) VoidInvoice(c *gin.Context) {
	invoiceID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid invoice id"})
		return
	}

	if err := h.billingService.VoidInvoice(c.Request.Context(), invoiceID); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "invoice voided", "id": invoiceID})
}

// GetInvoice returns one invoice with its line items.
// GET /v1/admin/invoices/:id
func (h *AdminHandler) GetInvoice(c *gin.Context) {
	invoiceID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid invoice id"})
		return
	}

	invoice, err := h.billingService.GetInvoice(c.Request.Context(), invoiceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "invoice not found"})
		return
	}
	c.JSON(http.StatusOK, invoice)
}

// ListProjectInvoices returns project-anchored invoices only (the
// pre-account-level usage path). Kept for that narrower case; an
// operator wanting everything an account owes should use
// ListAccountInvoices instead — see the ListInvoices doc comment.
// GET /v1/admin/projects/:project_id/invoices
func (h *AdminHandler) ListProjectInvoices(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("project_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	invoices, err := h.billingService.ListInvoices(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"invoices": invoices, "count": len(invoices)})
}

// ListAccountInvoices returns every invoice an account owns —
// project-anchored and account-level (manual) invoices together. This is
// the control centre's primary invoice list: an operator manages
// billing per account, the same way a customer sees one combined bill.
// GET /v1/admin/accounts/:account_id/invoices
func (h *AdminHandler) ListAccountInvoices(c *gin.Context) {
	accountID, err := uuid.Parse(c.Param("account_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account id"})
		return
	}

	invoices, err := h.billingService.ListAccountInvoices(c.Request.Context(), accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"invoices": invoices, "count": len(invoices)})
}
