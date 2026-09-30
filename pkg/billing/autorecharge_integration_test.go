// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs automatic recharge against a real Postgres. See
// credits_integration_test.go for how to run it.
package billing

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// recordingAutoNotifier captures what the customer would be told.
type recordingAutoNotifier struct {
	failed []AutoRechargeFailure
	caps   []AutoRechargeCap
}

func (r *recordingAutoNotifier) AutoRechargeFailed(_ context.Context, n AutoRechargeFailure) {
	r.failed = append(r.failed, n)
}
func (r *recordingAutoNotifier) AutoRechargeCapReached(_ context.Context, n AutoRechargeCap) {
	r.caps = append(r.caps, n)
}

func itCard(t *testing.T, db *sql.DB, account uuid.UUID, expYear int) string {
	t.Helper()
	pm := "pm_" + uuid.NewString()[:8]
	if _, err := db.Exec(`
		INSERT INTO billing.payment_methods
			(account_id, stripe_customer_id, stripe_payment_method_id, type, last4, brand, exp_month, exp_year,
			 is_default, status, verified_at)
		VALUES ($1, 'cus_it', $2, 'card', '4242', 'visa', 12, $3, TRUE, 'verified', NOW())`,
		account, pm, expYear); err != nil {
		t.Fatalf("seed card: %v", err)
	}
	return pm
}

// perAccountGateway gives each account its own Stripe customer id, as Stripe
// does (the column is unique), while still recording calls on the fake.
type perAccountGateway struct{ *fakeGateway }

func (g perAccountGateway) EnsureCustomer(existingID, email, name, accountNumber string) (string, error) {
	if existingID != "" {
		return existingID, nil
	}
	return "cus_" + accountNumber, nil
}

type autoHarness struct {
	t     *testing.T
	db    *sql.DB
	svc   *Service
	gw    *fakeGateway
	notes *recordingAutoNotifier
	r     *AutoRecharger
	acct  uuid.UUID
}

func newAutoHarness(t *testing.T, balance float64) *autoHarness {
	t.Helper()
	db := integrationDB(t)
	gw := &fakeGateway{}
	svc := NewService(db).WithStripe(perAccountGateway{gw})
	notes := &recordingAutoNotifier{}
	acct := itAccount(t, db)
	if balance > 0 {
		itLot(t, db, acct, "purchase", balance, 0, nil)
	}
	return &autoHarness{t: t, db: db, svc: svc, gw: gw, notes: notes, r: NewAutoRecharger(db, svc, notes), acct: acct}
}

func (h *autoHarness) enable(threshold, amount, cap float64) {
	h.t.Helper()
	if err := h.svc.SetAutoRecharge(context.Background(), h.acct, uuid.New(),
		AutoRechargeSettings{Enabled: true, Threshold: threshold, Amount: amount, MonthlyCap: cap}); err != nil {
		h.t.Fatalf("SetAutoRecharge: %v", err)
	}
}

func (h *autoHarness) due() {
	h.t.Helper()
	if _, err := h.db.Exec(`UPDATE billing.auto_recharge SET next_attempt_at = NULL WHERE account_id = $1`, h.acct); err != nil {
		h.t.Fatal(err)
	}
}

