// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs the credit alerter against a real Postgres. See
// credits_integration_test.go for how to run it.
package billing

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/email"
)

type recordingSender struct {
	sent []email.Message
	fail error
}

func (r *recordingSender) Send(_ context.Context, m email.Message) error {
	if r.fail != nil {
		return r.fail
	}
	r.sent = append(r.sent, m)
	return nil
}

func itUser(t *testing.T, db *sql.DB, account uuid.UUID, role, status string) string {
	t.Helper()
	addr := role + "-" + uuid.NewString()[:8] + "@example.test"
	if _, err := db.Exec(`INSERT INTO auth.users (email, password_hash, account_id, role, status)
		VALUES ($1, 'x', $2, $3, $4)`, addr, account, role, status); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return addr
}

func alertLevelOf(t *testing.T, db *sql.DB, account uuid.UUID) int {
	t.Helper()
	var l int
	if err := db.QueryRow(`SELECT level FROM billing.credit_alert_state WHERE account_id = $1`, account).Scan(&l); err != nil {
		t.Fatalf("read level: %v", err)
	}
	return l
}

// sweepOne evaluates a single account, so other tests' accounts sharing the
// database never affect the result.
func sweepOne(t *testing.T, a *CreditAlerter, s *Service, account uuid.UUID) {
	t.Helper()
	report, err := s.Runway(context.Background(), account)
	if err != nil {
		t.Fatalf("Runway: %v", err)
	}
	a.evaluate(context.Background(), account, report)
}

func TestCreditAlerterIntegration_LadderSendsEachWarningOnceAndRearmsOnTopUp(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	rates(t, db) // 20GB GPU = $2/hour, 50GB disk = $5/hour => $7/hour
	sender := &recordingSender{}
	a := NewCreditAlerter(db, s, sender, "https://console.example/")

	acct := itAccount(t, db)
	owner := itUser(t, db, acct, "owner", "active")
	admin := itUser(t, db, acct, "admin", "active")
	itUser(t, db, acct, "member", "active")
	itUser(t, db, acct, "viewer", "active")
	itUser(t, db, acct, "admin", "disabled")
	itInstance(t, ctx, s, acct, uniq("it-alert-1"), 20, 50)
	setInstance(t, db, uniq("it-alert-1"), "running", 10*60*1e9, -1, -1)

	// $150 at $7/hour is about 21 hours: a day-level warning on first sight.
	itLot(t, db, acct, "purchase", 150, 0, nil)
	sweepOne(t, a, s, acct)
	if len(sender.sent) != 1 {
		t.Fatalf("emails = %d, want 1", len(sender.sent))
	}
	got := append([]string(nil), sender.sent[0].To...)
	sort.Strings(got)
	want := []string{admin, owner}
	sort.Strings(want)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("recipients = %v, want only the active owner and admin %v", got, want)
	}

	// The same level again: nothing more is sent.
	sweepOne(t, a, s, acct)
	sweepOne(t, a, s, acct)
	if len(sender.sent) != 1 {
		t.Fatalf("emails = %d after repeat sweeps, want still 1", len(sender.sent))
	}

	// Spending takes it to six hours: one more, and only one.
	itConsume(t, s, acct, 110) // $40 left = under 6 hours
	sweepOne(t, a, s, acct)
	sweepOne(t, a, s, acct)
	if len(sender.sent) != 2 {
		t.Fatalf("emails = %d after reaching six hours, want 2", len(sender.sent))
	}

	// A top-up re-arms silently...
	itLot(t, db, acct, "purchase", 1000, 0, nil)
	sweepOne(t, a, s, acct)
	if len(sender.sent) != 2 || alertLevelOf(t, db, acct) != int(LevelNone) {
		t.Fatalf("top-up: emails=%d level=%d, want a silent reset", len(sender.sent), alertLevelOf(t, db, acct))
	}
	// ...so sliding toward zero warns again.
	itConsume(t, s, acct, 990) // $50 left => about 7 hours
	sweepOne(t, a, s, acct)
	if len(sender.sent) != 3 {
		t.Fatalf("emails = %d after the second slide, want 3", len(sender.sent))
	}
}

func TestCreditAlerterIntegration_FailedSendIsRetried(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	rates(t, db)
	sender := &recordingSender{fail: errors.New("ses unavailable")}
	a := NewCreditAlerter(db, s, sender, "https://console.example")

	acct := itAccount(t, db)
	itUser(t, db, acct, "owner", "active")
	itInstance(t, ctx, s, acct, uniq("it-alert-2"), 20, 50)
	setInstance(t, db, uniq("it-alert-2"), "running", 10*60*1e9, -1, -1)
	itLot(t, db, acct, "purchase", 30, 0, nil) // about 4 hours

	sweepOne(t, a, s, acct)
	if alertLevelOf(t, db, acct) != int(LevelNone) {
		t.Fatalf("level advanced though the email was not sent")
	}
	sender.fail = nil
	sweepOne(t, a, s, acct)
	if len(sender.sent) != 1 || alertLevelOf(t, db, acct) != int(LevelSixHours) {
		t.Fatalf("emails=%d level=%d, want the warning sent on retry", len(sender.sent), alertLevelOf(t, db, acct))
	}
}

