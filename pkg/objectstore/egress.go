// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package objectstore

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

type egressKey struct {
	accountID, projectID, bucketID uuid.UUID
}

// EgressTracker batches per-bucket transferred-byte counts in memory and
// flushes them as usage records on a fixed interval, rather than one
// billing.usage_records insert per download — real write amplification
// on a busy bucket otherwise. A single fixed interval, deliberately, not
// a size-threshold trigger too: one trigger path is enough to bound both
// how stale the billed total can be and how much unflushed data sits in
// memory, without needing two paths to reason about.
type EgressTracker struct {
	billingService *billing.Service
	interval       time.Duration

	mu      sync.Mutex
	pending map[egressKey]int64
}

// NewEgressTracker builds a tracker. interval <= 0 falls back to 60s.
func NewEgressTracker(billingService *billing.Service, interval time.Duration) *EgressTracker {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &EgressTracker{
		billingService: billingService,
		interval:       interval,
		pending:        map[egressKey]int64{},
	}
}

// Record adds n bytes transferred for one bucket. Callers must pass the
// bytes ACTUALLY written to the client after the stream completes or is
// cut short — never the object's full Content-Length up front, which
// would over-bill a download a customer cancelled partway through.
func (t *EgressTracker) Record(accountID, projectID, bucketID uuid.UUID, n int64) {
	if n <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending[egressKey{accountID, projectID, bucketID}] += n
}

// Start flushes accumulated egress on a fixed interval until ctx is
// cancelled — callers run this via `go tracker.Start(ctx)`.
func (t *EgressTracker) Start(ctx context.Context) {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.flush(ctx)
		case <-ctx.Done():
			t.flush(ctx) // best-effort final flush rather than losing the last interval
			return
		}
	}
}

func (t *EgressTracker) flush(ctx context.Context) {
	t.mu.Lock()
	batch := t.pending
	t.pending = map[egressKey]int64{}
	t.mu.Unlock()

	if len(batch) == 0 {
		return
	}

	// Checked once per flush, not per Record call: Record runs on every
	// download's hot path, and a live rate lookup there would add a query
	// to every request just to decide whether to keep a number already
	// sitting in memory for free.
	rate := t.billingService.ObjectStorageEgressGBRate(ctx)
	if rate <= 0 {
		// Deliberately dropped, not re-queued: egress tracking exists
		// only to bill it, and traffic from before a rate existed should
		// never be retroactively charged once one is set later.
		return
	}

	now := time.Now()
	for key, bytes := range batch {
		gb := float64(bytes) / (1 << 30)
		record := &billing.UsageRecord{
			AccountID:    key.accountID,
			ProjectID:    key.projectID,
			SubjectType:  SubjectTypeBucket,
			SubjectID:    key.bucketID.String(),
			ResourceType: "object_storage_gb_egress",
			Quantity:     gb,
			Unit:         "gb",
			UnitPrice:    rate,
			TotalCost:    gb * rate,
			StartTime:    now,
			EndTime:      now,
		}
		if err := t.billingService.RecordUsage(ctx, record); err != nil {
			log.Printf("WARN: objectstore egress: failed to record usage for bucket %s: %v", key.bucketID, err)
			continue
		}
		if _, err := t.billingService.ConsumeCredit(ctx, record.AccountID, record.ID, record.TotalCost); err != nil {
			log.Printf("WARN: objectstore egress: failed to apply credit for usage %s: %v", record.ID, err)
		}
	}
}
