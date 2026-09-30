// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"errors"
	"log"
	"net/http"
	"time"

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

	secret, paymentMethodID, err := h.billingService.CreateSetupIntent(c.Request.Context(), accountID)
	if err != nil {
		// Most likely payments not configured; a 503 tells the console the
		// feature is unavailable rather than the request being malformed.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	// payment_method_id lets the console remove this pending row if the
	// customer never completes the flow (cancels, or the Payment Element
	// fails to load) — otherwise it is a permanent orphaned "Validating…"
	// card with nothing that can ever clean it up.
	c.JSON(http.StatusOK, gin.H{"client_secret": secret, "payment_method_id": paymentMethodID})
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

// RemovePaymentMethod removes a card, including the last one (prepaid:
// no card is required to keep running).
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

// requireSignedInUser rejects API-key and agent-session credentials:
// buying credit spends a customer's money, so only a person signed in to
// the console may start it. Writes the response and returns false when
// the caller is not allowed.
func requireSignedInUser(c *gin.Context) (uuid.UUID, bool) {
	accountID, ok := auth.GetAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account authentication required"})
		return uuid.Nil, false
	}
	if _, viaAPIKey := auth.GetAPIKeyScopes(c); viaAPIKey {
		c.JSON(http.StatusForbidden, gin.H{"error": "credit can only be purchased by a signed-in user"})
		return uuid.Nil, false
	}
	if _, isSession := auth.GetSessionID(c); isSession {
		c.JSON(http.StatusForbidden, gin.H{"error": "credit can only be purchased by a signed-in user"})
		return uuid.Nil, false
	}
	return accountID, true
}

type createTopUpRequest struct {
	// Amount in USD. Whole cents, between billing.MinTopUp and
	// billing.MaxTopUp.
	Amount float64 `json:"amount" binding:"required"`
}

// CreateCreditTopUp starts a prepaid credit purchase and returns the
// client secret the browser uses to pay. Credit is added only when Stripe
// confirms the payment (webhook), never by this call.
// POST /v1/billing/credits/topups
func (h *BillingHandler) CreateCreditTopUp(c *gin.Context) {
	accountID, ok := requireSignedInUser(c)
	if !ok {
		return
	}
	var req createTopUpRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": billing.ErrTopUpAmount.Error()})
		return
	}

	intent, err := h.billingService.CreateTopUp(c.Request.Context(), accountID, req.Amount)
	switch {
	case errors.Is(err, billing.ErrTopUpAmount):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, billing.ErrAccountClosed):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	case errors.Is(err, billing.ErrPaymentsNotConfigured):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
	case err != nil:
		log.Printf("WARN: create top-up for account %s: %v", accountID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not start the payment, please try again"})
	default:
		c.JSON(http.StatusCreated, intent)
	}
}

// ListCreditTopUps returns the account's credit purchase history.
// GET /v1/billing/credits/topups
func (h *BillingHandler) ListCreditTopUps(c *gin.Context) {
	accountID, ok := auth.GetAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account authentication required"})
		return
	}
	topUps, err := h.billingService.ListTopUps(c.Request.Context(), accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"topups": topUps, "count": len(topUps)})
}

// GetCreditTopUp returns one top-up, so the console can follow a payment
// it just submitted until the webhook settles it. Another account's
// top-up is a 404.
// GET /v1/billing/credits/topups/:id
func (h *BillingHandler) GetCreditTopUp(c *gin.Context) {
	accountID, ok := auth.GetAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account authentication required"})
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid top-up id"})
		return
	}
	topUp, err := h.billingService.GetTopUp(c.Request.Context(), accountID, id)
	if errors.Is(err, billing.ErrTopUpNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, topUp)
}

