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

// gracePeriod is how long an account may run after its payment method
// goes bad before its resources are suspended. Deliberately generous: we
// are about to stop a customer's running workload, and 24h gives time to
// add a replacement card.
const gracePeriod = 24 * time.Hour

// ResourceSuspender tears down an account's running resources. Defined
// here as an interface so the billing package does not depend on the
// cluster or compute packages — main injects an implementation that has
// the cluster client and the instance store. Returns the number of
// instances stopped, for the audit log.
type ResourceSuspender interface {
	SuspendAccountResources(ctx context.Context, accountID uuid.UUID) (int, error)
}

// SuspensionSweeper suspends accounts whose 24h payment grace period has
// elapsed. It follows the UsageCollector pattern: a ticker, an immediate
// first run, and a select on stop/ctx.
//
// It is INERT until something sets accounts.payment_failed_at — which
// today only happens when a card is removed at Stripe's end (there is no
// charging yet, so no charge can fail). That is deliberate: the mechanism
// is built now, and the charge-failure trigger connects itself when
// charging ships, with nothing able to fire prematurely.
type SuspensionSweeper struct {
	db        *sql.DB
	suspender ResourceSuspender
	interval  time.Duration
	stopChan  chan struct{}
}

// NewSuspensionSweeper creates the sweeper. suspender may be nil, in
// which case accounts are still marked suspended but no cluster teardown
// happens — acceptable for a control plane with no cluster wired, since
// the gate already blocks new launches.
func NewSuspensionSweeper(db *sql.DB, suspender ResourceSuspender) *SuspensionSweeper {
	return &SuspensionSweeper{
		db:        db,
		suspender: suspender,
		interval:  1 * time.Hour,
		stopChan:  make(chan struct{}),
	}
}

// Start begins periodic suspension sweeps.
func (s *SuspensionSweeper) Start(ctx context.Context) {
	log.Println("Starting suspension sweeper...")

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	if err := s.sweep(ctx); err != nil {
		log.Printf("WARN: suspension sweep error: %v", err)
	}

	for {
		select {
		case <-ticker.C:
			if err := s.sweep(ctx); err != nil {
				log.Printf("WARN: suspension sweep error: %v", err)
			}
		case <-s.stopChan:
			log.Println("Stopping suspension sweeper...")
			return
		case <-ctx.Done():
			log.Println("Suspension sweeper stopped (context cancelled)")
			return
		}
	}
}

// Stop stops the sweeper.
func (s *SuspensionSweeper) Stop() { close(s.stopChan) }

// sweep suspends every active account whose grace period has elapsed.
func (s *SuspensionSweeper) sweep(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM auth.accounts
		WHERE status = 'active'
		  AND payment_failed_at IS NOT NULL
		  AND payment_failed_at < NOW() - $1::interval
	`, gracePeriod.String())
	if err != nil {
		return err
	}
	var due []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		due = append(due, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, accountID := range due {
		s.suspendOne(ctx, accountID)
	}
	return nil
}

// suspendOne suspends a single account: flip status, then tear down its
// resources. Order matters — mark suspended FIRST so that even if the
// teardown partially fails, the account cannot launch anything new (the
// gate honours status). Each suspension is logged: stopping a paying
// customer's workload is an operational action that must be auditable.
func (s *SuspensionSweeper) suspendOne(ctx context.Context, accountID uuid.UUID) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE auth.accounts SET status = 'suspended', updated_at = NOW()
		WHERE id = $1 AND status = 'active'
	`, accountID)
	if err != nil {
		log.Printf("WARN: failed to suspend account %s: %v", accountID, err)
		return
	}
	// A concurrent change (someone reactivated, or another sweeper won)
	// means we did not actually suspend — do not tear down resources.
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}

	stopped := 0
	if s.suspender != nil {
		stopped, err = s.suspender.SuspendAccountResources(ctx, accountID)
		if err != nil {
			// The account is suspended (no new launches); a teardown
			// failure is logged for an operator, not retried into a loop.
			log.Printf("WARN: account %s suspended but resource teardown failed: %v", accountID, err)
			return
		}
	}
	log.Printf("account %s suspended after payment grace period; %d instance(s) stopped", accountID, stopped)
}
