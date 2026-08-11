// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package payments is the Stripe boundary for the platform.
//
// It is the ONLY package that imports stripe-go. Everything else reaches
// Stripe through this narrow surface — the same discipline as
// pkg/storage/s3, so the billing domain never depends on a payment
// vendor's types. That keeps Stripe swappable and, more immediately,
// keeps the billing package's tests free of Stripe.
//
// This package both VALIDATES cards (SetupIntent, off-session) and CHARGES
// them (PaymentIntent, off-session). Validation moves no funds; the charge
// path (CreatePaymentIntent) is the only place money actually moves, and it
// is driven by the billing ChargeCollector against already-issued invoices.
package payments

import (
	"encoding/json"
	"fmt"

	"github.com/stripe/stripe-go/v79"
	"github.com/stripe/stripe-go/v79/client"
	"github.com/stripe/stripe-go/v79/webhook"
)

// Client wraps a Stripe API client bound to one secret key. Constructed
// with an explicit backend rather than the package-level global
// stripe.Key, so the key is not process-global shared state and tests
// can run without touching a real account.
type Client struct {
	sc            *client.API
	webhookSecret string
}

// CardDetails is the display-only summary of a validated card. Never the
// full number — Stripe holds that; we keep only what a customer needs to
// recognise which card is on file.
type CardDetails struct {
	Brand    string
	Last4    string
	ExpMonth int
	ExpYear  int
	// PaymentMethodID is Stripe's pm_… id; the account's stored card.
	PaymentMethodID string
}

// NewClient builds a Stripe client from the secret key. webhookSecret is
// the signing secret for the webhook endpoint (whsec_…); an empty one
// makes VerifyWebhook reject everything, which is the safe default when
// webhooks are not configured.
func NewClient(secretKey, webhookSecret string) *Client {
	sc := &client.API{}
	sc.Init(secretKey, nil)
	return &Client{sc: sc, webhookSecret: webhookSecret}
}

// EnsureCustomer returns the Stripe customer id for an account, creating
// one if existingID is empty. Idempotent by construction: the caller
// passes the account's stored stripe_customer_id and persists whatever
// comes back, so a customer is created at most once per account.
func (c *Client) EnsureCustomer(existingID, email, name, accountNumber string) (string, error) {
	if existingID != "" {
		return existingID, nil
	}
	params := &stripe.CustomerParams{
		Email: stripe.String(email),
		Name:  stripe.String(name),
	}
	// Tie the Stripe customer back to the TEEPIN account for support and
	// reconciliation — a Stripe dashboard row should be traceable to an
	// account without a second lookup.
	params.AddMetadata("teepin_account_number", accountNumber)

	cust, err := c.sc.Customers.New(params)
	if err != nil {
		return "", fmt.Errorf("stripe: create customer: %w", err)
	}
	return cust.ID, nil
}

// CreateSetupIntent starts card validation for a customer. usage is
// off_session so the saved card can be charged later without the
// customer present — the whole point of validating now to bill later.
// Returns the client secret (handed to the browser to confirm the card)
// and the SetupIntent id (stored so the webhook can match the result
// back to our pending payment-method row).
//
// currency is carried through so multi-currency is a later config change
// rather than a rewrite; today every caller passes "usd".
func (c *Client) CreateSetupIntent(customerID, currency string) (clientSecret, intentID string, err error) {
	params := &stripe.SetupIntentParams{
		Customer:           stripe.String(customerID),
		Usage:              stripe.String(string(stripe.SetupIntentUsageOffSession)),
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
	}
	params.AddMetadata("currency", currency)

	si, err := c.sc.SetupIntents.New(params)
	if err != nil {
		return "", "", fmt.Errorf("stripe: create setup intent: %w", err)
	}
	return si.ClientSecret, si.ID, nil
}

