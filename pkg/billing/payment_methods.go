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
}

// ErrLastVerifiedCard is returned when removing a card would leave the
// account with no means of payment. Callers map it to 409 Conflict: the
// request is well-formed, the current state forbids it.
var ErrLastVerifiedCard = errors.New("cannot remove the only verified payment method; add a replacement first")

// WithStripe enables payment-method management. Left unset (local dev,
// no Stripe), the payment endpoints and the provisioning gate's
// card check degrade safely — see each method. Returns the same
// *Service for chaining, so existing NewService(db) call sites and their
// tests compile unchanged.
func (s *Service) WithStripe(gw StripeGateway) *Service {
	s.stripe = gw
	return s
}

// AccountCanProvision is the single source of truth for the "no
// validated card, no resources" gate. It answers one question — may this
// account create resources right now — so every caller (the create
// handler, the console pre-check) agrees.
//
// Returns (false, reason, nil) with a customer-facing reason when the
// account is not active or has no verified payment method. The reason is
// safe to show a customer; it never leaks another tenant's state because
// the caller has already established this is the caller's own account.
//
// One query, so the gate costs a single round-trip on the hot path of
// instance creation.
func (s *Service) AccountCanProvision(ctx context.Context, accountID uuid.UUID) (bool, string, error) {
	var status string
	var verifiedCards int
	err := s.db.QueryRowContext(ctx, `
		SELECT a.status,
		       (SELECT COUNT(*) FROM billing.payment_methods pm
		          WHERE pm.account_id = a.id AND pm.status = 'verified')
		FROM auth.accounts a
		WHERE a.id = $1
	`, accountID).Scan(&status, &verifiedCards)
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

	if verifiedCards == 0 {
		return false, "add a validated payment method before launching resources", nil
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
		return "", uuid.Nil, fmt.Errorf("payments not configured")
	}

	// Resolve the account's billing identity and existing Stripe customer.
	var (
		customerID sql.NullString
		email      sql.NullString
		accountNo  string
		display    string
		legal      sql.NullString
	)
	err = s.db.QueryRowContext(ctx, `
		SELECT stripe_customer_id, billing_email, account_number, display_name, legal_name
		FROM auth.accounts WHERE id = $1
	`, accountID).Scan(&customerID, &email, &accountNo, &display, &legal)
	if err == sql.ErrNoRows {
		return "", uuid.Nil, fmt.Errorf("account not found")
	}
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("failed to load account: %w", err)
	}

	name := legal.String
	if name == "" {
		name = display
	}

	newCustomerID, err := s.stripe.EnsureCustomer(customerID.String, email.String, name, accountNo)
	if err != nil {
		return "", uuid.Nil, err
	}
	// Persist a freshly created customer id so we never create a second.
	if !customerID.Valid || customerID.String == "" {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE auth.accounts SET stripe_customer_id = $1, updated_at = NOW() WHERE id = $2`,
			newCustomerID, accountID); err != nil {
			return "", uuid.Nil, fmt.Errorf("failed to store stripe customer: %w", err)
		}
	}

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
// end (bank-initiated, fraud block, or a detach we did not originate). It
// flips the row to removed and, if that was the account's LAST verified
// card, starts the 24h grace clock — this is exactly the "card went bad
// out from under us" case the clock exists for. A detach we performed
// ourselves already removed the row (RemovePaymentMethod), so the status
// update is a no-op and the last-card check sees the truthful count.
//
// Idempotent: a missing/already-removed row is not an error.
func (s *Service) MarkPaymentMethodDetachedByStripeID(ctx context.Context, stripePMID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var accountID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		SELECT account_id FROM billing.payment_methods
		WHERE stripe_payment_method_id = $1 AND status != 'removed'
	`, stripePMID).Scan(&accountID)
	if err == sql.ErrNoRows {
		return nil // already removed or never ours — idempotent
	}
	if err != nil {
		return fmt.Errorf("failed to find payment method: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE billing.payment_methods
		SET status = 'removed', is_default = FALSE, updated_at = NOW()
		WHERE stripe_payment_method_id = $1
	`, stripePMID); err != nil {
		return fmt.Errorf("failed to mark detached: %w", err)
	}

	// If the account now has no verified card, start the grace clock —
	// but only if it is not already running (do not push the deadline
	// forward on a second card loss).
	var remaining int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM billing.payment_methods
		WHERE account_id = $1 AND status = 'verified'
	`, accountID).Scan(&remaining); err != nil {
		return fmt.Errorf("failed to count remaining cards: %w", err)
	}
	if remaining == 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth.accounts
			SET payment_failed_at = COALESCE(payment_failed_at, NOW()), updated_at = NOW()
			WHERE id = $1
		`, accountID); err != nil {
			return fmt.Errorf("failed to start grace clock: %w", err)
		}
	}

	return tx.Commit()
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

// RemovePaymentMethod removes a card, enforcing the invariant that an
// account is never left card-less: if the target is the only VERIFIED
// card, it refuses with ErrLastVerifiedCard (→ 409). The replacement
// flow is therefore always add-new-then-remove-old.
func (s *Service) RemovePaymentMethod(ctx context.Context, accountID, paymentMethodID uuid.UUID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Lock the account's verified cards for the duration so a concurrent
	// remove cannot race us to "both were the last one".
	var targetStatus string
	var stripePMID string
	err = tx.QueryRowContext(ctx, `
		SELECT status, stripe_payment_method_id
		FROM billing.payment_methods
		WHERE id = $1 AND account_id = $2
		FOR UPDATE
	`, paymentMethodID, accountID).Scan(&targetStatus, &stripePMID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("payment method not found")
	}
	if err != nil {
		return fmt.Errorf("failed to load payment method: %w", err)
	}

	// Only a verified card counts toward the "must keep one" invariant —
	// removing a pending/failed card is always allowed.
	if targetStatus == "verified" {
		var verifiedCount int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM billing.payment_methods
			WHERE account_id = $1 AND status = 'verified'
		`, accountID).Scan(&verifiedCount); err != nil {
			return fmt.Errorf("failed to count verified cards: %w", err)
		}
		if verifiedCount <= 1 {
			return ErrLastVerifiedCard
		}
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
