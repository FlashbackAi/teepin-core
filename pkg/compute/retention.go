// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package compute

import (
	"context"
	"log"
	"time"
)

// retentionSweepInterval is how often the purge runs. Daily is plenty —
// this trims history, it does not need to react quickly to anything.
const retentionSweepInterval = 24 * time.Hour

// RetentionWindow is how long a fully-billed terminated instance's row
// survives before RetentionSweeper deletes it — long enough for console
// history, support, and audit lookups to still find it, short enough
// that compute.instances does not grow unbounded forever. Matches this
// codebase's existing 30-day default for operational history (see
// teepin-control-plane's log_retention_days), and is vastly longer than
// the reconciler's own 1-hour reviveWindow, so a sweep can never race a
// legitimate revive.
const RetentionWindow = 30 * 24 * time.Hour

// RetentionSweeper periodically purges terminated compute.instances rows
// once they are both fully billed and older than RetentionWindow — see
// Store.PurgeFullyBilledTerminated's own doc comment for what "fully
// billed" means. Mirrors billing.UsageCollector's own Start/ticker shape.
// Found live 2026-09-02: nothing previously purged this table at all, so
// every instance ever created — including every ephemeral Kumbha
// build/agent/screenshot pod — accumulated as a permanent row.
type RetentionSweeper struct {
	store *Store
}

// NewRetentionSweeper builds a sweeper against store.
func NewRetentionSweeper(store *Store) *RetentionSweeper {
	return &RetentionSweeper{store: store}
}

// Start runs the purge immediately, then on retentionSweepInterval, until
// ctx is cancelled. A failed sweep is logged and retried on the next
// tick, never fatal — a slowly growing table is not worth crashing the
// control plane over.
func (r *RetentionSweeper) Start(ctx context.Context) {
	log.Println("Starting compute instance retention sweeper...")

	sweep := func() {
		n, err := r.store.PurgeFullyBilledTerminated(ctx, RetentionWindow)
		if err != nil {
			log.Printf("WARN: instance retention sweep failed: %v", err)
		} else if n > 0 {
			log.Printf("Instance retention sweep purged %d fully-billed terminated instance(s)", n)
		}

		// Same window, same cadence, same "log and retry next tick"
		// posture — instance_metrics rows have no billing/audit weight
		// (see migration 038's own comment), so this purge is independent
		// of whether the instance itself is still active.
		if n, err := r.store.PurgeOldInstanceMetrics(ctx, RetentionWindow); err != nil {
			log.Printf("WARN: instance metrics retention sweep failed: %v", err)
		} else if n > 0 {
			log.Printf("Instance metrics retention sweep purged %d old row(s)", n)
		}
	}

	sweep()

	ticker := time.NewTicker(retentionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sweep()
		case <-ctx.Done():
			log.Println("Stopping compute instance retention sweeper...")
			return
		}
	}
}