// GetPaymentMethod fetches a card's display details. Used after a
// SetupIntent succeeds to snapshot brand/last4/exp onto our row, so the
// console can show "Visa ···· 4242" without another Stripe round-trip.
func (c *Client) GetPaymentMethod(pmID string) (*CardDetails, error) {
	pm, err := c.sc.PaymentMethods.Get(pmID, nil)
	if err != nil {
		return nil, fmt.Errorf("stripe: get payment method: %w", err)
	}
	d := &CardDetails{PaymentMethodID: pm.ID}
	if pm.Card != nil {
		d.Brand = string(pm.Card.Brand)
		d.Last4 = pm.Card.Last4
		d.ExpMonth = int(pm.Card.ExpMonth)
		d.ExpYear = int(pm.Card.ExpYear)
	}
	return d, nil
}

// DetachPaymentMethod removes a card from its customer at Stripe. Called
// when a customer removes a card AND our own invariant (never leave an
// account card-less) has already been checked by the caller.
func (c *Client) DetachPaymentMethod(pmID string) error {
	if _, err := c.sc.PaymentMethods.Detach(pmID, nil); err != nil {
		return fmt.Errorf("stripe: detach payment method: %w", err)
	}
	return nil
}

// CreatePaymentIntent charges a stored card off-session for an issued
// invoice. This is the FIRST place in the platform that moves money — a
// SetupIntent validated the card, this actually collects.
//
//   - amountCents is the NET amount to collect (invoice total minus any
//     credit already applied), in the smallest currency unit; the caller
//     has already handled the credit-covered case and never passes below
//     Stripe's minimum.
//   - Confirm+OffSession together mean "charge the saved card now, the
//     customer is not present" — the whole point of validating off-session
//     earlier. A card that needs 3DS will decline here rather than prompt,
//     surfacing as an authentication_required error the caller records.
//   - idempotencyKey (the invoice id) makes a retried create a no-op at
//     Stripe: a network failure after Stripe charged the card, followed by
//     our retry, returns the SAME PaymentIntent rather than double-charging.
//
// Returns the PaymentIntent id and its status ("succeeded",
// "requires_action", "processing", …). A card decline is a normal outcome,
// returned as an error with a customer-safe message the caller stores.
func (c *Client) CreatePaymentIntent(customerID, pmID, currency string, amountCents int64, invoiceID, idempotencyKey string) (piID, status string, err error) {
	params := &stripe.PaymentIntentParams{
		Amount:        stripe.Int64(amountCents),
		Currency:      stripe.String(currency),
		Customer:      stripe.String(customerID),
		PaymentMethod: stripe.String(pmID),
		Confirm:       stripe.Bool(true),
		OffSession:    stripe.Bool(true),
	}
	params.AddMetadata("teepin_invoice_id", invoiceID)
	if idempotencyKey != "" {
		params.SetIdempotencyKey(idempotencyKey)
	}

	pi, err := c.sc.PaymentIntents.New(params)
	if err != nil {
		// A decline still carries a PaymentIntent id on the error's
		// underlying object; surface the id when we have it so the caller
		// can reconcile, but return the error so the charge counts as failed.
		if serr, ok := err.(*stripe.Error); ok {
			id := ""
			if serr.PaymentIntent != nil {
				id = serr.PaymentIntent.ID
			}
			return id, "", fmt.Errorf("stripe: charge declined: %s", serr.Msg)
		}
		return "", "", fmt.Errorf("stripe: create payment intent: %w", err)
	}
	return pi.ID, string(pi.Status), nil
}

