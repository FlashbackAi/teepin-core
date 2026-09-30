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
// This package VALIDATES cards (SetupIntent, off-session) and collects
// money two ways: prepaid credit top-ups the customer confirms in the
// browser (CreateTopUpPaymentIntent), and off-session invoice charges
// (CreatePaymentIntent).
package payments

import (
	"encoding/json"
	"fmt"
	"strings"

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

// TopUpPurpose is the PaymentIntent metadata value marking a prepaid
// credit top-up, so the webhook can route the event to top-up settlement
// rather than invoice settlement. Routing only: the top-up itself is
// always matched by the PaymentIntent id we minted.
const TopUpPurpose = "credit_topup"

// CreateTopUpPaymentIntent opens a PaymentIntent for a prepaid credit
// purchase that the customer completes in the browser (the Payment
// Element), unlike CreatePaymentIntent's off-session charge.
//
//   - Automatic payment methods let Stripe offer every method enabled on
//     the account (cards, Link, wallets, ACH Direct Debit) without code
//     changes here; which ones appear is Dashboard configuration.
//   - idempotencyKey (derived from our top-up id) makes a retried create
//     return the same PaymentIntent instead of opening a second one.
//
// Returns the PaymentIntent id (stored so the webhook can find the
// top-up) and its client secret (handed to the browser to confirm).
func (c *Client) CreateTopUpPaymentIntent(customerID string, amountCents int64, currency, topUpID, accountNumber, idempotencyKey string) (piID, clientSecret string, err error) {
	params := &stripe.PaymentIntentParams{
		Amount:   stripe.Int64(amountCents),
		Currency: stripe.String(currency),
		Customer: stripe.String(customerID),
		AutomaticPaymentMethods: &stripe.PaymentIntentAutomaticPaymentMethodsParams{
			Enabled: stripe.Bool(true),
		},
		Description: stripe.String("Teepin prepaid credit"),
	}
	params.AddMetadata("teepin_purpose", TopUpPurpose)
	params.AddMetadata("teepin_topup_id", topUpID)
	params.AddMetadata("teepin_account_number", accountNumber)
	params.SetIdempotencyKey(idempotencyKey)

	pi, err := c.sc.PaymentIntents.New(params)
	if err != nil {
		return "", "", fmt.Errorf("stripe: create top-up payment intent: %w", err)
	}
	return pi.ID, pi.ClientSecret, nil
}

// CreateAutoTopUpPaymentIntent charges a saved card for a prepaid credit
// top-up without the customer present (automatic recharge). Confirmed
// immediately and off-session; tagged exactly like a customer-initiated top-up
// so the same webhook credits it, issues the receipt and sends the email.
//
// A decline, or a card that needs the customer to authenticate, comes back as
// an error carrying a customer-safe message; the PaymentIntent id is returned
// when Stripe created one. idempotencyKey (derived from our top-up id) makes a
// retried create a no-op at Stripe instead of a second charge.
func (c *Client) CreateAutoTopUpPaymentIntent(customerID, pmID string, amountCents int64, currency, topUpID, accountNumber, idempotencyKey string) (piID, status string, err error) {
	params := &stripe.PaymentIntentParams{
		Amount:        stripe.Int64(amountCents),
		Currency:      stripe.String(currency),
		Customer:      stripe.String(customerID),
		PaymentMethod: stripe.String(pmID),
		Confirm:       stripe.Bool(true),
		OffSession:    stripe.Bool(true),
		Description:   stripe.String("Teepin prepaid credit (automatic recharge)"),
	}
	params.AddMetadata("teepin_purpose", TopUpPurpose)
	params.AddMetadata("teepin_topup_id", topUpID)
	params.AddMetadata("teepin_account_number", accountNumber)
	params.SetIdempotencyKey(idempotencyKey)

	pi, err := c.sc.PaymentIntents.New(params)
	if err != nil {
		if serr, ok := err.(*stripe.Error); ok {
			id := ""
			if serr.PaymentIntent != nil {
				id = serr.PaymentIntent.ID
			}
			return id, "", fmt.Errorf("stripe: charge declined: %s", serr.Msg)
		}
		return "", "", fmt.Errorf("stripe: create automatic top-up payment intent: %w", err)
	}
	return pi.ID, string(pi.Status), nil
}

// PaymentMethodSummary describes a payment method in a form fit for a
// receipt ("Visa ending 4242", "Chase account ending 6789", "Link"), for
// any method type Stripe may have used — not only cards. Unknown types
// fall back to a readable form of Stripe's type name.
func (c *Client) PaymentMethodSummary(pmID string) (string, error) {
	pm, err := c.sc.PaymentMethods.Get(pmID, nil)
	if err != nil {
		return "", fmt.Errorf("stripe: get payment method: %w", err)
	}
	return summarizePaymentMethod(pm), nil
}

// summarizePaymentMethod is the pure formatting half of
// PaymentMethodSummary, separated so it is testable without Stripe.
func summarizePaymentMethod(pm *stripe.PaymentMethod) string {
	switch {
	case pm.Card != nil:
		return fmt.Sprintf("%s ending %s", cardBrandLabel(string(pm.Card.Brand)), pm.Card.Last4)
	case pm.USBankAccount != nil:
		bank := pm.USBankAccount.BankName
		if bank == "" {
			bank = "Bank"
		}
		return fmt.Sprintf("%s account ending %s", bank, pm.USBankAccount.Last4)
	case pm.Link != nil:
		return "Link"
	case pm.CashApp != nil:
		return "Cash App Pay"
	case pm.AmazonPay != nil:
		return "Amazon Pay"
	case pm.Paypal != nil:
		return "PayPal"
	case pm.SEPADebit != nil:
		return fmt.Sprintf("SEPA Direct Debit ending %s", pm.SEPADebit.Last4)
	}
	if pm.Type == "" {
		return "Stripe"
	}
	return strings.ReplaceAll(string(pm.Type), "_", " ")
}

// cardBrandLabel turns Stripe's lowercase brand code into the name printed
// on a receipt.
func cardBrandLabel(brand string) string {
	switch brand {
	case "visa":
		return "Visa"
	case "mastercard":
		return "Mastercard"
	case "amex":
		return "American Express"
	case "discover":
		return "Discover"
	case "diners":
		return "Diners Club"
	case "jcb":
		return "JCB"
	case "unionpay":
		return "UnionPay"
	case "":
		return "Card"
	}
	name := strings.ReplaceAll(brand, "_", " ")
	return strings.ToUpper(name[:1]) + name[1:]
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
	// Purpose is the "teepin_purpose" metadata we set on the intent;
	// TopUpPurpose marks a prepaid credit top-up. Used for routing only.
	Purpose string
	// AmountReceivedCents / Currency are what Stripe actually collected, so
	// settlement can check them against what the top-up recorded.
	AmountReceivedCents int64
	Currency            string
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

	case "payment_intent.succeeded", "payment_intent.payment_failed", "payment_intent.processing":
		var pi struct {
			ID             string            `json:"id"`
			Metadata       map[string]string `json:"metadata"`
			AmountReceived int64             `json:"amount_received"`
			Currency       string            `json:"currency"`
			// payment_method is an id string unless expanded; we never
			// expand it on webhooks.
			PaymentMethod  string `json:"payment_method"`
			LastPaymentErr *struct {
				Message string `json:"message"`
			} `json:"last_payment_error"`
		}
		if err := json.Unmarshal(event.Data.Raw, &pi); err != nil {
			return nil, fmt.Errorf("stripe: decode payment_intent: %w", err)
		}
		out.PaymentIntentID = pi.ID
		out.PaymentMethodID = pi.PaymentMethod
		out.AmountReceivedCents = pi.AmountReceived
		out.Currency = pi.Currency
		if pi.Metadata != nil {
			out.InvoiceID = pi.Metadata["teepin_invoice_id"]
			out.Purpose = pi.Metadata["teepin_purpose"]
		}
		if pi.LastPaymentErr != nil {
			out.FailureReason = pi.LastPaymentErr.Message
		}
	}

	return out, nil
}
