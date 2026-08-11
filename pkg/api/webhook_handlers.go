// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"io"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

// WebhookEvent is the neutral, already-verified event the webhook acts
// on. Defined in the api package (mirroring payments.WebhookEvent) so the
// verifier can be an interface and the handler stays free of stripe-go.
type WebhookEvent struct {
	Type            string
	SetupIntentID   string
	PaymentMethodID string
	Card            *CardDetails
}

// CardDetails mirrors payments.CardDetails for the same decoupling reason.
type CardDetails struct {
	Brand, Last4      string
	ExpMonth, ExpYear int
	PaymentMethodID   string
}

// StripeWebhookVerifier authenticates and decodes a raw webhook request.
// Implemented by an adapter over *payments.Client (see main), so this
// package depends on behaviour, not on stripe-go.
type StripeWebhookVerifier interface {
	VerifyWebhook(payload []byte, sigHeader string) (*WebhookEvent, error)
	// GetCard fetches display details for a payment method — needed after
	// a setup_intent succeeds, which carries only the pm id.
	GetCard(paymentMethodID string) (*CardDetails, error)
}

// WebhookHandler processes Stripe webhooks.
type WebhookHandler struct {
	verifier StripeWebhookVerifier
	billing  *billing.Service
}

// NewWebhookHandler wires the verifier and billing service. Either nil
// disables the endpoint (it returns 503) — the standalone/no-Stripe path.
func NewWebhookHandler(verifier StripeWebhookVerifier, billingService *billing.Service) *WebhookHandler {
	return &WebhookHandler{verifier: verifier, billing: billingService}
}

// HandleStripe is POST /v1/webhooks/stripe. Unauthenticated at the router
// (Stripe calls it) — its authentication IS the signature check, which is
// mandatory and happens first.
//
// Contract with Stripe: return 2xx quickly, and do the work idempotently.
// Stripe retries on any non-2xx or slow response, so every handler below
// must be safe to run twice. Unknown event types are acknowledged with
// 200, not errored — Stripe should not retry an event we simply do not
// act on.
func (h *WebhookHandler) HandleStripe(c *gin.Context) {
	if h.verifier == nil || h.billing == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhooks not configured"})
		return
	}

	// The signature is computed over the EXACT raw body, so read it
	// verbatim — no JSON binding, which would re-serialize and break the
	// signature.
	payload, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot read body"})
		return
	}

	event, err := h.verifier.VerifyWebhook(payload, c.GetHeader("Stripe-Signature"))
	if err != nil {
		// A bad signature is an authentication failure. 400 (not 401) is
		// what Stripe's own docs expect, and it must not be retried into
		// success — the body is simply not trusted.
		log.Printf("WARN: stripe webhook rejected: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid signature"})
		return
	}

	ctx := c.Request.Context()
	if err := h.dispatch(ctx, event); err != nil {
		// A processing failure (DB down) should be retried by Stripe, so
		// return 500 — the signature was fine, the work was not done.
		log.Printf("WARN: stripe webhook %s processing failed: %v", event.Type, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "processing failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"received": true})
}

func (h *WebhookHandler) dispatch(ctx context.Context, event *WebhookEvent) error {
	switch event.Type {
	case "setup_intent.succeeded":
		// The card is validated. Fetch its display details, then mark our
		// pending row verified — which opens the provisioning gate.
		card, err := h.verifier.GetCard(event.PaymentMethodID)
		if err != nil {
			return err
		}
		return h.billing.MarkPaymentMethodVerified(ctx, event.SetupIntentID, billing.CardSummary{
			Brand:           card.Brand,
			Last4:           card.Last4,
			ExpMonth:        card.ExpMonth,
			ExpYear:         card.ExpYear,
			PaymentMethodID: card.PaymentMethodID,
		})

	case "setup_intent.setup_failed":
		return h.billing.MarkSetupFailed(ctx, event.SetupIntentID)

	case "payment_method.detached":
		// Card removed at Stripe's end. If it was the account's last
		// verified card, this starts the 24h grace clock.
		return h.billing.MarkPaymentMethodDetachedByStripeID(ctx, event.PaymentMethodID)

	case "payment_method.automatically_updated":
		if event.Card != nil {
			return h.billing.RefreshCardByStripeID(ctx, event.PaymentMethodID,
				event.Card.Brand, event.Card.Last4, event.Card.ExpMonth, event.Card.ExpYear)
		}
		return nil

	default:
		// Acknowledged but not acted on — Stripe sends many event types;
		// silently 200 the ones we do not handle.
		return nil
	}
}
