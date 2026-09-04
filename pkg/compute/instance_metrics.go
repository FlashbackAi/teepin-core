// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package compute

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// InstanceMetricSample is one compute.instance_metrics row — a single
// utilization reading at a point in time. What ListInstanceMetrics
// returns; the raw series a customer's console graph plots. All fields
// are point-in-time CURRENT USE, not the static allocation already on
// InstanceRecord (CPUUnits/MemoryGB) — a zero reading is indistinguishable
// from "genuinely idle"; a caller needing to tell the two apart should key
// off RecordedAt instead.
type InstanceMetricSample struct {
	RecordedAt     time.Time `json:"recorded_at"`
	CPUUsedPercent float64   `json:"cpu_used_percent"`
	MemoryUsedGB   float64   `json:"memory_used_gb"`
	NetworkRxMbps  float64   `json:"network_rx_mbps"`
	NetworkTxMbps  float64   `json:"network_tx_mbps"`
	// StorageUsedGB is a SNAPSHOT of ephemeral storage usage, NOT a
	// throughput rate — see cluster.InstanceMetric's own doc comment on
	// why this is a different KIND of measurement from the node
	// pipeline's storage_read_mbps/storage_write_mbps.
	StorageUsedGB float64 `json:"storage_used_gb"`
}

// InstanceMetricWrite is one instance's reading to persist — the
// pkg/compute-local shape RecordInstanceMetrics takes, kept separate from
// cluster.InstanceMetricSeen so this package does not need to import
// pkg/cluster just for a DTO (cmd/api-server's adapter translates between
// the two, same pattern as nodeReporterAdapter for pkg/nodes).
type InstanceMetricWrite struct {
	InstanceID     string
	CPUUsedPercent float64
	MemoryUsedGB   float64
	NetworkRxMbps  float64
	NetworkTxMbps  float64
	StorageUsedGB  float64
}

// DefaultMetricsWindow/MaxMetricsWindow mirror pkg/nodes' identical
// consts and identical reasoning: an omitted or unparseable `since` on a
// read should degrade to a small, sane default query (1 hour), not
// silently cost the largest possible one (7 days) — see that package's
// own doc comments for the full argument.
const (
	DefaultInstanceMetricsWindow = time.Hour
	MaxInstanceMetricsWindow     = 7 * 24 * time.Hour
)

// RecordInstanceMetrics persists one sweep's readings in a single
// multi-row INSERT — unlike pkg/nodes.UpsertSeen's one-row-at-a-time
// writes (a node reports itself once per sweep), one InstanceMetricsReport
// carries EVERY locally-running instance's reading at once, and a home
// fleet can run many of them, so batching avoids one round trip per
// instance per 30s sweep.
//
// Best-effort by design: the caller (cmd/api-server's write-through
// adapter, mirroring nodeReporterAdapter) already treats this as
// non-fatal observability, same as pkg/nodes.UpsertSeen's own metrics
// insert — a failure here must never be allowed to look like a billing-
// relevant write failing.
func (s *Store) RecordInstanceMetrics(ctx context.Context, samples []InstanceMetricWrite) error {
	if len(samples) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString(`INSERT INTO compute.instance_metrics
		(instance_id, cpu_used_percent, memory_used_gb, network_rx_mbps, network_tx_mbps, storage_used_gb)
		VALUES `)
	args := make([]any, 0, len(samples)*6)
	for i, sample := range samples {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * 6
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d)", base+1, base+2, base+3, base+4, base+5, base+6)
		args = append(args, sample.InstanceID, sample.CPUUsedPercent, sample.MemoryUsedGB,
			sample.NetworkRxMbps, sample.NetworkTxMbps, sample.StorageUsedGB)
	}

	if _, err := s.db.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("insert instance metrics: %w", err)
	}
	return nil
}

// ListInstanceMetrics returns one instance's utilization history over the
// last `since` duration, oldest first — same shape and same two-threshold
// clamping as pkg/nodes.Service.ListMetrics (see DefaultInstanceMetricsWindow's
// own doc comment for why omitted/invalid differs from explicitly-too-large).
//
// Does NOT verify the instance belongs to any particular tenant — the
// caller (pkg/api's GetInstanceMetrics handler) already proves that via
// cluster.GetInstanceStatus's own scope check before this is ever called,
// the same trust boundary GetInstanceLogs already uses for StreamLogs.
func (s *Store) ListInstanceMetrics(ctx context.Context, instanceID string, since time.Duration) ([]InstanceMetricSample, error) {
	switch {
	case since <= 0:
		since = DefaultInstanceMetricsWindow
	case since > MaxInstanceMetricsWindow:
		since = MaxInstanceMetricsWindow
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT recorded_at, cpu_used_percent, memory_used_gb, network_rx_mbps, network_tx_mbps, storage_used_gb
		FROM compute.instance_metrics
		WHERE instance_id = $1 AND recorded_at > $2
		ORDER BY recorded_at ASC
	`, instanceID, time.Now().Add(-since))
	if err != nil {
		return nil, fmt.Errorf("failed to list instance metrics: %w", err)
	}
	defer rows.Close()

	var samples []InstanceMetricSample
	for rows.Next() {
		var m InstanceMetricSample
		if err := rows.Scan(&m.RecordedAt, &m.CPUUsedPercent, &m.MemoryUsedGB, &m.NetworkRxMbps, &m.NetworkTxMbps, &m.StorageUsedGB); err != nil {
			return nil, fmt.Errorf("failed to scan instance metric: %w", err)
		}
		samples = append(samples, m)
	}
	return samples, rows.Err()
}

// PurgeOldInstanceMetrics deletes instance_metrics rows older than
// retentionWindow — called from RetentionSweeper alongside the existing
// terminated-instance purge, same daily cadence, same "log and retry
// next tick" non-fatal posture.
func (s *Store) PurgeOldInstanceMetrics(ctx context.Context, retentionWindow time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM compute.instance_metrics WHERE recorded_at < $1
	`, time.Now().Add(-retentionWindow))
	if err != nil {
		return 0, fmt.Errorf("purge old instance metrics: %w", err)
	}
	return res.RowsAffected()
}
