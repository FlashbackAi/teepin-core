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
	db              *sql.DB
	billingService  *Service
	collectionInterval time.Duration
	stopChan        chan struct{}
}

// NewUsageCollector creates a new usage collector
func NewUsageCollector(db *sql.DB, billingService *Service) *UsageCollector {
	return &UsageCollector{
		db:              db,
		billingService:  billingService,
		collectionInterval: 1 * time.Hour, // Collect every hour
		stopChan:        make(chan struct{}),
	}
}

// Start begins periodic usage collection
func (c *UsageCollector) Start(ctx context.Context) {
	log.Println("📊 Starting usage collector...")

	ticker := time.NewTicker(c.collectionInterval)
	defer ticker.Stop()

	// Run immediately on start
	if err := c.collectUsage(ctx); err != nil {
		log.Printf("⚠️  Usage collection error: %v", err)
	}

	for {
		select {
		case <-ticker.C:
			if err := c.collectUsage(ctx); err != nil {
				log.Printf("⚠️  Usage collection error: %v", err)
			}
		case <-c.stopChan:
			log.Println("📊 Stopping usage collector...")
			return
		case <-ctx.Done():
			log.Println("📊 Usage collector stopped (context cancelled)")
			return
		}
	}
}

// Stop stops the usage collector
func (c *UsageCollector) Stop() {
	close(c.stopChan)
}

// collectUsage collects usage for all running instances
func (c *UsageCollector) collectUsage(ctx context.Context) error {
	log.Println("📊 Collecting usage metrics...")

	// Get all running instances
	instances, err := c.getRunningInstances(ctx)
	if err != nil {
		return fmt.Errorf("failed to get running instances: %w", err)
	}

	if len(instances) == 0 {
		log.Println("📊 No running instances to collect")
		return nil
	}

	endTime := time.Now()
	var recordedCount int

	for _, inst := range instances {
		// Get last collection time for this instance
		lastCollectionTime, err := c.getLastCollectionTime(ctx, inst.ID)
		if err != nil {
			log.Printf("⚠️  Failed to get last collection time for %s: %v", inst.ID, err)
			continue
		}

		// If no previous collection, use instance creation time
		if lastCollectionTime.IsZero() {
			lastCollectionTime = inst.CreatedAt
		}

		// Calculate hours since last collection
		duration := endTime.Sub(lastCollectionTime)
		hours := duration.Hours()

		// Skip if less than 1 minute (avoid tiny charges)
		if duration < 1*time.Minute {
			continue
		}

		// Calculate cost based on instance type
		cost := c.billingService.CalculateInstanceCost(inst.InstanceType, hours)
		unitPrice := c.getUnitPrice(inst.InstanceType)

		// Record usage
		record := &UsageRecord{
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
			log.Printf("⚠️  Failed to record usage for %s: %v", inst.ID, err)
			continue
		}

		recordedCount++
	}

	log.Printf("✅ Collected usage for %d instances (total: %d running)", recordedCount, len(instances))
	return nil
}

// runningInstance represents a running instance with billing info
type runningInstance struct {
	ID           string
	ProjectID    uuid.UUID
	InstanceType string
	CreatedAt    time.Time
}

// getRunningInstances gets all currently running instances
func (c *UsageCollector) getRunningInstances(ctx context.Context) ([]runningInstance, error) {
	query := `
		SELECT id, project_id, instance_type_id, created_at
		FROM compute.instances
		WHERE status = 'running' AND terminated_at IS NULL
	`

	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	var instances []runningInstance
	for rows.Next() {
		var inst runningInstance
		if err := rows.Scan(&inst.ID, &inst.ProjectID, &inst.InstanceType, &inst.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan failed: %w", err)
		}
		instances = append(instances, inst)
	}

	return instances, nil
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

// getUnitPrice returns the hourly price for an instance type
func (c *UsageCollector) getUnitPrice(instanceType string) float64 {
	pricing := c.billingService.pricing

	switch instanceType {
	case "gpu.h100.1g.10gb":
		return pricing.GPU10GB
	case "gpu.h100.2g.20gb":
		return pricing.GPU20GB
	case "gpu.h100.4g.40gb":
		return pricing.GPU40GB
	case "gpu.h100.7g.80gb":
		return pricing.GPU80GB
	default:
		return 0
	}
}