// GetCreditStatus handles GET /v1/billing/credits/status: the account's credit
// position - balance, current spend, how long it lasts, and how worried to
// be - for the console banner. Computed by the same code that decides which
// warning emails to send, so the banner and the emails cannot disagree.
func (h *BillingHandler) GetCreditStatus(c *gin.Context) {
	accountID, ok := auth.GetAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account authentication required"})
		return
	}
	report, err := h.billingService.Runway(c.Request.Context(), accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	resp := gin.H{
		"balance":       report.Balance,
		"burn_per_hour": report.BurnPerHour,
		"level":         report.Level.String(),
		"impacted":      report.Impacted,
		"auto_recharge": report.AutoRecharge,
		"runway_hours":  nil,
	}
	if hours, ok := report.RunwayHours(); ok {
		resp["runway_hours"] = hours
	}
	if hold, err := h.billingService.ActiveStorageHold(c.Request.Context(), accountID); err == nil && hold != nil {
		resp["storage_delete_after"] = hold.DeleteAfter.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, resp)
}

// GetCreditBalance returns the account's current credit balance, for the
// billing overview and the console's provisioning pre-check.
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

// autoRechargeView is the wire form of an account's automatic recharge rule.
func autoRechargeView(ar *billing.AutoRecharge) gin.H {
	return gin.H{
		"enabled":              ar.Enabled,
		"configured":           ar.Configured,
		"threshold":            ar.Threshold,
		"amount":               ar.Amount,
		"monthly_cap":          ar.MonthlyCap,
		"spent_this_month":     ar.SpentThisMonth,
		"consecutive_failures": ar.ConsecutiveFailures,
		"disabled_reason":      ar.DisabledReason,
		"has_card":             ar.HasCard,
		"limits": gin.H{
			"min_threshold":   billing.MinAutoThreshold,
			"min_amount":      billing.MinTopUp,
			"max_amount":      billing.MaxTopUp,
			"max_monthly_cap": billing.MaxAutoMonthlyCap,
			"max_failures":    billing.MaxAutoFailures,
		},
	}
}

// GetAutoRecharge handles GET /v1/billing/credits/auto-recharge.
func (h *BillingHandler) GetAutoRecharge(c *gin.Context) {
	accountID, ok := auth.GetAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account authentication required"})
		return
	}
	ar, err := h.billingService.GetAutoRecharge(c.Request.Context(), accountID)
	if err != nil {
		log.Printf("api: automatic recharge read for %s: %v", accountID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read automatic recharge"})
		return
	}
	c.JSON(http.StatusOK, autoRechargeView(ar))
}

type putAutoRechargeRequest struct {
	Enabled    bool    `json:"enabled"`
	Threshold  float64 `json:"threshold"`
	Amount     float64 `json:"amount"`
	MonthlyCap float64 `json:"monthly_cap"`
}

// PutAutoRecharge handles PUT /v1/billing/credits/auto-recharge: save the
// rule, or switch it off. Only a signed-in user may set it - it authorises
// charging the saved card without the customer present, so an API key or an
// agent session must never be able to.
func (h *BillingHandler) PutAutoRecharge(c *gin.Context) {
	accountID, ok := requireSignedInUser(c)
	if !ok {
		return
	}
	var req putAutoRechargeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "threshold, amount and monthly_cap are required"})
		return
	}
	userID, _ := auth.GetUserID(c)
	err := h.billingService.SetAutoRecharge(c.Request.Context(), accountID, userID, billing.AutoRechargeSettings{
		Enabled: req.Enabled, Threshold: req.Threshold, Amount: req.Amount, MonthlyCap: req.MonthlyCap,
	})
	switch {
	case errors.Is(err, billing.ErrAutoRechargeInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "invalid_auto_recharge"})
		return
	case errors.Is(err, billing.ErrNoDefaultCard):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "no_default_card"})
		return
	case err != nil:
		log.Printf("api: automatic recharge save for %s: %v", accountID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not save automatic recharge"})
		return
	}
	ar, err := h.billingService.GetAutoRecharge(c.Request.Context(), accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "saved, but could not read it back"})
		return
	}
	c.JSON(http.StatusOK, autoRechargeView(ar))
}