func (h *autoHarness) topUps(where string) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM billing.credit_topups WHERE account_id = $1 AND `+where, h.acct).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *autoHarness) state() (enabled bool, failures int, reason sql.NullString) {
	h.t.Helper()
	if err := h.db.QueryRow(`SELECT enabled, consecutive_failures, disabled_reason FROM billing.auto_recharge WHERE account_id = $1`, h.acct).
		Scan(&enabled, &failures, &reason); err != nil {
		h.t.Fatal(err)
	}
	return
}

func TestAutoRechargeIntegration_ChargesTheSavedCardOnceWhenBelowThreshold(t *testing.T) {
	h := newAutoHarness(t, 4)
	pm := itCard(t, h.db, h.acct, time.Now().Year()+2)
	h.enable(10, 50, 250)

	h.r.Attempt(context.Background(), h.acct)

	if h.gw.autoCalls != 1 || h.gw.lastAutoPM != pm || h.gw.lastAutoCents != 5000 {
		t.Fatalf("charge calls=%d pm=%q cents=%d, want one charge of $50 on %s", h.gw.autoCalls, h.gw.lastAutoPM, h.gw.lastAutoCents, pm)
	}
	if h.topUps("source = 'auto' AND status = 'pending' AND stripe_payment_intent_id LIKE 'pi_auto_%'") != 1 {
		t.Error("the automatic top-up was not recorded with its payment intent")
	}
	// Immediately again (before the webhook credits it): still only one charge.
	h.due()
	h.r.Attempt(context.Background(), h.acct)
	if h.gw.autoCalls != 1 {
		t.Fatalf("charged again while the first was unsettled: %d calls", h.gw.autoCalls)
	}
}

func TestAutoRechargeIntegration_DoesNothingAboveThreshold(t *testing.T) {
	h := newAutoHarness(t, 40)
	itCard(t, h.db, h.acct, time.Now().Year()+2)
	h.enable(10, 50, 250)

	h.r.Sweep(context.Background())
	h.r.Attempt(context.Background(), h.acct)
	// Sweep also visits other tests' accounts in this shared database, so only
	// this account's own charges are checked.
	if n := h.topUps("source = 'auto'"); n != 0 {
		t.Fatalf("charged an account above its threshold (%d charges)", n)
	}
}

func TestAutoRechargeIntegration_OffByDefaultAndWhenDisabled(t *testing.T) {
	h := newAutoHarness(t, 1)
	itCard(t, h.db, h.acct, time.Now().Year()+2)
	h.r.Attempt(context.Background(), h.acct) // no rule at all
	h.enable(10, 50, 250)
	if err := h.svc.SetAutoRecharge(context.Background(), h.acct, uuid.New(),
		AutoRechargeSettings{Enabled: false, Threshold: 10, Amount: 50, MonthlyCap: 250}); err != nil {
		t.Fatal(err)
	}
	h.due()
	h.r.Attempt(context.Background(), h.acct)
	if h.gw.autoCalls != 0 {
		t.Fatalf("charged an account with automatic recharge off")
	}
}

func TestAutoRechargeIntegration_MonthlyCapStopsChargesAndSaysSoOncePerMonth(t *testing.T) {
	h := newAutoHarness(t, 1)
	itCard(t, h.db, h.acct, time.Now().Year()+2)
	h.enable(10, 50, 100)
	// $60 already charged automatically this month (settled).
	if _, err := h.db.Exec(`INSERT INTO billing.credit_topups (account_id, amount, currency, provider, status, source, stripe_payment_intent_id)
		VALUES ($1, 60, 'USD', 'stripe', 'succeeded', 'auto', $2)`, h.acct, "pi_"+uuid.NewString()[:8]); err != nil {
		t.Fatal(err)
	}

	h.r.Attempt(context.Background(), h.acct) // 60 + 50 > 100
	h.due()
	h.r.Attempt(context.Background(), h.acct)
	if h.gw.autoCalls != 0 {
		t.Fatalf("charged past the monthly cap")
	}
	if len(h.notes.caps) != 1 || h.notes.caps[0].Cap != 100 || h.notes.caps[0].Spent != 60 {
		t.Fatalf("cap notices = %+v, want exactly one", h.notes.caps)
	}
}

// A manual purchase does not use up the automatic cap.
func TestAutoRechargeIntegration_ManualTopUpsDoNotCountTowardTheCap(t *testing.T) {
	h := newAutoHarness(t, 1)
	itCard(t, h.db, h.acct, time.Now().Year()+2)
	h.enable(10, 50, 60)
	if _, err := h.db.Exec(`INSERT INTO billing.credit_topups (account_id, amount, currency, provider, status, source, stripe_payment_intent_id)
		VALUES ($1, 500, 'USD', 'stripe', 'succeeded', 'manual', $2)`, h.acct, "pi_"+uuid.NewString()[:8]); err != nil {
		t.Fatal(err)
	}
	h.r.Attempt(context.Background(), h.acct)
	if h.gw.autoCalls != 1 {
		t.Fatalf("a manual purchase blocked automatic recharge")
	}
}

func TestAutoRechargeIntegration_DeclineBacksOffThenSwitchesOffAfterThree(t *testing.T) {
	h := newAutoHarness(t, 1)
	itCard(t, h.db, h.acct, time.Now().Year()+2)
	h.enable(10, 50, 1000)
	h.gw.autoErr = errors.New("stripe: charge declined: Your card has insufficient funds.")

	for i := 1; i <= 3; i++ {
		h.due()
		h.r.Attempt(context.Background(), h.acct)
		enabled, failures, reason := h.state()
		if failures != i {
			t.Fatalf("after failure %d: failures=%d", i, failures)
		}
		if i < 3 && !enabled {
			t.Fatalf("switched off after only %d failures", i)
		}
		if i == 3 {
			if enabled || !reason.Valid || reason.String != "Your card has insufficient funds." {
				t.Fatalf("after 3 failures: enabled=%v reason=%v, want switched off with the reason", enabled, reason)
			}
		}
	}
	if len(h.notes.failed) != 3 || h.notes.failed[0].Disabled || h.notes.failed[0].RetryAfter != 2*time.Hour ||
		h.notes.failed[1].RetryAfter != 8*time.Hour || !h.notes.failed[2].Disabled {
		t.Fatalf("notices = %+v, want retry in 2h, retry in 8h, then switched off", h.notes.failed)
	}
	if h.topUps("source = 'auto' AND status = 'failed'") != 3 {
		t.Errorf("failed attempts were not recorded")
	}
	// Once off, nothing more is charged.
	h.due()
	h.r.Attempt(context.Background(), h.acct)
	if h.gw.autoCalls != 3 {
		t.Errorf("charged after being switched off: %d calls", h.gw.autoCalls)
	}
}

// The wait after a failure is honoured: no retry until it has passed.
func TestAutoRechargeIntegration_RetryWaitsForTheBackoff(t *testing.T) {
	h := newAutoHarness(t, 1)
	itCard(t, h.db, h.acct, time.Now().Year()+2)
	h.enable(10, 50, 1000)
	h.gw.autoErr = errors.New("stripe: charge declined: nope")

	h.r.Attempt(context.Background(), h.acct)
	h.r.Attempt(context.Background(), h.acct) // still inside the 2h backoff
	h.r.Sweep(context.Background())
	if n := h.topUps("source = 'auto'"); n != 1 {
		t.Fatalf("retried before the backoff passed: %d attempts recorded", n)
	}
}

func TestAutoRechargeIntegration_AuthenticationRequiredCountsAsAFailure(t *testing.T) {
	h := newAutoHarness(t, 1)
	itCard(t, h.db, h.acct, time.Now().Year()+2)
	h.enable(10, 50, 1000)
	h.gw.autoStatus = "requires_action"

	h.r.Attempt(context.Background(), h.acct)
	if _, failures, _ := h.state(); failures != 1 || len(h.notes.failed) != 1 {
		t.Fatalf("failures=%d notices=%d, want the card-needs-confirmation case treated as a failure", failures, len(h.notes.failed))
	}
}

func TestAutoRechargeIntegration_SuccessResetsTheFailureCount(t *testing.T) {
	h := newAutoHarness(t, 1)
	itCard(t, h.db, h.acct, time.Now().Year()+2)
	h.enable(10, 50, 1000)
	h.gw.autoErr = errors.New("stripe: charge declined: nope")
	h.r.Attempt(context.Background(), h.acct)
	h.gw.autoErr = nil
	h.due()
	// The failed row is terminal, so the next attempt may proceed.
	h.r.Attempt(context.Background(), h.acct)
	if _, failures, _ := h.state(); failures != 0 {
		t.Fatalf("failures = %d after a successful charge, want reset to 0", failures)
	}
}

func TestAutoRechargeIntegration_NoValidCardSwitchesItOffAndTellsTheCustomer(t *testing.T) {
	h := newAutoHarness(t, 1)
	itCard(t, h.db, h.acct, time.Now().Year()+2)
	h.enable(10, 50, 1000)
	// The card expires (or is gone) before the charge.
	if _, err := h.db.Exec(`UPDATE billing.payment_methods SET exp_year = $2 WHERE account_id = $1`, h.acct, time.Now().Year()-1); err != nil {
		t.Fatal(err)
	}
	h.r.Attempt(context.Background(), h.acct)
	enabled, _, reason := h.state()
	if enabled || h.gw.autoCalls != 0 || !reason.Valid || len(h.notes.failed) != 1 || !h.notes.failed[0].Disabled {
		t.Fatalf("enabled=%v calls=%d reason=%v notices=%+v", enabled, h.gw.autoCalls, reason, h.notes.failed)
	}
}

func TestAutoRechargeIntegration_EnablingNeedsAValidCardAndValidNumbers(t *testing.T) {
	h := newAutoHarness(t, 1)
	ctx := context.Background()
	good := AutoRechargeSettings{Enabled: true, Threshold: 10, Amount: 50, MonthlyCap: 250}

	if err := h.svc.SetAutoRecharge(ctx, h.acct, uuid.Nil, good); !errors.Is(err, ErrNoDefaultCard) {
		t.Errorf("no card: err = %v, want ErrNoDefaultCard", err)
	}
	itCard(t, h.db, h.acct, time.Now().Year()+2)
	for name, bad := range map[string]AutoRechargeSettings{
		"amount under the minimum": {Enabled: true, Threshold: 10, Amount: 19, MonthlyCap: 250},
		"amount over the maximum":  {Enabled: true, Threshold: 10, Amount: 1001, MonthlyCap: 5000},
		"threshold too low":        {Enabled: true, Threshold: 4, Amount: 50, MonthlyCap: 250},
		"cap under the amount":     {Enabled: true, Threshold: 10, Amount: 50, MonthlyCap: 49},
		"cap over the maximum":     {Enabled: true, Threshold: 10, Amount: 50, MonthlyCap: 10001},
	} {
		if err := h.svc.SetAutoRecharge(ctx, h.acct, uuid.Nil, bad); !errors.Is(err, ErrAutoRechargeInvalid) {
			t.Errorf("%s: err = %v, want ErrAutoRechargeInvalid", name, err)
		}
	}
	if err := h.svc.SetAutoRecharge(ctx, h.acct, uuid.New(), good); err != nil {
		t.Fatalf("valid settings refused: %v", err)
	}
	ar, err := h.svc.GetAutoRecharge(ctx, h.acct)
	if err != nil || !ar.Enabled || !ar.Configured || !ar.HasCard || ar.Amount != 50 {
		t.Fatalf("GetAutoRecharge = %+v, %v", ar, err)
	}
}

// Turning it back on after it was switched off clears the failure history.
func TestAutoRechargeIntegration_ReEnablingClearsFailures(t *testing.T) {
	h := newAutoHarness(t, 1)
	itCard(t, h.db, h.acct, time.Now().Year()+2)
	h.enable(10, 50, 1000)
	h.gw.autoErr = errors.New("stripe: charge declined: nope")
	for i := 0; i < 3; i++ {
		h.due()
		h.r.Attempt(context.Background(), h.acct)
	}
	if enabled, _, _ := h.state(); enabled {
		t.Fatal("expected it to be switched off")
	}
	h.enable(10, 50, 1000)
	enabled, failures, reason := h.state()
	if !enabled || failures != 0 || reason.Valid {
		t.Errorf("enabled=%v failures=%d reason=%v, want a clean restart", enabled, failures, reason)
	}
}

func TestAutoRechargeIntegration_RemovingTheCardSwitchesItOff(t *testing.T) {
	h := newAutoHarness(t, 1)
	itCard(t, h.db, h.acct, time.Now().Year()+2)
	h.enable(10, 50, 250)
	if _, err := h.db.Exec(`UPDATE billing.payment_methods SET status = 'removed', is_default = FALSE WHERE account_id = $1`, h.acct); err != nil {
		t.Fatal(err)
	}
	h.svc.DisableAutoRechargeIfNoCard(context.Background(), h.acct)
	if enabled, _, reason := h.state(); enabled || !reason.Valid {
		t.Errorf("enabled=%v reason=%v, want switched off with a reason", enabled, reason)
	}
}

// While automatic recharge is healthy the "running low" warnings stay quiet;
// they return the moment it is failing, capped out, or the account is empty.
func TestAutoRechargeIntegration_QuietsLowCreditWarningsOnlyWhileHealthy(t *testing.T) {
	h := newAutoHarness(t, 30)
	ctx := context.Background()
	rates(t, h.db) // 20GB GPU + 50GB disk = $7/hour
	itInstance(t, ctx, h.svc, h.acct, uniq("it-auto-1"), 20, 50)
	setInstance(t, h.db, uniq("it-auto-1"), "running", 10*60*1e9, -1, -1)
	itCard(t, h.db, h.acct, time.Now().Year()+2)

	r, _ := h.svc.Runway(ctx, h.acct)
	if r.Level == LevelNone {
		t.Fatalf("baseline: $30 at $7/hour should warn, got %v", r.Level)
	}
	h.enable(10, 50, 250)
	r, _ = h.svc.Runway(ctx, h.acct)
	if r.Level != LevelNone || !r.AutoRecharge {
		t.Errorf("healthy auto-recharge: level=%v covered=%v, want quiet", r.Level, r.AutoRecharge)
	}

	// After a failed charge it no longer covers the account.
	if _, err := h.db.Exec(`UPDATE billing.auto_recharge SET consecutive_failures = 1 WHERE account_id = $1`, h.acct); err != nil {
		t.Fatal(err)
	}
	if r, _ = h.svc.Runway(ctx, h.acct); r.Level == LevelNone {
		t.Errorf("warnings stayed quiet though the last charge failed")
	}
	// Nor once this month's cap has no room for another charge.
	if _, err := h.db.Exec(`UPDATE billing.auto_recharge SET consecutive_failures = 0, monthly_cap = 50 WHERE account_id = $1`, h.acct); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO billing.credit_topups (account_id, amount, currency, provider, status, source, stripe_payment_intent_id)
		VALUES ($1, 50, 'USD', 'stripe', 'succeeded', 'auto', $2)`, h.acct, "pi_"+uuid.NewString()[:8]); err != nil {
		t.Fatal(err)
	}
	if r, _ = h.svc.Runway(ctx, h.acct); r.Level == LevelNone {
		t.Errorf("warnings stayed quiet though the monthly cap is used up")
	}
	// Out of credit is never hidden.
	itConsume(t, h.svc, h.acct, 1000)
	if r, _ = h.svc.Runway(ctx, h.acct); r.Level != LevelOutOfCredit {
		t.Errorf("level = %v, want out_of_credit to always show", r.Level)
	}
}
