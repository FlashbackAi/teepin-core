// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/email"
)

// DefaultAlertInterval is how often accounts are checked for a warning.
const DefaultAlertInterval = 5 * time.Minute

// topUpEpsilon is how much the balance must rise between two checks to count
// as credit having been added. Spending only lowers the balance, so any rise
// is a top-up or a grant; the epsilon just ignores rounding.
const topUpEpsilon = 0.01

// alertState is one account's place on the warning ladder.
type alertState struct {
	Level       AlertLevel
	LastBalance float64
	// Exists is false for an account never evaluated before.
	Exists bool
}

// alertDecision is what to do about an account this round.
type alertDecision struct {
	NewLevel AlertLevel
	// Fire is true when a warning must be sent for NewLevel.
	Fire bool
}

// decideAlert is the whole warning policy, kept pure so it can be tested
// exhaustively.
//
//   - Each level is sent once: a warning fires only when the account has
//     moved to a worse level than the last one it was warned about.
//   - Credit being added (the balance rose) re-arms the ladder silently: the
//     account starts again from wherever it now stands, so the next slide
//     toward zero is warned about again.
//   - An account with nothing at risk (not spending, nothing on hold) is
//     reset, so a later launch on a thin balance can warn afresh.
//   - Otherwise the level never moves down, so a warning is not repeated as
//     spending fluctuates around a threshold.
func decideAlert(st alertState, r RunwayReport) alertDecision {
	switch {
	case st.Exists && r.Balance > st.LastBalance+topUpEpsilon:
		return alertDecision{NewLevel: r.Level}
	case r.Level > st.Level:
		return alertDecision{NewLevel: r.Level, Fire: true}
	case r.Level == LevelNone && !r.Impacted:
		return alertDecision{NewLevel: LevelNone}
	default:
		return alertDecision{NewLevel: st.Level}
	}
}

// CreditAlerter sends the low-credit warnings. Warnings are mandatory service
// notices to the account's owners and admins: they protect running services
// and stored data, so there is no per-account opt-out.
type CreditAlerter struct {
	db         *sql.DB
	billing    *Service
	sender     email.Sender
	consoleURL string
	interval   time.Duration
	stopChan   chan struct{}
}

// NewCreditAlerter builds the alerter. consoleURL is the console's base URL,
// used for the Add credit link.
func NewCreditAlerter(db *sql.DB, billing *Service, sender email.Sender, consoleURL string) *CreditAlerter {
	return &CreditAlerter{
		db: db, billing: billing, sender: sender,
		consoleURL: strings.TrimRight(consoleURL, "/"),
		interval:   DefaultAlertInterval, stopChan: make(chan struct{}),
	}
}

