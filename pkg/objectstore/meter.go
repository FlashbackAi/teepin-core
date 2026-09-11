// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package objectstore

import (
	"context"
	"log"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

// gbMonthHours matches pkg/billing/collector.go's own hoursPerMonth
// constant and reasoning exactly (365*24/12, the standard average-month
// conversion) — kept as a separate local constant rather than exported
// from pkg/billing, since importing one unexported constant across a
// package boundary isn't possible and duplicating a single well-known
// number is simpler than restructuring pkg/billing to export it.
const gbMonthHours = 730.0

// SubjectTypeBucket is the billing.UsageRecord.SubjectType this package
// records object-storage usage under — one row per bucket, not per
// object, matching how storage.buckets already aggregates object_count
// and total_bytes for exactly this purpose.
const SubjectTypeBucket = "bucket"

// Meter periodically bills every live bucket for GB-month storage,
// modeled directly on pkg/billing.UsageCollector's own shape (ticker,
// run-immediately-on-start, rates read fresh each run and never cached
// across runs, skip intervals under a minute to avoid tiny charges).
//
// Only STORAGE is metered here — GB-transferred (egress) is tracked
// separately (see egress.go) since it has a fundamentally different
// shape: storage is "how much exists right now, sampled hourly";
// egress is "how many bytes were actually written to a response",
// which only makes sense to record as it happens, not on a timer.
type Meter struct {
	store          *Store
	billingService *billing.Service
	interval       time.Duration
}

// NewMeter builds a Meter. interval <= 0 falls back to one hour, matching
// pkg/billing.UsageCollector's own default cadence.
func NewMeter(store *Store, billingService *billing.Service, interval time.Duration) *Meter {
	if interval <= 0 {
		interval = time.Hour
	}
	return &Meter{store: store, billingService: billingService, interval: interval}
}

// Start runs metering cycles until ctx is cancelled — callers run this
// via `go meter.Start(ctx)`.
func (m *Meter) Start(ctx context.Context) {
	if err := m.collect(ctx); err != nil {
		log.Printf("WARN: objectstore meter: %v", err)
	}
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := m.collect(ctx); err != nil {
				log.Printf("WARN: objectstore meter: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// collect bills every live bucket for the storage-GB-hours accrued since
// its own last billed interval (falling back to the bucket's creation
// time if it has never been billed). Skips all the real work entirely
// when the configured rate is 0 — most deployments will never set one,
// and there is no reason to touch every bucket's billing history on a
// schedule just to compute a guaranteed-zero charge.
func (m *Meter) collect(ctx context.Context) error {
	rate := m.billingService.ObjectStorageGBMonthRate(ctx)
	if rate <= 0 {
		return nil
	}

	buckets, err := m.store.ListAllBucketsForMetering(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	var billed int
	for _, b := range buckets {
		lastEnd, err := m.store.LastBillingEndTime(ctx, SubjectTypeBucket, b.ID.String())
		if err != nil {
			log.Printf("WARN: objectstore meter: failed to read last billing time for bucket %s: %v", b.ID, err)
			continue
		}
		if lastEnd.IsZero() {
			lastEnd = b.CreatedAt
		}

		duration := now.Sub(lastEnd)
		if duration < time.Minute {
			continue // avoid tiny charges, same floor as billing/collector.go
		}
		hours := duration.Hours()

		gb := float64(b.TotalBytes) / (1 << 30)
		cost := gb * rate / gbMonthHours * hours

		record := &billing.UsageRecord{
			AccountID:    b.AccountID,
			ProjectID:    b.ProjectID,
			SubjectType:  SubjectTypeBucket,
			SubjectID:    b.ID.String(),
			ResourceType: "object_storage_gb_month",
			Quantity:     gb,
			Unit:         "gb",
			UnitPrice:    rate,
			TotalCost:    cost,
			StartTime:    lastEnd,
			EndTime:      now,
		}
		if err := m.billingService.RecordUsage(ctx, record); err != nil {
			log.Printf("WARN: objectstore meter: failed to record usage for bucket %s: %v", b.ID, err)
			continue
		}

		// Best-effort, same posture as billing/collector.go's own
		// identical call: a failed credit draw must not undo the usage
		// record, since that would lose billable usage. Idempotent per
		// usage record, so a retried interval never double-draws.
		if _, err := m.billingService.ConsumeCredit(ctx, record.AccountID, record.ID, record.TotalCost); err != nil {
			log.Printf("WARN: objectstore meter: failed to apply credit for usage %s: %v", record.ID, err)
		}
		billed++
	}

	log.Printf("objectstore meter: billed %d of %d buckets", billed, len(buckets))
	return nil
}
