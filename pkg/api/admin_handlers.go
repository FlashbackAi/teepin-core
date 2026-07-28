// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

// AdminHandler serves the platform-operator API (pricing management).
// It is only registered when ADMIN_API_TOKEN is configured; the web
// console's admin UI will call these same endpoints.
type AdminHandler struct {
	billingService *billing.Service
	token          string
}

// NewAdminHandler creates the admin API handler. token must be
// non-empty — main refuses to register admin routes without one.
func NewAdminHandler(billingService *billing.Service, token string) *AdminHandler {
	return &AdminHandler{billingService: billingService, token: token}
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
