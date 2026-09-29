// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// CardSummary is the display detail the console shows for a stored card,
// mirrored from the payments package so billing does not import Stripe.
type CardSummary struct {
	Brand           string
	Last4           string
	ExpMonth        int
	ExpYear         int
	PaymentMethodID string
}

// StripeGateway is the slice of the Stripe boundary the billing service
// needs. Defined here as an interface so billing depends on behaviour,
// not on pkg/payments (or stripe-go) — main injects the concrete client,
// tests inject a fake, and neither creates an import cycle. Mirrors the
// PDFStore pattern.
type StripeGateway interface {
	EnsureCustomer(existingID, email, name, accountNumber string) (string, error)
	CreateSetupIntent(customerID, currency string) (clientSecret, intentID string, err error)
	GetPaymentMethod(pmID string) (*CardSummary, error)
	DetachPaymentMethod(pmID string) error
	// CreatePaymentIntent charges a stored card off-session for the net
	// amount of an issued invoice. idempotencyKey (the invoice id) makes a
	// retried create a no-op at Stripe rather than a double charge.
	CreatePaymentIntent(customerID, pmID, currency string, amountCents int64, invoiceID, idempotencyKey string) (piID, status string, err error)
	// CreateTopUpPaymentIntent opens a PaymentIntent for a prepaid credit
	// purchase the customer confirms in the browser; returns its id and
	// client secret. idempotencyKey (derived from the top-up id) makes a
	// retried create return the same intent.
	CreateTopUpPaymentIntent(customerID string, amountCents int64, currency, topUpID, accountNumber, idempotencyKey string) (piID, clientSecret string, err error)
}

// ErrPaymentsNotConfigured means no payment provider is wired (local dev,
// or Stripe keys unset). Callers map it to 503.
var ErrPaymentsNotConfigured = errors.New("payments are not configured")

// WithStripe enables payment-method management. Left unset (local dev,
// no Stripe), the payment endpoints degrade safely — see each method.
// Returns the same *Service for chaining, so existing NewService(db) call
// sites and their tests compile unchanged.
func (s *Service) WithStripe(gw StripeGateway) *Service {
	s.stripe = gw
	return s
}

// AccountCanProvision is the single source of truth for the prepaid
// "no credit, no resources" gate. It answers one question — may this
// account create resources right now — so every caller (the create
// handler, Kumbha, the console pre-check) agrees. A card on file is not
// required: Teepin is prepaid only, so what matters is spendable credit,
// however it was funded.
//
// Returns (false, reason, nil) with a customer-facing reason when the
// account is not active or has no spendable credit. The balance is the same
// billing.credit_balance function CreditBalance reads. The
// reason is safe to show a customer; it never leaks another tenant's state
// because the caller has already established this is the caller's own
// account.
//
// One query, so the gate costs a single round-trip on the hot path of
// instance creation.
func (s *Service) AccountCanProvision(ctx context.Context, accountID uuid.UUID) (bool, string, error) {
	var status string
	var balance float64
	err := s.db.QueryRowContext(ctx, `
		SELECT a.status,
		       billing.credit_balance(a.id)
		FROM auth.accounts a
		WHERE a.id = $1
	`, accountID).Scan(&status, &balance)
	if err == sql.ErrNoRows {
		return false, "account not found", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("failed to check provisioning eligibility: %w", err)
	}

	switch status {
	case "suspended":
		return false, "this account is suspended; contact support", nil
	case "closed":
		return false, "this account is closed", nil
	}

	if balance <= 0 {
		return false, "add credit to your account before launching resources", nil
	}
	return true, "", nil
}

