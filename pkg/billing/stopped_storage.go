// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
)

// stoppedDisk is a stopped instance whose disk is still kept.
type stoppedDisk struct {
	ID           string
	AccountID    uuid.UUID
	ProjectID    uuid.UUID
	InstanceType string
	StorageGB    int
	// From is where storage billing for this disk resumes: after the last
	// billed interval, after the moment it was stopped, and after the most
	// recent storage hold ended. Time spent on hold is never billed.
	From time.Time
}

// collectStoppedStorage bills the disks of stopped instances at the storage
// rate. A stopped instance costs nothing for compute, but its disk is still
// the customer's and still occupies a node, so once the account has credit
// again (no active storage hold) the disk is billed like any other storage.
// While a hold is active it is free, by design.
//
// Runs alongside collectUsage; accountID limits it to one account.
func (c *UsageCollector) collectStoppedStorage(ctx context.Context, accountID *uuid.UUID) {
	rate := c.billingService.StorageGBMonthRate(ctx)
	if rate <= 0 {
		return
	}
	disks, err := c.stoppedDisks(ctx, accountID)
	if err != nil {
		log.Printf("WARN: stopped-disk billing: %v", err)
		return
	}
	now := time.Now()
	for _, d := range disks {
		dur := now.Sub(d.From)
		if dur < time.Minute {
			continue
		}
		hours := dur.Hours()
		perHour := float64(d.StorageGB) * rate / hoursPerMonth
		record := &UsageRecord{
			AccountID:    d.AccountID,
			ProjectID:    d.ProjectID,
			InstanceID:   d.ID,
			ResourceType: d.InstanceType + " (stopped, disk only)",
			Quantity:     hours,
			Unit:         "hours",
			UnitPrice:    perHour,
			TotalCost:    perHour * hours,
			StartTime:    d.From,
			EndTime:      now,
		}
		if err := c.billingService.RecordUsage(ctx, record); err != nil {
			log.Printf("WARN: stopped-disk billing: record for %s: %v", d.ID, err)
			continue
		}
		if _, err := c.billingService.ConsumeCredit(ctx, record.AccountID, record.ID, record.TotalCost); err != nil {
			log.Printf("WARN: stopped-disk billing: apply credit for usage %s: %v", record.ID, err)
		}
	}
}

func (c *UsageCollector) stoppedDisks(ctx context.Context, accountID *uuid.UUID) ([]stoppedDisk, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT i.id, i.account_id, i.project_id, COALESCE(i.instance_type_id, ''), i.storage_gb,
		       GREATEST(COALESCE(b.last_end, i.created_at), i.stopped_at,
		                COALESCE(h.released, i.stopped_at))
		FROM compute.instances i
		LEFT JOIN LATERAL (
			SELECT MAX(end_time) AS last_end FROM billing.usage_records ur WHERE ur.instance_id = i.id
		) b ON true
		LEFT JOIN LATERAL (
			SELECT MAX(released_at) AS released FROM billing.storage_holds sh WHERE sh.account_id = i.account_id
		) h ON true
		WHERE i.status = 'stopped' AND i.terminated_at IS NULL AND i.storage_gb > 0
		  AND i.stopped_at IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1 FROM billing.storage_holds sh
			WHERE sh.account_id = i.account_id AND sh.released_at IS NULL AND sh.purged_at IS NULL)
		  AND ($1::uuid IS NULL OR i.account_id = $1)`, accountID)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()
	var out []stoppedDisk
	for rows.Next() {
		var d stoppedDisk
		if err := rows.Scan(&d.ID, &d.AccountID, &d.ProjectID, &d.InstanceType, &d.StorageGB, &d.From); err != nil {
			return nil, fmt.Errorf("scan failed: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
