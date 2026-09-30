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
	"strings"
	"time"

	"github.com/google/uuid"
)

// Automatic recharge: when an account's credit falls below a threshold, charge
// its saved default card for a fixed amount. The customer chooses the
// threshold, the amount and a monthly cap; three failed charges in a row
// switch it off.
const (
	// MinAutoThreshold is the lowest balance a customer may set the trigger at.
	MinAutoThreshold = 5.0
	// MaxAutoMonthlyCap is the highest monthly cap a customer may set.
	MaxAutoMonthlyCap = 10000.0
	// MaxAutoFailures is how many failed charges in a row switch it off.
	MaxAutoFailures = 3

	// Suggested settings shown before a customer has chosen any.
	DefaultAutoThreshold  = 10.0
	DefaultAutoAmount     = 50.0
	DefaultAutoMonthlyCap = 250.0

	// autoInflightWindow is how long a charge that has not settled counts as
	// still in progress. While one is, no second charge is made, so a slow
	// webhook can never cause a double charge.
	autoInflightWindow = 24 * time.Hour

	// autoClaimWindow is how long an attempt reserves the account: it stops a
	// second sweep or replica from charging at the same moment.
	autoClaimWindow = 10 * time.Minute

	// DefaultAutoRechargeInterval is how often accounts below their
	// threshold are looked for.
	DefaultAutoRechargeInterval = 1 * time.Minute
)

// autoRetryBackoff is the wait after the first and second failed charge.
var autoRetryBackoff = []time.Duration{2 * time.Hour, 8 * time.Hour}

var (
	// ErrAutoRechargeInvalid means the settings are out of range.
	ErrAutoRechargeInvalid = errors.New("invalid automatic recharge settings")
	// ErrNoDefaultCard means automatic recharge needs a valid saved card.
	ErrNoDefaultCard = errors.New("add a valid card before turning on automatic recharge")
)

// AutoRechargeSettings is what the customer chooses.
type AutoRechargeSettings struct {
	Enabled    bool
	Threshold  float64
	Amount     float64
	MonthlyCap float64
}

// AutoRecharge is an account's rule plus where it stands.
type AutoRecharge struct {
	AutoRechargeSettings
	// Configured is false until the customer has saved settings once; the
	// settings then hold the suggested defaults.
	Configured          bool
	ConsecutiveFailures int
	// DisabledReason says why it was switched off automatically.
	DisabledReason string
	// SpentThisMonth is what automatic charges have used of the monthly cap.
	SpentThisMonth float64
	// HasCard is true when the account has a valid saved card to charge.
	HasCard bool
	// CardBrand / CardLast4 name the card that would be charged.
	CardBrand string
	CardLast4 string
}

func validateAutoRecharge(in AutoRechargeSettings) error {
	if _, err := topUpAmountCents(in.Amount); err != nil {
		return fmt.Errorf("%w: the recharge amount must be between $%.0f and $%.0f", ErrAutoRechargeInvalid, MinTopUp, MaxTopUp)
	}
	if in.Threshold < MinAutoThreshold || math.IsNaN(in.Threshold) || in.Threshold > 100000 {
		return fmt.Errorf("%w: the threshold must be at least $%.0f", ErrAutoRechargeInvalid, MinAutoThreshold)
	}
	if in.MonthlyCap < in.Amount || in.MonthlyCap > MaxAutoMonthlyCap || math.IsNaN(in.MonthlyCap) {
		return fmt.Errorf("%w: the monthly cap must be at least the recharge amount and at most $%.0f", ErrAutoRechargeInvalid, MaxAutoMonthlyCap)
	}
	return nil
}