// CreateSetupIntent begins adding a card to an account: it ensures the
// account has a Stripe customer, opens a SetupIntent, and records a
// PENDING payment-method row keyed to that intent. The card is not yet
// usable — the webhook flips it to verified when Stripe confirms it.
// Returns the client secret the browser needs to confirm the card, and
// the pending row's own id.
//
// The id is returned specifically so the caller can clean the row up if
// the customer never completes the flow (closes the dialog, the Payment
// Element fails to load, a network error, etc.) — this row is created
// BEFORE any card is entered, so without a way to remove it, every
// abandoned attempt leaves a permanent, un-removable-by-anything-but-hand
// "Validating…" card behind. Found live 2026-08-21: the console's Stripe
// Elements failed to load (a null loadStripe() result was silently
// swallowed, a separate bug fixed in add-card-dialog.tsx), and the
// customer was left looking at a phantom pending card despite never
// entering anything and clicking Cancel.
func (s *Service) CreateSetupIntent(ctx context.Context, accountID uuid.UUID) (clientSecret string, paymentMethodID uuid.UUID, err error) {
	if s.stripe == nil {
		return "", uuid.Nil, ErrPaymentsNotConfigured
	}

	cust, err := s.ensureStripeCustomer(ctx, accountID)
	if err != nil {
		return "", uuid.Nil, err
	}
	newCustomerID := cust.CustomerID

	secret, intentID, err := s.stripe.CreateSetupIntent(newCustomerID, "usd")
	if err != nil {
		return "", uuid.Nil, err
	}

	// Record the pending card. stripe_payment_method_id is empty until
	// the webhook learns it; the intent id is how the webhook finds this
	// row again. RETURNING id so the caller can remove this row if the
	// customer never completes the flow — see the doc comment above.
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO billing.payment_methods
		(account_id, stripe_customer_id, stripe_payment_method_id, stripe_setup_intent_id, type, status)
		VALUES ($1, $2, '', $3, 'card', 'pending')
		RETURNING id
	`, accountID, newCustomerID, intentID).Scan(&paymentMethodID); err != nil {
		return "", uuid.Nil, fmt.Errorf("failed to record pending payment method: %w", err)
	}

	return secret, paymentMethodID, nil
}

// stripeCustomer is an account's Stripe identity plus the account facts
// callers opening a payment need alongside it.
type stripeCustomer struct {
	CustomerID    string
	AccountNumber string
	Status        string
}

// ensureStripeCustomer returns the account's Stripe customer, creating and
// persisting one on first use so an account never gets a second customer.
// Requires s.stripe to be set.
func (s *Service) ensureStripeCustomer(ctx context.Context, accountID uuid.UUID) (*stripeCustomer, error) {
	var (
		customerID sql.NullString
		email      sql.NullString
		accountNo  string
		display    string
		legal      sql.NullString
		status     string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT stripe_customer_id, billing_email, account_number, display_name, legal_name, status
		FROM auth.accounts WHERE id = $1
	`, accountID).Scan(&customerID, &email, &accountNo, &display, &legal, &status)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("account not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load account: %w", err)
	}

	name := legal.String
	if name == "" {
		name = display
	}

	id, err := s.stripe.EnsureCustomer(customerID.String, email.String, name, accountNo)
	if err != nil {
		return nil, err
	}
	if !customerID.Valid || customerID.String == "" {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE auth.accounts SET stripe_customer_id = $1, updated_at = NOW() WHERE id = $2`,
			id, accountID); err != nil {
			return nil, fmt.Errorf("failed to store stripe customer: %w", err)
		}
	}
	return &stripeCustomer{CustomerID: id, AccountNumber: accountNo, Status: status}, nil
}

// MarkPaymentMethodVerified is called from the Stripe webhook when a
// SetupIntent succeeds. It flips the pending row (matched by setup-intent
// id) to verified, snapshots the card's display details, makes it the
// default if the account has none, and CLEARS the account's grace clock —
// a good card on file cancels any pending suspension.
func (s *Service) MarkPaymentMethodVerified(ctx context.Context, setupIntentID string, card CardSummary) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var accountID uuid.UUID
	var existingDefaults int
	err = tx.QueryRowContext(ctx, `
		SELECT account_id,
		       (SELECT COUNT(*) FROM billing.payment_methods d
		          WHERE d.account_id = billing.payment_methods.account_id
		            AND d.is_default AND d.status = 'verified')
		FROM billing.payment_methods
		WHERE stripe_setup_intent_id = $1
	`, setupIntentID).Scan(&accountID, &existingDefaults)
	if err == sql.ErrNoRows {
		// No matching pending row — an event we did not initiate, or a
		// replay after the row was already updated. Not an error: webhooks
		// must be idempotent.
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to find pending payment method: %w", err)
	}

	makeDefault := existingDefaults == 0

	if _, err := tx.ExecContext(ctx, `
		UPDATE billing.payment_methods
		SET status = 'verified', verified_at = NOW(), updated_at = NOW(),
		    stripe_payment_method_id = $1, brand = $2, last4 = $3,
		    exp_month = $4, exp_year = $5, is_default = $6
		WHERE stripe_setup_intent_id = $7 AND status = 'pending'
	`, card.PaymentMethodID, card.Brand, card.Last4, card.ExpMonth, card.ExpYear,
		makeDefault, setupIntentID); err != nil {
		return fmt.Errorf("failed to verify payment method: %w", err)
	}

	// A good card cancels the grace clock.
	if _, err := tx.ExecContext(ctx,
		`UPDATE auth.accounts SET payment_failed_at = NULL, updated_at = NOW() WHERE id = $1`,
		accountID); err != nil {
		return fmt.Errorf("failed to clear grace clock: %w", err)
	}

	return tx.Commit()
}

// MarkSetupFailed flips a pending card to failed when its SetupIntent
// fails (bad card, failed 3DS). Idempotent — a missing row is not an
// error.
func (s *Service) MarkSetupFailed(ctx context.Context, setupIntentID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE billing.payment_methods
		SET status = 'failed', updated_at = NOW()
		WHERE stripe_setup_intent_id = $1 AND status = 'pending'
	`, setupIntentID)
	if err != nil {
		return fmt.Errorf("failed to mark setup failed: %w", err)
	}
	return nil
}

