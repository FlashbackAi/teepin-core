// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/google/uuid"
)

// BillingCycle generates and issues monthly usage invoices automatically.
//
// On the 1st of each month it closes out the PREVIOUS calendar month:
// for every account with metered usage in that window it generates a
// draft usage invoice and issues it (which also renders the PDF). This is
// the automation that turns "an operator clicks Generate" into a real
// monthly bill, matching how a cloud provider bills usage.
//
// It AUTO-ISSUES rather than leaving drafts: a usage invoice is computed
// from usage_records — the same data the customer already sees — so there
// is no hand-typed figure for a human to check, and holding it in draft
// would be manual toil with no safety benefit. Two guards keep that safe:
// no usage means no invoice, and an account already invoiced for a period
// is never invoiced again.
type BillingCycle struct {
	db             *sql.DB
	billingService *Service
	interval       time.Duration
	stopChan       chan struct{}

	// lastRunMonth guards against running twice in one month if the ticker
	// fires several times on the 1st or the process restarts that day.
	// "YYYY-MM" of the period already processed.
	lastRunMonth string
}

// NewBillingCycle creates the monthly billing job.
func NewBillingCycle(db *sql.DB, billingService *Service) *BillingCycle {
	return &BillingCycle{
		db:             db,
		billingService: billingService,
		interval:       1 * time.Hour, // wake hourly; act only on the 1st
		stopChan:       make(chan struct{}),
	}
}

// Start begins the monthly cycle. It wakes hourly and acts only when the
// date is the 1st and the previous month has not yet been processed —
// robust to the exact tick time and to restarts.
func (b *BillingCycle) Start(ctx context.Context) {
	log.Println("Starting monthly billing cycle...")

	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	b.maybeRun(ctx)

	for {
		select {
		case <-ticker.C:
			b.maybeRun(ctx)
		case <-b.stopChan:
			log.Println("Stopping monthly billing cycle...")
			return
		case <-ctx.Done():
			log.Println("Monthly billing cycle stopped (context cancelled)")
			return
		}
	}
}

// Stop stops the cycle.
func (b *BillingCycle) Stop() { close(b.stopChan) }

// maybeRun runs the cycle if today is the 1st and the prior month has not
// already been processed this run.
func (b *BillingCycle) maybeRun(ctx context.Context) {
	now := time.Now().UTC()
	if now.Day() != 1 {
		return
	}
	periodStart, periodEnd := previousMonth(now)
	monthKey := periodStart.Format("2006-01")
	if b.lastRunMonth == monthKey {
		return
	}

	log.Printf("Running monthly billing for %s (%s → %s)",
		monthKey, periodStart.Format("2006-01-02"), periodEnd.Format("2006-01-02"))
	if err := b.run(ctx, periodStart, periodEnd); err != nil {
		log.Printf("WARN: monthly billing run failed: %v", err)
		return // leave lastRunMonth unset so the next tick retries
	}
	b.lastRunMonth = monthKey
}

// run generates and issues a usage invoice for every account that has
// usage in the period and has not already been invoiced for it.
func (b *BillingCycle) run(ctx context.Context, periodStart, periodEnd time.Time) error {
	accounts, err := b.accountsWithUsage(ctx, periodStart, periodEnd)
	if err != nil {
		return err
	}

	issued := 0
	for _, accountID := range accounts {
		// Idempotency: skip an account already invoiced for this exact
		// period. A re-run, a restart mid-sweep, or two tasks racing must
		// never double-bill.
		already, err := b.hasUsageInvoiceForPeriod(ctx, accountID, periodStart, periodEnd)
		if err != nil {
			log.Printf("WARN: billing cycle: idempotency check failed for %s: %v", accountID, err)
			continue
		}
		if already {
			continue
		}

		invoice, err := b.billingService.CreateAccountUsageInvoice(ctx, accountID, periodStart, periodEnd)
		if err != nil {
			// "no billable usage" is expected for accounts that appeared in
			// the usage query only via a zeroing edge — skip quietly;
			// anything else is logged.
			log.Printf("billing cycle: no invoice for %s: %v", accountID, err)
			continue
		}
		if _, err := b.billingService.IssueInvoice(ctx, invoice.ID); err != nil {
			// The draft exists; issuing failed (e.g. transient). An
			// operator can issue it by hand, and the next run's idempotency
			// check will see it and not duplicate.
			log.Printf("WARN: billing cycle: created draft %s for %s but issuing failed: %v",
				invoice.InvoiceNumber, accountID, err)
			continue
		}
		issued++
	}

	log.Printf("Monthly billing complete: %d invoice(s) issued across %d account(s) with usage",
		issued, len(accounts))
	return nil
}

// accountsWithUsage returns the distinct accounts that have any metered
// usage in the period.
func (b *BillingCycle) accountsWithUsage(ctx context.Context, start, end time.Time) ([]uuid.UUID, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT DISTINCT p.account_id
		FROM billing.usage_records u
		JOIN auth.projects p ON p.id = u.project_id
		WHERE u.start_time >= $1 AND u.end_time <= $2
	`, start, end)
	if err != nil {
		return nil, err
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

// hasUsageInvoiceForPeriod reports whether a usage invoice already covers
// this account+period — the idempotency guard.
func (b *BillingCycle) hasUsageInvoiceForPeriod(ctx context.Context, accountID uuid.UUID, start, end time.Time) (bool, error) {
	var n int
	err := b.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM billing.invoices
		WHERE account_id = $1 AND source = 'usage'
		  AND period_start = $2 AND period_end = $3
	`, accountID, start, end).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// previousMonth returns the first instant of the previous calendar month
// and the last instant of it, in UTC. A mid-month signup needs no special
// handling: the period is always the whole calendar month, and the usage
// aggregation only picks up what actually ran.
func previousMonth(now time.Time) (start, end time.Time) {
	firstOfThis := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	start = firstOfThis.AddDate(0, -1, 0)
	// End: the last second of the previous month.
	end = firstOfThis.Add(-1 * time.Second)
	return start, end
}
