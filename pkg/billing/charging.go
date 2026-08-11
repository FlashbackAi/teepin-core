// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/google/uuid"
)

// maxChargeAttempts bounds the dunning retries. After this many failed
// attempts to collect an invoice, the account's grace clock is armed and
// the existing 24h suspension sweeper takes over. Deliberately forgiving:
// a couple of transient declines must not race to kill a workload.
const maxChargeAttempts = 3

// minChargeCents is Stripe's minimum chargeable amount ($0.50 USD). A net
// below this cannot be collected via a card, so such an invoice is settled
// without a Stripe call — the amount is effectively covered (by credit or
// rounding) and it would be absurd to dun a customer over four cents.
const minChargeCents = 50

// chargeBackoff returns how long to wait after attempt n before retrying.
// Attempt 0 (never tried) has no wait; subsequent retries space out so a
// declining card is not hammered every hourly sweep.
func chargeBackoff(attempts int) time.Duration {
	switch attempts {
	case 0:
		return 0
	case 1:
		return 6 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// ChargeCollector charges issued usage invoices off-session. It follows the
// UsageCollector/SuspensionSweeper pattern: a ticker, an immediate first
// run, and a select on stop/ctx.
//
// It only ever touches usage invoices (source='usage'). Manual/negotiated
// invoices are collected out of band — auto-drafting a hand-agreed amount
// onto a card is exactly the surprise this platform must not create.
type ChargeCollector struct {
	db             *sql.DB
	billingService *Service
	interval       time.Duration
	stopChan       chan struct{}
}

// NewChargeCollector creates the charging job.
func NewChargeCollector(db *sql.DB, billingService *Service) *ChargeCollector {
	return &ChargeCollector{
		db:             db,
		billingService: billingService,
		interval:       1 * time.Hour,
		stopChan:       make(chan struct{}),
	}
}

// Start begins periodic charge sweeps.
func (c *ChargeCollector) Start(ctx context.Context) {
	log.Println("Starting charge collector...")

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	if err := c.sweep(ctx); err != nil {
		log.Printf("WARN: charge sweep error: %v", err)
	}

	for {
		select {
		case <-ticker.C:
			if err := c.sweep(ctx); err != nil {
				log.Printf("WARN: charge sweep error: %v", err)
			}
		case <-c.stopChan:
			log.Println("Stopping charge collector...")
			return
		case <-ctx.Done():
			log.Println("Charge collector stopped (context cancelled)")
			return
		}
	}
}

// Stop stops the collector.
func (c *ChargeCollector) Stop() { close(c.stopChan) }

// sweep finds open usage invoices that are due for a charge attempt and
// charges each. "Due" means: not yet settled (stripe_invoice_id IS NULL),
// under the attempt cap, and either never tried or past its backoff window.
func (c *ChargeCollector) sweep(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `
		SELECT id, charge_attempts
		FROM billing.invoices
		WHERE status = 'open'
		  AND source = 'usage'
		  AND stripe_invoice_id IS NULL
		  AND charge_attempts < $1
	`, maxChargeAttempts)
	if err != nil {
		return err
	}
	type due struct {
		id       uuid.UUID
		attempts int
	}
	var candidates []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.attempts); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	charged := 0
	for _, d := range candidates {
		// Backoff is evaluated here (not in SQL) so the window is a single
		// well-tested Go function rather than duplicated interval arithmetic.
		if !c.dueForAttempt(ctx, d.id, d.attempts) {
			continue
		}
		if err := c.billingService.ChargeInvoice(ctx, d.id); err != nil {
			log.Printf("WARN: charge attempt for invoice %s failed: %v", d.id, err)
			continue
		}
		charged++
	}
	if charged > 0 {
		log.Printf("Charge sweep: processed %d invoice(s)", charged)
	}
	return nil
}

// dueForAttempt reports whether enough time has passed since the last
// attempt on this invoice to try again.
func (c *ChargeCollector) dueForAttempt(ctx context.Context, invoiceID uuid.UUID, attempts int) bool {
	if attempts == 0 {
		return true
	}
	var last sql.NullTime
	if err := c.db.QueryRowContext(ctx,
		`SELECT last_charge_attempt_at FROM billing.invoices WHERE id = $1`,
		invoiceID).Scan(&last); err != nil {
		return false
	}
	if !last.Valid {
		return true
	}
	return time.Since(last.Time) >= chargeBackoff(attempts)
}