// Start runs the alert loop until Stop or ctx is cancelled.
func (a *CreditAlerter) Start(ctx context.Context) {
	log.Println("Starting credit alerter...")
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	a.Sweep(ctx)
	for {
		select {
		case <-ticker.C:
			a.Sweep(ctx)
		case <-a.stopChan:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Stop stops the loop.
func (a *CreditAlerter) Stop() { close(a.stopChan) }

// Sweep checks every account that has something at stake.
func (a *CreditAlerter) Sweep(ctx context.Context) {
	ids, err := a.candidates(ctx)
	if err != nil {
		log.Printf("WARN: credit alerter: %v", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		report, err := a.billing.Runway(ctx, id)
		if err != nil {
			log.Printf("WARN: credit alerter: runway for %s: %v", id, err)
			continue
		}
		a.evaluate(ctx, id, report)
	}
}

// candidates are the accounts worth checking: those running or holding
// something, storing data, spending lately, or already mid-ladder.
func (a *CreditAlerter) candidates(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT DISTINCT account_id FROM (
			SELECT account_id FROM compute.instances
				WHERE terminated_at IS NULL AND status IN ('running', 'stopped')
			UNION SELECT account_id FROM storage.buckets WHERE deleted_at IS NULL
			UNION SELECT account_id FROM billing.credit_transactions
				WHERE kind = 'consumption' AND created_at > NOW() - INTERVAL '24 hours'
			UNION SELECT account_id FROM billing.credit_alert_state WHERE level > 0
		) x WHERE account_id IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("cannot list accounts to check: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (a *CreditAlerter) loadState(ctx context.Context, id uuid.UUID) (alertState, error) {
	var level int
	var last float64
	err := a.db.QueryRowContext(ctx,
		`SELECT level, last_balance FROM billing.credit_alert_state WHERE account_id = $1`, id).Scan(&level, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return alertState{}, nil
	}
	if err != nil {
		return alertState{}, fmt.Errorf("cannot read alert state: %w", err)
	}
	return alertState{Level: AlertLevel(level), LastBalance: last, Exists: true}, nil
}

// swapState moves the account from oldLevel to newLevel atomically: it
// succeeds only if the level is still oldLevel, so two control-plane
// replicas cannot both send the same warning. The row is created on first use.
func (a *CreditAlerter) swapState(ctx context.Context, id uuid.UUID, oldLevel, newLevel AlertLevel, balance float64, fired bool) (bool, error) {
	if _, err := a.db.ExecContext(ctx,
		`INSERT INTO billing.credit_alert_state (account_id, level, last_balance) VALUES ($1, 0, $2)
		 ON CONFLICT (account_id) DO NOTHING`, id, balance); err != nil {
		return false, fmt.Errorf("cannot create alert state: %w", err)
	}
	res, err := a.db.ExecContext(ctx, `
		UPDATE billing.credit_alert_state
		SET level = $3, last_balance = $4, updated_at = NOW(),
		    notified_at = CASE WHEN $5 THEN NOW() ELSE notified_at END
		WHERE account_id = $1 AND level = $2`, id, int(oldLevel), int(newLevel), balance, fired)
	if err != nil {
		return false, fmt.Errorf("cannot update alert state: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// evaluate applies the policy to one account and sends the warning if due.
func (a *CreditAlerter) evaluate(ctx context.Context, id uuid.UUID, report RunwayReport) {
	st, err := a.loadState(ctx, id)
	if err != nil {
		log.Printf("WARN: credit alerter: %v", err)
		return
	}
	d := decideAlert(st, report)
	if !d.Fire && d.NewLevel == st.Level && st.Exists && report.Balance == st.LastBalance {
		return
	}
	a.apply(ctx, id, st, d, report)
}

// apply records the decision and, when it calls for a warning, sends it. The
// level is claimed first (compare-and-swap) and given back if sending fails,
// so a failed send is retried on the next sweep instead of being lost.
func (a *CreditAlerter) apply(ctx context.Context, id uuid.UUID, st alertState, d alertDecision, report RunwayReport) {
	claimed, err := a.swapState(ctx, id, st.Level, d.NewLevel, report.Balance, d.Fire)
	if err != nil {
		log.Printf("WARN: credit alerter: %v", err)
		return
	}
	if !claimed || !d.Fire {
		return
	}
	if err := a.send(ctx, id, d.NewLevel, report); err != nil {
		log.Printf("WARN: credit alerter: %s warning for account %s not sent (will retry): %v", d.NewLevel, id, err)
		// Only the level goes back. The balance stays current: restoring an
		// older (or zero) balance would make the next sweep read the whole
		// balance as freshly added credit and silently skip the retry.
		if _, rerr := a.swapState(ctx, id, d.NewLevel, st.Level, report.Balance, false); rerr != nil {
			log.Printf("WARN: credit alerter: could not roll back alert state for %s: %v", id, rerr)
		}
		return
	}
	log.Printf("Credit alerter: sent %s warning to account %s (balance %.2f, spending %.4f/hour)", d.NewLevel, id, report.Balance, report.BurnPerHour)
}

// RaiseOutOfCredit sends the out-of-credit notice when the credit enforcer
// has just stopped an account's compute. The enforcer acts a couple of
// minutes before the balance reaches zero, so the periodic check alone could
// see a small positive balance and nothing spending, and never say so.
func (a *CreditAlerter) RaiseOutOfCredit(ctx context.Context, accountID uuid.UUID) {
	st, err := a.loadState(ctx, accountID)
	if err != nil {
		log.Printf("WARN: credit alerter: %v", err)
		return
	}
	if st.Level >= LevelOutOfCredit {
		return
	}
	report, err := a.billing.Runway(ctx, accountID)
	if err != nil {
		log.Printf("WARN: credit alerter: runway for %s: %v", accountID, err)
		return
	}
	report.Level = LevelOutOfCredit
	a.apply(ctx, accountID, st, alertDecision{NewLevel: LevelOutOfCredit, Fire: true}, report)
}

// recipients are the owners and admins of the account.
func (a *CreditAlerter) recipients(ctx context.Context, id uuid.UUID) ([]string, error) {
	return accountRecipients(ctx, a.db, id)
}

func (a *CreditAlerter) send(ctx context.Context, id uuid.UUID, level AlertLevel, report RunwayReport) error {
	to, err := a.recipients(ctx, id)
	if err != nil {
		return err
	}
	if len(to) == 0 {
		// Nobody to tell. Not an error worth retrying every sweep.
		log.Printf("WARN: credit alerter: account %s has no owner or admin with an email address", id)
		return nil
	}
	var deleteAfter *time.Time
	if hold, err := a.billing.ActiveStorageHold(ctx, id); err == nil && hold != nil {
		deleteAfter = &hold.DeleteAfter
	}
	msg := AlertMessage(level, report, a.consoleURL+"/billing/credits", deleteAfter)
	msg.To = to
	return a.sender.Send(ctx, msg)
}

// AlertMessage writes the warning email for a level. It says how much is
// left, how long that lasts, what happens at zero, and links to add credit.
func AlertMessage(level AlertLevel, r RunwayReport, addCreditURL string, deleteAfter *time.Time) email.Message {
	balance := fmt.Sprintf("$%.2f", maxFloat(r.Balance, 0))
	var subject, lead string
	switch level {
	case LevelOutOfCredit:
		subject = "Your Teepin credit has run out"
		lead = "Your Teepin credit is used up (" + balance + " left). Running services that cost money have been stopped."
	case LevelSixHours:
		subject = "About 6 hours of Teepin credit left"
		lead = "You have " + balance + " of Teepin credit left, about " + humanRunway(r) + " at your current usage."
	case LevelDay:
		subject = "About a day of Teepin credit left"
		lead = "You have " + balance + " of Teepin credit left, about " + humanRunway(r) + " at your current usage."
	default:
		subject = "Your Teepin credit is running low"
		lead = "You have " + balance + " of Teepin credit left, about " + humanRunway(r) + " at your current usage."
	}

	var consequences string
	if level == LevelOutOfCredit {
		consequences = "Your stored data (object storage and the disks of stopped instances) is kept but cannot be accessed"
		if deleteAfter != nil {
			consequences += ". Add credit before " + deleteAfter.UTC().Format("2 Jan 2006 15:04 UTC") + " or it will be permanently deleted."
		} else {
			consequences += ", and is permanently deleted 7 days after your balance reaches zero unless you add credit first."
		}
		consequences += " After adding credit, start your stopped instances from the console."
	} else {
		consequences = "When your credit reaches zero, running services are stopped, and your stored data is held for 7 days and then permanently deleted unless you add credit."
	}

	text := lead + "\n\n" + consequences + "\n\nAdd credit: " + addCreditURL +
		"\n\nYou are receiving this because you are an owner or admin of this Teepin account. These notices protect your running services and data, so they cannot be turned off.\n"

	htmlBody := "<p>" + html.EscapeString(lead) + "</p><p>" + html.EscapeString(consequences) +
		"</p><p><a href=\"" + html.EscapeString(addCreditURL) + "\">Add credit</a></p>" +
		"<p style=\"color:#666;font-size:12px\">You are receiving this because you are an owner or admin of this Teepin account. " +
		"These notices protect your running services and data, so they cannot be turned off.</p>"

	return email.Message{Subject: subject, Text: text, HTML: htmlBody}
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// humanRunway words the time left: "3 days", "about 5 hours", "less than an hour".
func humanRunway(r RunwayReport) string {
	hours, ok := r.RunwayHours()
	if !ok {
		return "an unknown time"
	}
	switch {
	case hours < 1:
		return "less than an hour"
	case hours < 48:
		n := int(hours + 0.5)
		if n == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", n)
	default:
		return fmt.Sprintf("%d days", int(hours/24+0.5))
	}
}