// The enforcer stops compute a couple of minutes before zero; the notice must
// still go out even though the balance is a few cents and nothing is spending.
func TestCreditAlerterIntegration_OutOfCreditNoticeAfterEnforcerStop(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	rates(t, db)
	sender := &recordingSender{}
	a := NewCreditAlerter(db, s, sender, "https://console.example")

	acct := itAccount(t, db)
	itUser(t, db, acct, "owner", "active")
	itLot(t, db, acct, "purchase", 0.03, 0, nil)

	a.RaiseOutOfCredit(context.Background(), acct)
	a.RaiseOutOfCredit(context.Background(), acct) // a second stop does not repeat it
	if len(sender.sent) != 1 || sender.sent[0].Subject != "Your Teepin credit has run out" {
		t.Fatalf("sent = %+v, want exactly the out-of-credit notice", sender.sent)
	}
	// Nothing is spending and nothing is held, so the periodic sweep stays quiet.
	sweepOne(t, a, s, acct)
	if len(sender.sent) != 1 {
		t.Errorf("the sweep repeated the notice")
	}
}

// The runway counts running compute, stopped disks and object storage at
// current rates, and never less than what was really consumed lately.
func TestRunwayIntegration_BurnAndImpact(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	rates(t, db) // GPU 20GB = $2/hour, disk $0.10 per GB-hour

	// Running: 20GB GPU + 50GB disk = $7/hour.
	acct := itAccount(t, db)
	itLot(t, db, acct, "purchase", 70, 0, nil)
	itInstance(t, ctx, s, acct, uniq("it-runway-1"), 20, 50)
	setInstance(t, db, uniq("it-runway-1"), "running", 10*60*1e9, -1, -1)
	r, err := s.Runway(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if !near(r.BurnPerHour, 7, 0.001) || !near(r.Balance, 70, 0.001) {
		t.Fatalf("burn=%v balance=%v, want $7/hour on $70", r.BurnPerHour, r.Balance)
	}
	if hours, ok := r.RunwayHours(); !ok || !near(hours, 10, 0.01) {
		t.Errorf("runway = %v %v, want 10 hours", hours, ok)
	}
	if r.Level != LevelDay || r.Impacted {
		t.Errorf("level=%v impacted=%v, want a day-level warning and nothing on hold", r.Level, r.Impacted)
	}

	// Stopped: only the disk keeps costing ($5/hour), and it counts as impact.
	setInstance(t, db, uniq("it-runway-1"), "stopped", 10*60*1e9, 5*60*1e9, -1)
	r, _ = s.Runway(ctx, acct)
	if !near(r.BurnPerHour, 5, 0.001) || !r.Impacted {
		t.Errorf("stopped: burn=%v impacted=%v, want $5/hour and impacted", r.BurnPerHour, r.Impacted)
	}

	// Real consumption lately beats a lower current rate: $48 consumed in the
	// last day is $2/hour even for an account with nothing running now.
	acct2 := itAccount(t, db)
	itLot(t, db, acct2, "purchase", 100, 0, nil)
	itConsume(t, s, acct2, 48)
	r, _ = s.Runway(ctx, acct2)
	if !near(r.BurnPerHour, 2, 0.01) {
		t.Errorf("burn = %v, want the trailing $2/hour", r.BurnPerHour)
	}
}

func TestBillingMailerIntegration_ReceiptGoesToOwnersAndMentionsStoppedInstances(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	rates(t, db)
	sender := &recordingSender{}
	mailer := NewBillingMailer(db, s, sender, "https://console.example/")

	acct := itAccount(t, db)
	owner := itUser(t, db, acct, "owner", "active")
	itUser(t, db, acct, "member", "active")
	itLot(t, db, acct, "purchase", 80, 0, nil)
	itInstance(t, ctx, s, acct, uniq("it-mail-1"), 20, 50)
	setInstance(t, db, uniq("it-mail-1"), "stopped", 60*60*1e9, 30*60*1e9, -1)

	receipt := uuid.New()
	mailer.TopUpSettled(ctx, TopUpReceiptNotice{
		AccountID: acct, ReceiptID: receipt, ReceiptNumber: "RCT-2026-000042",
		Amount: 50, Currency: "usd", PaymentMethod: "Visa ending 4242",
	})

	if len(sender.sent) != 1 {
		t.Fatalf("emails = %d, want 1", len(sender.sent))
	}
	m := sender.sent[0]
	if len(m.To) != 1 || m.To[0] != owner {
		t.Errorf("recipients = %v, want only the owner %s", m.To, owner)
	}
	for _, want := range []string{"RCT-2026-000042", "$80.00", "choose Start", "https://console.example/billing/invoices/" + receipt.String()} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("email lacks %q:\n%s", want, m.Text)
		}
	}
}

// A sending failure never becomes the payment's problem.
func TestBillingMailerIntegration_SendFailureIsSwallowed(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	mailer := NewBillingMailer(db, s, &recordingSender{fail: errors.New("ses down")}, "https://console.example")
	acct := itAccount(t, db)
	itUser(t, db, acct, "owner", "active")
	mailer.TopUpSettled(context.Background(), TopUpReceiptNotice{AccountID: acct, ReceiptID: uuid.New(), ReceiptNumber: "RCT-1", Amount: 20, Currency: "usd"})
}