func monthStartUTC(now time.Time) time.Time {
	n := now.UTC()
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// autoRechargeSpent is what automatic charges have used of this month's cap.
// Charges still in flight count: they are about to be spent.
func (s *Service) autoRechargeSpent(ctx context.Context, accountID uuid.UUID) (float64, error) {
	var spent sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		SELECT SUM(amount) FROM billing.credit_topups
		WHERE account_id = $1 AND source = 'auto'
		  AND status IN ('pending', 'processing', 'succeeded') AND created_at >= $2`,
		accountID, monthStartUTC(time.Now())).Scan(&spent)
	if err != nil {
		return 0, fmt.Errorf("failed to read automatic recharge spend: %w", err)
	}
	return spent.Float64, nil
}

// savedCard is a card automatic recharge can charge.
type savedCard struct {
	ID         uuid.UUID
	StripePMID string
	Brand      string
	Last4      string
	IsDefault  bool
}

// chargeableCard returns the card automatic recharge charges, or nil when the
// account has none it can use. The default card is preferred; when there is no
// valid default, the most recently verified card that has not expired is used.
// A customer with one saved card that was never marked default plainly means
// it to be used, and insisting on the flag would tell them to "add a card"
// while their card sits in the Payments tab.
func (s *Service) chargeableCard(ctx context.Context, accountID uuid.UUID) (*savedCard, error) {
	now := time.Now().UTC()
	var c savedCard
	err := s.db.QueryRowContext(ctx, `
		SELECT id, stripe_payment_method_id, COALESCE(brand, ''), COALESCE(last4, ''), is_default
		FROM billing.payment_methods
		WHERE account_id = $1 AND status = 'verified'
		  AND (exp_year IS NULL OR exp_month IS NULL
		       OR exp_year > $2 OR (exp_year = $2 AND exp_month >= $3))
		ORDER BY is_default DESC, verified_at DESC NULLS LAST, created_at DESC
		LIMIT 1`, accountID, now.Year(), int(now.Month())).
		Scan(&c.ID, &c.StripePMID, &c.Brand, &c.Last4, &c.IsDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the saved card: %w", err)
	}
	return &c, nil
}

// GetAutoRecharge returns the account's rule. Before any is saved it returns
// the suggested defaults, switched off.
func (s *Service) GetAutoRecharge(ctx context.Context, accountID uuid.UUID) (*AutoRecharge, error) {
	out := &AutoRecharge{AutoRechargeSettings: AutoRechargeSettings{
		Threshold: DefaultAutoThreshold, Amount: DefaultAutoAmount, MonthlyCap: DefaultAutoMonthlyCap,
	}}
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT enabled, threshold, amount, monthly_cap, consecutive_failures, disabled_reason
		FROM billing.auto_recharge WHERE account_id = $1`, accountID).
		Scan(&out.Enabled, &out.Threshold, &out.Amount, &out.MonthlyCap, &out.ConsecutiveFailures, &reason)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, fmt.Errorf("failed to read automatic recharge: %w", err)
	default:
		out.Configured = true
		out.DisabledReason = reason.String
	}
	spent, err := s.autoRechargeSpent(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out.SpentThisMonth = spent
	card, err := s.chargeableCard(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if card != nil {
		out.HasCard, out.CardBrand, out.CardLast4 = true, card.Brand, card.Last4
	}
	return out, nil
}

// SetAutoRecharge saves the customer's rule. Turning it on needs a valid
// saved card and records who agreed to be charged automatically, and when.
// userID may be uuid.Nil when it is not known.
func (s *Service) SetAutoRecharge(ctx context.Context, accountID, userID uuid.UUID, in AutoRechargeSettings) error {
	if err := validateAutoRecharge(in); err != nil {
		return err
	}
	if in.Enabled {
		card, err := s.chargeableCard(ctx, accountID)
		if err != nil {
			return err
		}
		if card == nil {
			return ErrNoDefaultCard
		}
		if !card.IsDefault {
			// Make the card being charged the account's default, so the
			// Payments tab shows the same card the customer agreed to. Only when
			// no other card holds the default.
			if _, err := s.db.ExecContext(ctx, `
				UPDATE billing.payment_methods SET is_default = TRUE, updated_at = NOW()
				WHERE id = $1 AND account_id = $2
				  AND NOT EXISTS (SELECT 1 FROM billing.payment_methods
				                  WHERE account_id = $2 AND is_default AND status = 'verified')`,
				card.ID, accountID); err != nil {
				log.Printf("WARN: could not mark card %s as the default for %s: %v", card.ID, accountID, err)
			}
		}
	}
	var by any
	if userID != uuid.Nil {
		by = userID
	}
	// Switching on (from off) clears the failure count, the reason it was off
	// and any backoff, and records the consent. Editing an already-on rule
	// leaves those alone.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO billing.auto_recharge
			(account_id, enabled, threshold, amount, monthly_cap, enabled_by, enabled_at)
		VALUES ($1, $2, $3, $4, $5, $6, CASE WHEN $2 THEN NOW() END)
		ON CONFLICT (account_id) DO UPDATE SET
			consecutive_failures = CASE WHEN $2 AND NOT billing.auto_recharge.enabled
			                            THEN 0 ELSE billing.auto_recharge.consecutive_failures END,
			next_attempt_at      = CASE WHEN $2 AND NOT billing.auto_recharge.enabled
			                            THEN NULL ELSE billing.auto_recharge.next_attempt_at END,
			enabled_by           = CASE WHEN $2 AND NOT billing.auto_recharge.enabled
			                            THEN $6 ELSE billing.auto_recharge.enabled_by END,
			enabled_at           = CASE WHEN $2 AND NOT billing.auto_recharge.enabled
			                            THEN NOW() ELSE billing.auto_recharge.enabled_at END,
			disabled_reason      = NULL,
			enabled = $2, threshold = $3, amount = $4, monthly_cap = $5, updated_at = NOW()`,
		accountID, in.Enabled, in.Threshold, in.Amount, in.MonthlyCap, by)
	if err != nil {
		return fmt.Errorf("failed to save automatic recharge: %w", err)
	}
	return nil
}

// DisableAutoRechargeIfNoCard switches automatic recharge off when the account
// no longer has a valid default card (it was removed). Called after a card is
// removed; a no-op when the rule is off or a valid card remains.
func (s *Service) DisableAutoRechargeIfNoCard(ctx context.Context, accountID uuid.UUID) {
	card, err := s.chargeableCard(ctx, accountID)
	if err != nil || card != nil {
		return
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE billing.auto_recharge
		SET enabled = FALSE, disabled_reason = 'the saved card was removed', next_attempt_at = NULL, updated_at = NOW()
		WHERE account_id = $1 AND enabled`, accountID)
	if err != nil {
		log.Printf("WARN: could not switch off automatic recharge for %s after its card was removed: %v", accountID, err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("Automatic recharge switched off for account %s: its saved card was removed", accountID)
	}
}

// autoRechargeCovers reports whether automatic recharge will keep the account
// funded, so the low-credit warnings can stay quiet: it is on, has not
// failed, and this month's cap still has room for another charge.
func (s *Service) autoRechargeCovers(ctx context.Context, accountID uuid.UUID) (bool, error) {
	var enabled bool
	var amount, cap float64
	var failures int
	err := s.db.QueryRowContext(ctx, `
		SELECT enabled, amount, monthly_cap, consecutive_failures
		FROM billing.auto_recharge WHERE account_id = $1`, accountID).Scan(&enabled, &amount, &cap, &failures)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to read automatic recharge: %w", err)
	}
	if !enabled || failures > 0 {
		return false, nil
	}
	spent, err := s.autoRechargeSpent(ctx, accountID)
	if err != nil {
		return false, err
	}
	return spent+amount <= cap, nil
}

