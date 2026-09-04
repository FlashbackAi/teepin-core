// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nodes

import (
	"context"
	"fmt"
	"log"
	"time"
)

// metricsRetentionSweepInterval is how often the purge runs — daily is
// plenty, this trims history, it does not need to react quickly to
// anything. Mirrors pkg/compute.RetentionSweeper's own shape.
const metricsRetentionSweepInterval = 24 * time.Hour

// MetricsRetentionWindow is how long a compute.node_metrics row survives
// before MetricsRetentionSweeper deletes it. At the agent's ~15s report
// cadence, unbounded retention would accumulate roughly 5,760 rows per
// node per day — this bounds it. 30 days matches this codebase's
// existing default for operational history (teepin-control-plane's
// log_retention_days) and is comfortably past any real "last 7 days"
// graph a status page or console would actually render.
const MetricsRetentionWindow = 30 * 24 * time.Hour

// PurgeOldMetrics deletes compute.node_metrics rows older than
// retentionWindow. No "fully billed" gate to check first, unlike
// compute.instances' own purge — this table carries no commercial
// weight at all, only observability history, so age alone is sufficient.
func (s *Service) PurgeOldMetrics(ctx context.Context, retentionWindow time.Duration) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM compute.node_metrics WHERE recorded_at < $1
	`, time.Now().Add(-retentionWindow))
	if err != nil {
		return 0, fmt.Errorf("failed to purge old node metrics: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to count purged node metrics: %w", err)
	}
	return n, nil
}

// MetricsRetentionSweeper periodically purges compute.node_metrics rows
// older than MetricsRetentionWindow. Mirrors
// pkg/compute.RetentionSweeper's own Start/ticker shape exactly.
type MetricsRetentionSweeper struct {
	svc *Service
}

// NewMetricsRetentionSweeper builds a sweeper against svc.
func NewMetricsRetentionSweeper(svc *Service) *MetricsRetentionSweeper {
	return &MetricsRetentionSweeper{svc: svc}
}

// Start runs the purge immediately, then on metricsRetentionSweepInterval,
// until ctx is cancelled. A failed sweep is logged and retried on the
// next tick, never fatal — a slowly growing table is not worth crashing
// the control plane over.
func (r *MetricsRetentionSweeper) Start(ctx context.Context) {
	log.Println("Starting node metrics retention sweeper...")

	sweep := func() {
		n, err := r.svc.PurgeOldMetrics(ctx, MetricsRetentionWindow)
		if err != nil {
			log.Printf("WARN: node metrics retention sweep failed: %v", err)
			return
		}
		if n > 0 {
			log.Printf("Node metrics retention sweep purged %d old sample(s)", n)
		}
	}

	sweep()

	ticker := time.NewTicker(metricsRetentionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sweep()
		case <-ctx.Done():
			log.Println("Stopping node metrics retention sweeper...")
			return
		}
	}
}
