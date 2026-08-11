// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

// Payment-method endpoints are ACCOUNT-scoped and use the JWT session,
// not a project API key — a card belongs to the account (the billing
// entity), the same reasoning as invoices. All are mounted under
// /v1/accounts/current/payment-methods.

// CreateSetupIntent begins adding a card: returns the client secret the
// browser hands to Stripe to confirm the card. The card is not usable
// until the webhook confirms it.
// POST /v1/accounts/current/payment-methods/setup-intent
func (h *BillingHandler) CreateSetupIntent(c *gin.Context) {
	accountID, ok := auth.GetAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account authentication required"})
		return
	}

	secret, err := h.billingService.CreateSetupIntent(c.Request.Context(), accountID)
	if err != nil {
		// Most likely payments not configured; a 503 tells the console the
		// feature is unavailable rather than the request being malformed.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"client_secret": secret})
}

// ListPaymentMethods returns the account's cards.
// GET /v1/accounts/current/payment-methods
func (h *BillingHandler) ListPaymentMethods(c *gin.Context) {
	accountID, ok := auth.GetAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account authentication required"})
		return
	}
	methods, err := h.billingService.ListPaymentMethods(c.Request.Context(), accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"payment_methods": methods, "count": len(methods)})
}

// RemovePaymentMethod removes a card. Refusing to remove the last
// verified card is a 409 — the account must never be left card-less.
// DELETE /v1/accounts/current/payment-methods/:id
func (h *BillingHandler) RemovePaymentMethod(c *gin.Context) {
	accountID, ok := auth.GetAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account authentication required"})
		return
	}
	pmID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payment method id"})
		return
	}

	err = h.billingService.RemovePaymentMethod(c.Request.Context(), accountID, pmID)
	if errors.Is(err, billing.ErrLastVerifiedCard) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"removed": true})
}

// SetDefaultPaymentMethod makes one verified card the default.
// POST /v1/accounts/current/payment-methods/:id/default
func (h *BillingHandler) SetDefaultPaymentMethod(c *gin.Context) {
	accountID, ok := auth.GetAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account authentication required"})
		return
	}
	pmID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payment method id"})
		return
	}
	if err := h.billingService.SetDefaultPaymentMethod(c.Request.Context(), accountID, pmID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"updated": true})
}

// GetCreditBalance returns the account's current credit balance, for the
// billing overview. Read-only; grants are operator-only (admin API).
// GET /v1/billing/credits
func (h *BillingHandler) GetCreditBalance(c *gin.Context) {
	accountID, ok := auth.GetAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account authentication required"})
		return
	}
	balance, err := h.billingService.CreditBalance(c.Request.Context(), accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"balance": balance})
}
