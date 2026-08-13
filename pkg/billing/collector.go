// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
)

// UsageCollector periodically collects usage metrics
type UsageCollector struct {
	db                 *sql.DB
	billingService     *Service
	collectionInterval time.Duration
	stopChan           chan struct{}
}

// NewUsageCollector creates a new usage collector
func NewUsageCollector(db *sql.DB, billingService *Service) *UsageCollector {
	return &UsageCollector{
		db:                 db,
		billingService:     billingService,
		collectionInterval: 1 * time.Hour, // Collect every hour
		stopChan:           make(chan struct{}),
	}
}

// Start begins periodic usage collection
func (c *UsageCollector) Start(ctx context.Context) {
	log.Println("Starting usage collector...")

	ticker := time.NewTicker(c.collectionInterval)
	defer ticker.Stop()

	// Run immediately on start
	if err := c.collectUsage(ctx); err != nil {
		log.Printf("WARN: usage collection error: %v", err)
	}

	for {
		select {
		case <-ticker.C:
			if err := c.collectUsage(ctx); err != nil {
				log.Printf("WARN: usage collection error: %v", err)
			}
		case <-c.stopChan:
			log.Println("Stopping usage collector...")
			return
		case <-ctx.Done():
			log.Println("Usage collector stopped (context cancelled)")
			return
		}
	}
}

// Stop stops the usage collector
func (c *UsageCollector) Stop() {
	close(c.stopChan)
}

// collectUsage collects usage for all billable instances: running ones,
// plus terminated ones whose final partial interval has not been billed
// yet. Without the terminated pass, everything between the last hourly
// tick and termination — including the entire life of instances shorter
// than one interval — would silently ride free.
func (c *UsageCollector) collectUsage(ctx context.Context) error {
	log.Println("Collecting usage metrics...")

	instances, err := c.getBillableInstances(ctx)
	if err != nil {
		return fmt.Errorf("failed to get billable instances: %w", err)
	}

	if len(instances) == 0 {
		log.Println("No billable instances to collect")
		return nil
	}

	now := time.Now()
	var recordedCount int

	// Rates are read once per collection run so every record in the run is
	// metered consistently, but never cached across runs — admin price
	// changes apply from the next tick.
	vramRate := 0.0
	cpuRate := 0.0
	memRate := 0.0
	rateFetched := false

	for _, inst := range instances {
		// Get last collection time for this instance
		lastCollectionTime, err := c.getLastCollectionTime(ctx, inst.ID)
		if err != nil {
			log.Printf("WARN: failed to get last collection time for %s: %v", inst.ID, err)
			continue
		}

		// If no previous collection, use instance creation time
		if lastCollectionTime.IsZero() {
			lastCollectionTime = inst.CreatedAt
		}

		// Bill up to now for running instances; terminated instances
		// are billed exactly to their termination timestamp.
		endTime := now
		if inst.TerminatedAt != nil && inst.TerminatedAt.Before(endTime) {
			endTime = *inst.TerminatedAt
		}

		// Calculate hours since last collection
		duration := endTime.Sub(lastCollectionTime)
		hours := duration.Hours()

		// Skip if less than 1 minute (avoid tiny charges)
		if duration < 1*time.Minute {
			continue
		}

		if !rateFetched {
			vramRate = c.billingService.VRAMPricePerGBHour(ctx)
			cpuRate = c.billingService.CPUCoreRate(ctx)
			memRate = c.billingService.MemoryGBRate(ctx)
			rateFetched = true
		}

		// Cost by class. A GPU instance is linear on allocated VRAM. A
		// CPU-only instance (home compute) is linear on cores + memory; its
		// rates default to 0, so it costs nothing until an operator sets a
		// price. unitPrice is the per-hour rate for the interval, recorded
		// for transparency on the usage record.
		var unitPrice float64
		if inst.GPUVRAMGB > 0 {
			unitPrice = float64(inst.GPUVRAMGB) * vramRate
		} else {
			unitPrice = float64(inst.CPUUnits)*cpuRate + float64(inst.MemoryGB)*memRate
		}
		cost := unitPrice * hours

		// Record usage
		record := &UsageRecord{
			AccountID:    inst.AccountID,
			ProjectID:    inst.ProjectID,
			InstanceID:   inst.ID,
			ResourceType: inst.InstanceType,
			Quantity:     hours,
			Unit:         "hours",
			UnitPrice:    unitPrice,
			TotalCost:    cost,
			StartTime:    lastCollectionTime,
			EndTime:      endTime,
		}

		if err := c.billingService.RecordUsage(ctx, record); err != nil {
			log.Printf("WARN: failed to record usage for %s: %v", inst.ID, err)
			continue
		}

		// Draw this interval's cost from the account's credit balance
		// first — credits are spent before the card is ever charged. Any
		// remainder is what will bill to the card later. Best-effort: a
		// consumption failure must not undo the usage record (that would
		// lose billable usage), so it is logged and the run continues.
		// ConsumeCredit is idempotent on the usage record, so a retried
		// interval never double-draws.
		if _, err := c.billingService.ConsumeCredit(ctx, record.AccountID, record.ID, record.TotalCost); err != nil {
			log.Printf("WARN: failed to apply credit for usage %s: %v", record.ID, err)
		}

		recordedCount++
	}

	log.Printf("Collected usage for %d instances (total: %d billable)", recordedCount, len(instances))
	return nil
}