// ChargeInvoice attempts to collect a single open usage invoice against the
// account's default verified card, for the net amount (total minus credit
// already applied to the invoice's usage). Also callable standalone for an
// operator "charge now".
//
// The method is idempotent and safe to retry: an already-paid invoice is a
// no-op, and Stripe's idempotency key (the invoice id) prevents a
// double-charge on a retried create.
func (s *Service) ChargeInvoice(ctx context.Context, invoiceID uuid.UUID) error {
	// Load the invoice's charge-relevant state. status/stripe_invoice_id
	// gate whether there is anything to do; account_id/total/currency/period
	// drive the charge and the net-of-credits math.
	var (
		status        string
		source        string
		settled       sql.NullString
		accountID     uuid.UUID
		total         float64
		currency      string
		periodStart   time.Time
		periodEnd     time.Time
		invoiceNumber string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT status, source, stripe_invoice_id, account_id, total, currency,
		       period_start, period_end, invoice_number
		FROM billing.invoices WHERE id = $1
	`, invoiceID).Scan(&status, &source, &settled, &accountID, &total, &currency,
		&periodStart, &periodEnd, &invoiceNumber)
	if err == sql.ErrNoRows {
		return fmt.Errorf("invoice not found")
	}
	if err != nil {
		return fmt.Errorf("failed to load invoice for charging: %w", err)
	}

	// Nothing to do: already settled, already paid, or not in a chargeable
	// state. Idempotent — a webhook replay or a racing sweep lands here.
	if settled.Valid || status == "paid" || status == "void" {
		return nil
	}
	if status != "open" {
		return fmt.Errorf("invoice %s is %s, not open", invoiceNumber, status)
	}
	if source != "usage" {
		// Manual invoices are never auto-charged.
		return fmt.Errorf("invoice %s is %s-sourced; not auto-chargeable", invoiceNumber, source)
	}

	// Net amount = total minus credit already consumed for this invoice's
	// usage. Consumption rows are negative, so we add their magnitude back.
	credited, err := s.creditAppliedForPeriod(ctx, accountID, periodStart, periodEnd)
	if err != nil {
		return fmt.Errorf("failed to compute applied credit: %w", err)
	}
	net := total - credited
	netCents := int64(math.Round(net * 100))

	// Fully covered (by credit) or a sub-minimum remainder: settle without a
	// Stripe call. There is no card charge to make and nothing to dun.
	if netCents < minChargeCents {
		return s.MarkInvoicePaid(ctx, invoiceID, "credit")
	}

	if s.stripe == nil {
		// Charging not configured (local dev). Record the attempt so the
		// state is honest. bump=true because no gateway pre-bump happened.
		return s.recordChargeFailure(ctx, invoiceID, "charging not configured", true)
	}

	// Resolve the card to charge and the Stripe customer.
	customerID, pmID, err := s.defaultChargeCard(ctx, accountID)
	if errors.Is(err, errNoVerifiedCard) {
		// A can't-collect state: record it and, once attempts are exhausted,
		// arm the grace clock. This should be rare — the provisioning gate
		// requires a verified card — but a card removed after provisioning
		// lands here. bump=true: no gateway pre-bump happened.
		return s.recordChargeFailure(ctx, invoiceID, "no verified payment method on file", true)
	}
	if err != nil {
		return fmt.Errorf("failed to resolve charge card: %w", err)
	}

	// Reserve the attempt BEFORE the network call: write the attempt count
	// and time now so a crash mid-charge cannot lose the fact that we tried.
	// The PaymentIntent id is written on return; Stripe's idempotency key
	// (the invoice id) makes a duplicated create safe if we crash between.
	if _, err := s.db.ExecContext(ctx, `
		UPDATE billing.invoices
		SET charge_attempts = charge_attempts + 1,
		    last_charge_attempt_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, invoiceID); err != nil {
		return fmt.Errorf("failed to record charge attempt: %w", err)
	}

	piID, piStatus, chargeErr := s.stripe.CreatePaymentIntent(
		customerID, pmID, currency, netCents, invoiceID.String(), invoiceID.String())

	// Record the PaymentIntent id whenever we have one (success or a decline
	// that still minted an intent) so the webhook can reconcile by it.
	if piID != "" {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE billing.invoices SET stripe_payment_intent_id = $1, updated_at = NOW()
			WHERE id = $2 AND stripe_payment_intent_id IS NULL
		`, piID, invoiceID); err != nil {
			log.Printf("WARN: invoice %s charged (pi %s) but recording the intent id failed: %v",
				invoiceNumber, piID, err)
		}
	}

	if chargeErr != nil {
		// bump=false: the attempt was already counted before the gateway call.
		return s.recordChargeFailure(ctx, invoiceID, chargeErr.Error(), false)
	}

	// Optimistic settle: if the charge already succeeded synchronously, mark
	// paid now. The webhook is still the authority and will no-op on replay;
	// this just avoids waiting a webhook round-trip for the common case.
	if piStatus == "succeeded" {
		return s.MarkInvoicePaid(ctx, invoiceID, piID)
	}
	// requires_action / processing: leave it open; the webhook settles or
	// fails it. Clear any stale error from a prior attempt.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE billing.invoices SET last_charge_error = NULL, updated_at = NOW() WHERE id = $1`,
		invoiceID); err != nil {
		log.Printf("WARN: invoice %s: failed to clear charge error: %v", invoiceNumber, err)
	}
	return nil
}