// AutoRechargeFailure describes a charge that did not go through.
type AutoRechargeFailure struct {
	AccountID uuid.UUID
	Amount    float64
	Reason    string
	// Disabled is true when this failure switched automatic recharge off.
	Disabled bool
	// RetryAfter is when the next attempt will be made (zero when Disabled).
	RetryAfter time.Duration
}

// AutoRechargeCap describes an account whose monthly cap stops another charge.
type AutoRechargeCap struct {
	AccountID uuid.UUID
	Amount    float64
	Cap       float64
	Spent     float64
}

// AutoRechargeNotifier tells the customer about outcomes they must act on.
type AutoRechargeNotifier interface {
	AutoRechargeFailed(ctx context.Context, n AutoRechargeFailure)
	AutoRechargeCapReached(ctx context.Context, n AutoRechargeCap)
}

// AutoRecharger runs the recharge loop.
type AutoRecharger struct {
	db       *sql.DB
	billing  *Service
	notify   AutoRechargeNotifier
	interval time.Duration
	stopChan chan struct{}
}

// NewAutoRecharger builds the loop. notify may be nil.
func NewAutoRecharger(db *sql.DB, billing *Service, notify AutoRechargeNotifier) *AutoRecharger {
	return &AutoRecharger{db: db, billing: billing, notify: notify, interval: DefaultAutoRechargeInterval, stopChan: make(chan struct{})}
}

