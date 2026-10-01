// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

// GetCryptoTopUpConfig tells the console whether USDC payments are offered here
// and within what limits, so it shows the option only when it will work.
// GET /v1/billing/credits/crypto
func (h *BillingHandler) GetCryptoTopUpConfig(c *gin.Context) {
	if _, ok := auth.GetAccountID(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account authentication required"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled":    h.billingService.CryptoEnabled(),
		"min_amount": billing.MinTopUp,
		"max_amount": billing.MaxCryptoTopUp,
	})
}

// CreateCryptoTopUp starts a USDC purchase and returns the payment request
// (Solana Pay link) for the browser to show. Credit is added only after the
// payment is finalized on chain and verified, never by this call.
// POST /v1/billing/credits/crypto-topups
func (h *BillingHandler) CreateCryptoTopUp(c *gin.Context) {
	accountID, ok := requireSignedInUser(c)
	if !ok {
		return
	}
	var req createTopUpRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": billing.ErrCryptoAmount.Error()})
		return
	}

	intent, err := h.billingService.CreateCryptoTopUp(c.Request.Context(), accountID, req.Amount)
	switch {
	case errors.Is(err, billing.ErrCryptoAmount):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, billing.ErrTooManyPending):
		c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
	case errors.Is(err, billing.ErrAccountClosed):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	case errors.Is(err, billing.ErrCryptoNotConfigured):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
	case err != nil:
		log.Printf("WARN: create USDC top-up for account %s: %v", accountID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not start the payment, please try again"})
	default:
		c.JSON(http.StatusCreated, intent)
	}
}