// billableInstance represents an instance with billing info. A nil
// TerminatedAt means the instance is still running.
type billableInstance struct {
	ID           string
	AccountID    uuid.UUID
	ProjectID    uuid.UUID
	InstanceType string
	GPUVRAMGB    int
	CPUUnits     int
	MemoryGB     int
	CreatedAt    time.Time
	TerminatedAt *time.Time
}

// getBillableInstances returns instances with unbilled time — GPU and CPU
// alike. Running instances, and terminated instances whose terminated_at lies
// beyond their last billed end_time (the unbilled tail).
//
// The old `gpu_vram_gb > 0` filter is gone: CPU-only instances are now
// metered too (home compute). Whether a CPU instance actually costs anything
// depends on the CPU/memory rates, which default to 0.
func (c *UsageCollector) getBillableInstances(ctx context.Context) ([]billableInstance, error) {
	query := `
		SELECT i.id, i.account_id, i.project_id, COALESCE(i.instance_type_id, ''),
		       COALESCE(i.gpu_vram_gb, 0), COALESCE(i.cpu_units, 0),
		       COALESCE(i.memory_gb, 0), i.created_at, i.terminated_at
		FROM compute.instances i
		LEFT JOIN LATERAL (
			SELECT MAX(end_time) AS last_end
			FROM billing.usage_records ur
			WHERE ur.instance_id = i.id
		) b ON true
		WHERE (i.status = 'running' AND i.terminated_at IS NULL)
			OR (i.terminated_at IS NOT NULL
			    AND i.terminated_at > COALESCE(b.last_end, i.created_at) + interval '1 minute')
	`

	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	var instances []billableInstance
	for rows.Next() {
		var inst billableInstance
		if err := rows.Scan(&inst.ID, &inst.AccountID, &inst.ProjectID, &inst.InstanceType,
			&inst.GPUVRAMGB, &inst.CPUUnits, &inst.MemoryGB, &inst.CreatedAt, &inst.TerminatedAt); err != nil {
			return nil, fmt.Errorf("scan failed: %w", err)
		}
		instances = append(instances, inst)
	}

	return instances, rows.Err()
}

// getLastCollectionTime gets the last time usage was collected for an instance
func (c *UsageCollector) getLastCollectionTime(ctx context.Context, instanceID string) (time.Time, error) {
	query := `
		SELECT MAX(end_time)
		FROM billing.usage_records
		WHERE instance_id = $1
	`

	var lastTime sql.NullTime
	err := c.db.QueryRowContext(ctx, query, instanceID).Scan(&lastTime)
	if err != nil && err != sql.ErrNoRows {
		return time.Time{}, fmt.Errorf("query failed: %w", err)
	}

	if lastTime.Valid {
		return lastTime.Time, nil
	}

	return time.Time{}, nil
}
