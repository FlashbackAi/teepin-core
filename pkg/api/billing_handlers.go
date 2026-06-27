// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
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

		// Verify user has access to this project
		userID, exists := auth.GetUserID(c)
		if !exists {
			return uuid.Nil, fmt.Errorf("authentication required")
		}

		project, err := h.authService.GetProject(c.Request.Context(), projectID)
		if err != nil {
			return uuid.Nil, fmt.Errorf("project not found")
		}

		if project.OwnerID != userID {
			return uuid.Nil, fmt.Errorf("access denied to project")
		}

		return projectID, nil
	}

	// Fall back to user's first project
	userID, exists := auth.GetUserID(c)
	if !exists {
		return uuid.Nil, fmt.Errorf("project_id required (provide in query param or use API key)")
	}

	projects, err := h.authService.ListProjects(c.Request.Context(), userID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to get user projects: %w", err)
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

// ListInvoices lists all invoices for a project
// GET /v1/billing/invoices
// Supports project resolution via: API key context, ?project_id=xxx, or user's first project
func (h *BillingHandler) ListInvoices(c *gin.Context) {
	projectID, err := h.resolveProjectID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	invoices, err := h.billingService.ListInvoices(c.Request.Context(), projectID)
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

	// Verify user has access to this invoice's project
	projectID, _ := auth.GetProjectID(c)
	if invoice.ProjectID != projectID {
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
		return
	}

	c.JSON(http.StatusOK, invoice)
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
		"summary":      summary,
		"period_start": start,
		"period_end":   end,
		"days_in_month": now.Day(),
		"projected_total": summary.TotalCost / float64(now.Day()) * float64(daysInMonth(now)),
	})
}

// daysInMonth returns the number of days in a given month
func daysInMonth(t time.Time) int {
	return time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