// SettleInvoiceByPaymentIntent marks the invoice tied to a PaymentIntent
// paid. Called from the payment_intent.succeeded webhook — the authority on
// whether a charge settled. Matches by the PaymentIntent id WE minted (never
// a client-supplied invoice id). Idempotent: an already-paid invoice, or a
// pi id we do not recognise, is a no-op.
func (s *Service) SettleInvoiceByPaymentIntent(ctx context.Context, piID string) error {
	var invoiceID uuid.UUID
	var status string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, status FROM billing.invoices WHERE stripe_payment_intent_id = $1
	`, piID).Scan(&invoiceID, &status)
	if err == sql.ErrNoRows {
		// Not ours, or the optimistic settle already cleared the intent id —
		// either way nothing to do.
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to find invoice for payment intent: %w", err)
	}
	if status == "paid" {
		return nil // already settled (optimistic path or a prior replay)
	}
	return s.MarkInvoicePaid(ctx, invoiceID, piID)
}

// RecordChargeFailureByPaymentIntent records a failed charge from the
// payment_intent.payment_failed webhook. The attempt was already counted by
// the ChargeInvoice call that created the intent, so bump=false. Idempotent:
// an unknown pi id is a no-op.
func (s *Service) RecordChargeFailureByPaymentIntent(ctx context.Context, piID, reason string) error {
	var invoiceID uuid.UUID
	var status string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, status FROM billing.invoices WHERE stripe_payment_intent_id = $1
	`, piID).Scan(&invoiceID, &status)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to find invoice for payment intent: %w", err)
	}
	if status == "paid" {
		// A late failure webhook for an invoice already settled: ignore.
		return nil
	}
	if reason == "" {
		reason = "card charge failed"
	}
	return s.recordChargeFailure(ctx, invoiceID, reason, false)
}

// ChargeState is the operator-facing view of an invoice's charge progress:
// how many times we have tried, when, why the last attempt failed, and the
// PaymentIntent linkage. Deliberately NOT part of the Invoice model — these
// are retry internals an operator needs to diagnose a stuck invoice, never
// something a customer's invoice JSON should carry.
type ChargeState struct {
	ChargeAttempts        int        `json:"charge_attempts"`
	LastChargeAttemptAt   *time.Time `json:"last_charge_attempt_at,omitempty"`
	LastChargeError       string     `json:"last_charge_error,omitempty"`
	StripePaymentIntentID string     `json:"stripe_payment_intent_id,omitempty"`
}