// MarkPaymentMethodDetachedByStripeID handles a card removed at Stripe's
// end (bank-initiated, fraud block, or a detach we did not originate) by
// flipping the row to removed. It deliberately does not suspend or start
// any grace clock: Teepin is prepaid, so losing a card never affects an
// account's ability to run — only its credit balance does.
//
// Idempotent: a missing/already-removed row is not an error.
func (s *Service) MarkPaymentMethodDetachedByStripeID(ctx context.Context, stripePMID string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE billing.payment_methods
		SET status = 'removed', is_default = FALSE, updated_at = NOW()
		WHERE stripe_payment_method_id = $1 AND status != 'removed'
	`, stripePMID); err != nil {
		return fmt.Errorf("failed to mark detached: %w", err)
	}
	return nil
}

// RefreshCardByStripeID updates a stored card's display details when
// Stripe auto-updates it (a bank reissue). Best-effort; a missing row is
// not an error.
func (s *Service) RefreshCardByStripeID(ctx context.Context, stripePMID, brand, last4 string, expMonth, expYear int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE billing.payment_methods
		SET brand = $1, last4 = $2, exp_month = $3, exp_year = $4, updated_at = NOW()
		WHERE stripe_payment_method_id = $5 AND status = 'verified'
	`, brand, last4, expMonth, expYear, stripePMID)
	if err != nil {
		return fmt.Errorf("failed to refresh card: %w", err)
	}
	return nil
}

// ListPaymentMethods returns an account's cards (excluding removed ones),
// default first, for the console. Never exposes the Stripe setup-intent
// id.
func (s *Service) ListPaymentMethods(ctx context.Context, accountID uuid.UUID) ([]PaymentMethod, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_id, stripe_customer_id, stripe_payment_method_id,
		       type, last4, brand, exp_month, exp_year, status, verified_at,
		       is_default, created_at, updated_at
		FROM billing.payment_methods
		WHERE account_id = $1 AND status != 'removed'
		ORDER BY is_default DESC, created_at DESC
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to list payment methods: %w", err)
	}
	defer rows.Close()

	methods := []PaymentMethod{}
	for rows.Next() {
		var m PaymentMethod
		if err := rows.Scan(
			&m.ID, &m.AccountID, &m.StripeCustomerID, &m.StripePaymentMethodID,
			&m.Type, &m.Last4, &m.Brand, &m.ExpMonth, &m.ExpYear, &m.Status,
			&m.VerifiedAt, &m.IsDefault, &m.CreatedAt, &m.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan payment method: %w", err)
		}
		methods = append(methods, m)
	}
	return methods, rows.Err()
}

// RemovePaymentMethod removes a card. Any card may be removed, including
// the last one: Teepin is prepaid, so an account runs on its credit
// balance, not on a card on file.
func (s *Service) RemovePaymentMethod(ctx context.Context, accountID, paymentMethodID uuid.UUID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var stripePMID string
	err = tx.QueryRowContext(ctx, `
		SELECT stripe_payment_method_id
		FROM billing.payment_methods
		WHERE id = $1 AND account_id = $2
		FOR UPDATE
	`, paymentMethodID, accountID).Scan(&stripePMID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("payment method not found")
	}
	if err != nil {
		return fmt.Errorf("failed to load payment method: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE billing.payment_methods
		SET status = 'removed', is_default = FALSE, updated_at = NOW()
		WHERE id = $1
	`, paymentMethodID); err != nil {
		return fmt.Errorf("failed to remove payment method: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit removal: %w", err)
	}

	// Detach at Stripe AFTER our own state is durable. A detach failure
	// here is not fatal — the card is already removed on our side and the
	// worst case is an orphaned pm at Stripe, which is harmless.
	if s.stripe != nil && stripePMID != "" {
		if err := s.stripe.DetachPaymentMethod(stripePMID); err != nil {
			// best-effort; do not fail the removal the customer asked for
			_ = err
		}
	}
	return nil
}

// SetDefaultPaymentMethod makes one verified card the account's default.
func (s *Service) SetDefaultPaymentMethod(ctx context.Context, accountID, paymentMethodID uuid.UUID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Clear the current default, then set the new one — the partial
	// unique index forbids two defaults, so this order avoids a conflict.
	if _, err := tx.ExecContext(ctx,
		`UPDATE billing.payment_methods SET is_default = FALSE, updated_at = NOW()
		 WHERE account_id = $1 AND is_default`, accountID); err != nil {
		return fmt.Errorf("failed to clear default: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE billing.payment_methods SET is_default = TRUE, updated_at = NOW()
		WHERE id = $1 AND account_id = $2 AND status = 'verified'
	`, paymentMethodID, accountID)
	if err != nil {
		return fmt.Errorf("failed to set default: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("payment method not found or not verified")
	}
	return tx.Commit()
}
