// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package compute

import (
	"context"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestRecordInstanceMetrics_EmptyIsNoOp(t *testing.T) {
	store, mock := newMockStore(t)

	if err := store.RecordInstanceMetrics(context.Background(), nil); err != nil {
		t.Fatalf("RecordInstanceMetrics: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v (an empty batch must not touch the database at all)", err)
	}
}

// A batch of readings writes as ONE multi-row INSERT, not one round trip
// per instance — the whole point of taking a slice rather than being
// called once per instance the way pkg/nodes.UpsertSeen is.
func TestRecordInstanceMetrics_BatchesIntoOneInsert(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectExec(`INSERT INTO compute\.instance_metrics`).
		WithArgs(
			"inst-abc123", 42.5, 1.75, 3.5, 1.25, 8.0,
			"inst-def456", 10.0, 0.5, 0.0, 0.0, 0.0,
		).
		WillReturnResult(sqlmock.NewResult(0, 2))

	err := store.RecordInstanceMetrics(context.Background(), []InstanceMetricWrite{
		{InstanceID: "inst-abc123", CPUUsedPercent: 42.5, MemoryUsedGB: 1.75, NetworkRxMbps: 3.5, NetworkTxMbps: 1.25, StorageUsedGB: 8.0},
		{InstanceID: "inst-def456", CPUUsedPercent: 10.0, MemoryUsedGB: 0.5},
	})
	if err != nil {
		t.Fatalf("RecordInstanceMetrics: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestListInstanceMetrics_ReturnsOldestFirst(t *testing.T) {
	store, mock := newMockStore(t)

	t1 := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 4, 10, 0, 30, 0, time.UTC)
	mock.ExpectQuery(`SELECT recorded_at, cpu_used_percent, memory_used_gb, network_rx_mbps, network_tx_mbps, storage_used_gb\s+FROM compute\.instance_metrics`).
		WithArgs("inst-abc123", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"recorded_at", "cpu_used_percent", "memory_used_gb", "network_rx_mbps", "network_tx_mbps", "storage_used_gb"}).
			AddRow(t1, 12.5, 1.0, 1.0, 0.5, 2.0).
			AddRow(t2, 15.0, 1.2, 1.5, 0.75, 2.5))

	samples, err := store.ListInstanceMetrics(context.Background(), "inst-abc123", time.Hour)
	if err != nil {
		t.Fatalf("ListInstanceMetrics: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2", len(samples))
	}
	if !samples[0].RecordedAt.Equal(t1) || samples[0].CPUUsedPercent != 12.5 {
		t.Errorf("first sample = %+v, want t1/12.5", samples[0])
	}
	if !samples[1].RecordedAt.Equal(t2) || samples[1].StorageUsedGB != 2.5 {
		t.Errorf("second sample = %+v, want t2/2.5", samples[1])
	}
}

// Same two-threshold clamping as pkg/nodes.ListMetrics: a non-positive
// since uses the smaller DEFAULT window, an explicit but too-large since
// clamps down to the MAX window — different thresholds, not the same one
// applied twice, per DefaultInstanceMetricsWindow's own doc comment.
func TestListInstanceMetrics_SinceClamping(t *testing.T) {
	cases := []struct {
		name       string
		since      time.Duration
		wantCutoff time.Duration
	}{
		{"way past max clamps down to it", 365 * 24 * time.Hour, MaxInstanceMetricsWindow},
		{"zero uses the smaller default, not the max", 0, DefaultInstanceMetricsWindow},
		{"negative uses the smaller default, not the max", -time.Hour, DefaultInstanceMetricsWindow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, mock := newMockStore(t)

			mock.ExpectQuery(`SELECT recorded_at, cpu_used_percent, memory_used_gb, network_rx_mbps, network_tx_mbps, storage_used_gb\s+FROM compute\.instance_metrics`).
				WithArgs("inst-abc123", nearTimeArg{want: time.Now().Add(-tc.wantCutoff)}).
				WillReturnRows(sqlmock.NewRows([]string{"recorded_at", "cpu_used_percent", "memory_used_gb", "network_rx_mbps", "network_tx_mbps", "storage_used_gb"}))

			if _, err := store.ListInstanceMetrics(context.Background(), "inst-abc123", tc.since); err != nil {
				t.Fatalf("ListInstanceMetrics: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet: %v", err)
			}
		})
	}
}

func TestPurgeOldInstanceMetrics(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectExec(`DELETE FROM compute\.instance_metrics WHERE recorded_at < \$1`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 3))

	n, err := store.PurgeOldInstanceMetrics(context.Background(), 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeOldInstanceMetrics: %v", err)
	}
	if n != 3 {
		t.Errorf("purged %d, want 3", n)
	}
}

// nearTimeArg implements sqlmock.Argument to assert a time.Time argument
// is within a tolerance of an expected cutoff — mirrors
// pkg/nodes/service_test.go's identically-purposed nearCutoffArg.
type nearTimeArg struct{ want time.Time }

func (a nearTimeArg) Match(v driver.Value) bool {
	got, ok := v.(time.Time)
	if !ok {
		return false
	}
	d := got.Sub(a.want)
	if d < 0 {
		d = -d
	}
	return d < 5*time.Second
}