// InvoiceChargeState returns the charge-progress fields for an invoice, for
// the admin invoice view. Separate query so the shared Invoice model and the
// customer-facing path stay untouched.
func (s *Service) InvoiceChargeState(ctx context.Context, invoiceID uuid.UUID) (*ChargeState, error) {
	var st ChargeState
	var lastErr, piID sql.NullString
	var lastAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT charge_attempts, last_charge_attempt_at, last_charge_error,
		       stripe_payment_intent_id
		FROM billing.invoices WHERE id = $1
	`, invoiceID).Scan(&st.ChargeAttempts, &lastAt, &lastErr, &piID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("invoice not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load charge state: %w", err)
	}
	if lastAt.Valid {
		st.LastChargeAttemptAt = &lastAt.Time
	}
	st.LastChargeError = lastErr.String
	st.StripePaymentIntentID = piID.String
	return &st, nil
}

// errNoVerifiedCard signals that an account has no card we can charge.
var errNoVerifiedCard = errors.New("no verified payment method")

// defaultChargeCard returns the Stripe customer id and payment-method id to
// charge for an account: the default verified card, or any verified card if
// none is flagged default. Returns errNoVerifiedCard when there is none.
func (s *Service) defaultChargeCard(ctx context.Context, accountID uuid.UUID) (customerID, pmID string, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT stripe_customer_id, stripe_payment_method_id
		FROM billing.payment_methods
		WHERE account_id = $1 AND status = 'verified' AND stripe_payment_method_id != ''
		ORDER BY is_default DESC, verified_at ASC
		LIMIT 1
	`, accountID).Scan(&customerID, &pmID)
	if err == sql.ErrNoRows {
		return "", "", errNoVerifiedCard
	}
	if err != nil {
		return "", "", err
	}
	return customerID, pmID, nil
}

// creditAppliedForPeriod sums the credit consumed against usage that falls
// in the invoice's period for the account — the amount already covered by
// credit and therefore netted out of the card charge. Consumption rows are
// negative; the magnitude is returned as a positive number.
func (s *Service) creditAppliedForPeriod(ctx context.Context, accountID uuid.UUID, start, end time.Time) (float64, error) {
	var applied sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(-SUM(ct.amount), 0)
		FROM billing.credit_transactions ct
		JOIN billing.usage_records ur ON ur.id = ct.usage_record_id
		WHERE ct.account_id = $1
		  AND ct.kind = 'consumption'
		  AND ur.start_time >= $2 AND ur.end_time <= $3
	`, accountID, start, end).Scan(&applied)
	if err != nil {
		return 0, err
	}
	return applied.Float64, nil
}

// recordChargeFailure stores the failure reason and, once the attempts are
// exhausted, arms the account's grace clock — the trigger the
// otherwise-inert suspension sweeper has been waiting for. It never pushes
// an already-running deadline forward (COALESCE), the same idiom
// MarkPaymentMethodDetachedByStripeID uses.
//
// bump controls whether this call also increments charge_attempts. The
// gateway path in ChargeInvoice pre-increments the attempt before the
// network call (so a crash cannot lose it) and passes bump=false; the
// no-card and not-configured paths never reached the gateway and pass
// bump=true so their attempt still counts toward the cap. Exactly one of
// the two counts an attempt — never both, never neither.
func (s *Service) recordChargeFailure(ctx context.Context, invoiceID uuid.UUID, reason string, bump bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var accountID uuid.UUID
	var attempts int
	err = tx.QueryRowContext(ctx, `
		UPDATE billing.invoices
		SET last_charge_error = $1,
		    charge_attempts = CASE WHEN $2 THEN charge_attempts + 1 ELSE charge_attempts END,
		    last_charge_attempt_at = NOW(), updated_at = NOW()
		WHERE id = $3
		RETURNING account_id, charge_attempts
	`, reason, bump, invoiceID).Scan(&accountID, &attempts)
	if err != nil {
		return fmt.Errorf("failed to record charge failure: %w", err)
	}

	if attempts >= maxChargeAttempts {
		// Exhausted: arm the 24h grace clock. The suspension sweeper will
		// suspend the account once the clock elapses — unless a good card or
		// a successful later collection clears it first.
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth.accounts
			SET payment_failed_at = COALESCE(payment_failed_at, NOW()), updated_at = NOW()
			WHERE id = $1
		`, accountID); err != nil {
			return fmt.Errorf("failed to arm grace clock: %w", err)
		}
		log.Printf("invoice charge exhausted after %d attempts; grace clock armed for account %s",
			attempts, accountID)
	}

	return tx.Commit()
}