// WebhookEvent is a vendor-neutral view of the Stripe events this
// platform acts on, decoded from a verified webhook. Returning this
// rather than a stripe.Event keeps the api/billing packages free of
// stripe-go types — the same boundary discipline as the rest of this
// package.
type WebhookEvent struct {
	// Type is the Stripe event type, e.g. "setup_intent.succeeded".
	Type string
	// SetupIntentID / PaymentMethodID are populated for the events that
	// carry them; empty otherwise.
	SetupIntentID   string
	PaymentMethodID string
	// Card is populated for payment_method.* events.
	Card *CardDetails
	// PaymentIntentID / InvoiceID are populated for payment_intent.* events.
	// InvoiceID comes from the metadata we set when creating the intent; it
	// is for logging only — reconciliation matches on PaymentIntentID, which
	// we ourselves minted, never on a client-supplied id.
	PaymentIntentID string
	InvoiceID       string
	// FailureReason is the customer-safe decline message on a failed charge.
	FailureReason string
}

// VerifyWebhook authenticates a webhook request and decodes it to a
// neutral event. The signature check is MANDATORY: an unverified webhook
// body is attacker-controlled input, and acting on a forged event could
// mark a card verified without a real card. An empty webhook secret
// rejects everything.
func (c *Client) VerifyWebhook(payload []byte, sigHeader string) (*WebhookEvent, error) {
	if c.webhookSecret == "" {
		return nil, fmt.Errorf("stripe: webhook secret not configured")
	}
	// IgnoreAPIVersionMismatch: our Stripe account is on a newer API
	// version than this stripe-go release defaults to, and the default
	// ConstructEvent treats that as a HARD failure (rejecting a
	// correctly-signed event). That is safe to ignore HERE specifically
	// because we deserialize only a few stable primitive fields below
	// (the setup-intent/payment-method id and card brand/last4/exp) — none
	// of which change shape across these versions. The signature is still
	// fully verified; only the version gate is relaxed.
	event, err := webhook.ConstructEventWithOptions(payload, sigHeader, c.webhookSecret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		return nil, fmt.Errorf("stripe: webhook signature verification failed: %w", err)
	}

	out := &WebhookEvent{Type: string(event.Type)}

	switch event.Type {
	case "setup_intent.succeeded", "setup_intent.setup_failed":
		var si struct {
			ID            string `json:"id"`
			PaymentMethod string `json:"payment_method"`
		}
		if err := json.Unmarshal(event.Data.Raw, &si); err != nil {
			return nil, fmt.Errorf("stripe: decode setup_intent: %w", err)
		}
		out.SetupIntentID = si.ID
		out.PaymentMethodID = si.PaymentMethod

	case "payment_method.detached", "payment_method.automatically_updated":
		var pm struct {
			ID   string `json:"id"`
			Card *struct {
				Brand    string `json:"brand"`
				Last4    string `json:"last4"`
				ExpMonth int    `json:"exp_month"`
				ExpYear  int    `json:"exp_year"`
			} `json:"card"`
		}
		if err := json.Unmarshal(event.Data.Raw, &pm); err != nil {
			return nil, fmt.Errorf("stripe: decode payment_method: %w", err)
		}
		out.PaymentMethodID = pm.ID
		if pm.Card != nil {
			out.Card = &CardDetails{
				PaymentMethodID: pm.ID,
				Brand:           pm.Card.Brand,
				Last4:           pm.Card.Last4,
				ExpMonth:        pm.Card.ExpMonth,
				ExpYear:         pm.Card.ExpYear,
			}
		}

	case "payment_intent.succeeded", "payment_intent.payment_failed":
		var pi struct {
			ID            string            `json:"id"`
			Metadata      map[string]string `json:"metadata"`
			LastPaymentErr *struct {
				Message string `json:"message"`
			} `json:"last_payment_error"`
		}
		if err := json.Unmarshal(event.Data.Raw, &pi); err != nil {
			return nil, fmt.Errorf("stripe: decode payment_intent: %w", err)
		}
		out.PaymentIntentID = pi.ID
		if pi.Metadata != nil {
			out.InvoiceID = pi.Metadata["teepin_invoice_id"]
		}
		if pi.LastPaymentErr != nil {
			out.FailureReason = pi.LastPaymentErr.Message
		}
	}

	return out, nil
}