// Start runs the loop until Stop or ctx is cancelled.
func (r *AutoRecharger) Start(ctx context.Context) {
	log.Println("Starting automatic recharge...")
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	r.Sweep(ctx)
	for {
		select {
		case <-ticker.C:
			r.Sweep(ctx)
		case <-r.stopChan:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Stop stops the loop.
func (r *AutoRecharger) Stop() { close(r.stopChan) }

// Sweep attempts a recharge for every enabled account below its threshold.
func (r *AutoRecharger) Sweep(ctx context.Context) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT account_id FROM billing.auto_recharge
		WHERE enabled AND (next_attempt_at IS NULL OR next_attempt_at <= NOW())
		  AND billing.credit_balance(account_id) < threshold`)
	if err != nil {
		log.Printf("WARN: automatic recharge: %v", err)
		return
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		r.Attempt(ctx, id)
	}
}

type autoConfig struct {
	threshold, amount, monthlyCap float64
	failures                      int
}

// claim reserves the account for one attempt, or reports that it is not due.
func (r *AutoRecharger) claim(ctx context.Context, id uuid.UUID) (autoConfig, bool, error) {
	var c autoConfig
	err := r.db.QueryRowContext(ctx, `
		UPDATE billing.auto_recharge
		SET last_attempt_at = NOW(), next_attempt_at = NOW() + make_interval(secs => $2), updated_at = NOW()
		WHERE account_id = $1 AND enabled AND (next_attempt_at IS NULL OR next_attempt_at <= NOW())
		RETURNING threshold, amount, monthly_cap, consecutive_failures`,
		id, autoClaimWindow.Seconds()).Scan(&c.threshold, &c.amount, &c.monthlyCap, &c.failures)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, fmt.Errorf("cannot claim account: %w", err)
	}
	return c, true, nil
}

// Attempt makes one automatic recharge for the account if it is due and
// allowed. Every path that does not charge leaves the account reserved for a
// while (autoClaimWindow) so it is not re-examined every minute.
func (r *AutoRecharger) Attempt(ctx context.Context, accountID uuid.UUID) {
	cfg, ok, err := r.claim(ctx, accountID)
	if err != nil {
		log.Printf("WARN: automatic recharge: %v", err)
		return
	}
	if !ok {
		return
	}

	// The balance may have been topped up since the sweep looked.
	balance, err := r.billing.CreditBalance(ctx, accountID)
	if err != nil {
		log.Printf("WARN: automatic recharge for %s: %v", accountID, err)
		return
	}
	if balance >= cfg.threshold {
		_, _ = r.db.ExecContext(ctx, `UPDATE billing.auto_recharge SET next_attempt_at = NULL WHERE account_id = $1`, accountID)
		return
	}

	// A charge that has not settled yet is still in progress: never start a
	// second one on top of it.
	var inflight bool
	if err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM billing.credit_topups
		WHERE account_id = $1 AND source = 'auto' AND status IN ('pending', 'processing')
		  AND created_at > NOW() - make_interval(secs => $2))`,
		accountID, autoInflightWindow.Seconds()).Scan(&inflight); err != nil {
		log.Printf("WARN: automatic recharge for %s: %v", accountID, err)
		return
	}
	if inflight {
		return
	}

	// The monthly cap.
	spent, err := r.billing.autoRechargeSpent(ctx, accountID)
	if err != nil {
		log.Printf("WARN: automatic recharge for %s: %v", accountID, err)
		return
	}
	if spent+cfg.amount > cfg.monthlyCap {
		r.capReached(ctx, accountID, cfg, spent)
		return
	}

	card, err := r.billing.chargeableCard(ctx, accountID)
	if err != nil {
		log.Printf("WARN: automatic recharge for %s: %v", accountID, err)
		return
	}
	if card == nil {
		r.switchOff(ctx, accountID, cfg, "there is no valid saved card")
		return
	}

	r.charge(ctx, accountID, cfg, card.StripePMID)
}

func (r *AutoRecharger) capReached(ctx context.Context, id uuid.UUID, cfg autoConfig, spent float64) {
	month := monthStartUTC(time.Now())
	res, err := r.db.ExecContext(ctx, `
		UPDATE billing.auto_recharge SET cap_notified_month = $2
		WHERE account_id = $1 AND (cap_notified_month IS NULL OR cap_notified_month < $2)`, id, month)
	if err != nil {
		log.Printf("WARN: automatic recharge for %s: %v", id, err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 && r.notify != nil {
		log.Printf("Automatic recharge for account %s paused: monthly cap of $%.2f reached", id, cfg.monthlyCap)
		r.notify.AutoRechargeCapReached(ctx, AutoRechargeCap{AccountID: id, Amount: cfg.amount, Cap: cfg.monthlyCap, Spent: spent})
	}
}

// charge records the top-up, then charges the card.
func (r *AutoRecharger) charge(ctx context.Context, id uuid.UUID, cfg autoConfig, pm string) {
	if r.billing.stripe == nil {
		log.Printf("WARN: automatic recharge for %s: payments are not configured", id)
		return
	}
	cust, err := r.billing.ensureStripeCustomer(ctx, id)
	if err != nil {
		log.Printf("WARN: automatic recharge for %s: %v", id, err)
		return
	}
	if cust.Status == "closed" {
		r.switchOff(ctx, id, cfg, "the account is closed")
		return
	}
	cents, err := topUpAmountCents(cfg.amount)
	if err != nil {
		r.switchOff(ctx, id, cfg, "the recharge amount is no longer valid")
		return
	}
	amount := float64(cents) / 100

	var topUpID uuid.UUID
	if err := r.db.QueryRowContext(ctx, `
		INSERT INTO billing.credit_topups (account_id, amount, currency, provider, status, source)
		VALUES ($1, $2, $3, 'stripe', 'pending', 'auto') RETURNING id`,
		id, amount, topUpCurrency).Scan(&topUpID); err != nil {
		log.Printf("WARN: automatic recharge for %s: cannot record top-up: %v", id, err)
		return
	}

	piID, status, err := r.billing.stripe.CreateAutoTopUpPaymentIntent(cust.CustomerID, pm, cents,
		strings.ToLower(topUpCurrency), topUpID.String(), cust.AccountNumber, "autotopup-"+topUpID.String())
	if piID != "" {
		// Recorded even for a decline, so Stripe's later failure webhook finds
		// the row and does not report it as an unknown payment.
		if _, uerr := r.db.ExecContext(ctx, `UPDATE billing.credit_topups SET stripe_payment_intent_id = $2, updated_at = NOW() WHERE id = $1`, topUpID, piID); uerr != nil {
			log.Printf("ERROR: automatic recharge %s: charge made (%s) but recording its payment intent failed: %v", topUpID, piID, uerr)
		}
	}
	if err != nil {
		r.failed(ctx, id, cfg, topUpID, err.Error())
		return
	}
	switch status {
	case "succeeded", "processing":
		// The credit and receipt follow when Stripe's webhook confirms it, the
		// same path as a top-up bought in the console.
		if _, err := r.db.ExecContext(ctx, `
			UPDATE billing.auto_recharge SET consecutive_failures = 0, disabled_reason = NULL, updated_at = NOW()
			WHERE account_id = $1`, id); err != nil {
			log.Printf("WARN: automatic recharge for %s: %v", id, err)
		}
		log.Printf("Automatic recharge: charged $%.2f to account %s (payment %s)", amount, id, piID)
	default:
		r.failed(ctx, id, cfg, topUpID, "the card needs to be confirmed by its owner")
	}
}

// failed records a failed charge, backs off, and switches the rule off after
// the third in a row. Tells the customer either way.
func (r *AutoRecharger) failed(ctx context.Context, id uuid.UUID, cfg autoConfig, topUpID uuid.UUID, reason string) {
	reason = strings.TrimSpace(strings.TrimPrefix(reason, "stripe: charge declined:"))
	if reason == "" {
		reason = "the card was declined"
	}
	if _, err := r.db.ExecContext(ctx, `
		UPDATE billing.credit_topups SET status = 'failed', failure_reason = $2, updated_at = NOW()
		WHERE id = $1 AND status != 'succeeded'`, topUpID, reason); err != nil {
		log.Printf("WARN: automatic recharge %s: cannot record the failure: %v", topUpID, err)
	}

	n := cfg.failures + 1
	if n >= MaxAutoFailures {
		if _, err := r.db.ExecContext(ctx, `
			UPDATE billing.auto_recharge
			SET enabled = FALSE, consecutive_failures = $2, disabled_reason = $3, next_attempt_at = NULL, updated_at = NOW()
			WHERE account_id = $1`, id, n, reason); err != nil {
			log.Printf("WARN: automatic recharge for %s: %v", id, err)
		}
		log.Printf("Automatic recharge for account %s switched off after %d failed charges: %s", id, n, reason)
		if r.notify != nil {
			r.notify.AutoRechargeFailed(ctx, AutoRechargeFailure{AccountID: id, Amount: cfg.amount, Reason: reason, Disabled: true})
		}
		return
	}
	wait := autoRetryBackoff[len(autoRetryBackoff)-1]
	if n-1 < len(autoRetryBackoff) {
		wait = autoRetryBackoff[n-1]
	}
	if _, err := r.db.ExecContext(ctx, `
		UPDATE billing.auto_recharge
		SET consecutive_failures = $2, next_attempt_at = NOW() + make_interval(secs => $3), updated_at = NOW()
		WHERE account_id = $1`, id, n, wait.Seconds()); err != nil {
		log.Printf("WARN: automatic recharge for %s: %v", id, err)
	}
	log.Printf("Automatic recharge for account %s failed (%d of %d): %s", id, n, MaxAutoFailures, reason)
	if r.notify != nil {
		r.notify.AutoRechargeFailed(ctx, AutoRechargeFailure{AccountID: id, Amount: cfg.amount, Reason: reason, RetryAfter: wait})
	}
}

// switchOff turns the rule off for a reason no retry can fix.
func (r *AutoRecharger) switchOff(ctx context.Context, id uuid.UUID, cfg autoConfig, reason string) {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE billing.auto_recharge
		SET enabled = FALSE, disabled_reason = $2, next_attempt_at = NULL, updated_at = NOW()
		WHERE account_id = $1`, id, reason); err != nil {
		log.Printf("WARN: automatic recharge for %s: %v", id, err)
		return
	}
	log.Printf("Automatic recharge for account %s switched off: %s", id, reason)
	if r.notify != nil {
		r.notify.AutoRechargeFailed(ctx, AutoRechargeFailure{AccountID: id, Amount: cfg.amount, Reason: reason, Disabled: true})
	}
}
