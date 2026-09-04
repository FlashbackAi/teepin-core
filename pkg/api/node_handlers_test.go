// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/nodes"
)

// nearCutoffArg matches a time.Time sqlmock argument within 5s of want —
// enough slack for the handful of milliseconds between computing the
// expectation and the query actually running, without being so loose it
// would also accept MaxMetricsWindow's much-further-back cutoff.
type nearCutoffArg struct{ want time.Time }

func (a nearCutoffArg) Match(v driver.Value) bool {
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

// newNodeHandlerMock builds a NodeHandler backed by a real *nodes.Service on
// a sqlmock DB — GetNodeMetrics depends on the concrete Service (not an
// interface), so exercising it end-to-end requires a real Service the same
// way pkg/nodes/service_test.go does, not a hand-rolled fake.
func newNodeHandlerMock(t *testing.T) (*NodeHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	svc := nodes.NewService(db)
	return NewNodeHandler(svc, nil), mock, func() { db.Close() }
}

// nodeHandlerRequest drives a handler through a real gin.Context, mirroring
// this package's existing kumbhaRequest helper.
func nodeHandlerRequest(handler gin.HandlerFunc, method, path string, params gin.Params) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, bytes.NewReader(nil))
	c.Params = params
	handler(c)
	return w
}

// A malformed :id never reaches the database.
func TestGetNodeMetrics_InvalidNodeID(t *testing.T) {
	h, _, done := newNodeHandlerMock(t)
	defer done()

	w := nodeHandlerRequest(h.GetNodeMetrics, "GET", "/v1/admin/nodes/not-a-uuid/metrics",
		gin.Params{{Key: "id", Value: "not-a-uuid"}})

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// A well-formed but unknown node id is a 404, not an empty 200 — proving the
// NodeExists check added during the telemetry-pipeline audit is actually wired
// into the handler, not just present on the Service.
func TestGetNodeMetrics_NodeNotFound(t *testing.T) {
	h, mock, done := newNodeHandlerMock(t)
	defer done()

	id := uuid.New()
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM compute\.nodes WHERE id = \$1\)`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	w := nodeHandlerRequest(h.GetNodeMetrics, "GET", "/v1/admin/nodes/"+id.String()+"/metrics",
		gin.Params{{Key: "id", Value: id.String()}})

	if w.Code != 404 {
		t.Fatalf("status = %d, want 404, body: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The success path: an existing node returns its samples, oldest first.
func TestGetNodeMetrics_Success(t *testing.T) {
	h, mock, done := newNodeHandlerMock(t)
	defer done()

	id := uuid.New()
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM compute\.nodes WHERE id = \$1\)`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	recordedAt := time.Now().Add(-10 * time.Minute)
	mock.ExpectQuery(`SELECT recorded_at, cpu_used_percent, memory_used_gb, gpu_vram_used_gb`).
		WithArgs(id, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"recorded_at", "cpu_used_percent", "memory_used_gb", "gpu_vram_used_gb",
			"network_rx_mbps", "network_tx_mbps", "storage_read_mbps", "storage_write_mbps"}).
			AddRow(recordedAt, 42.5, 12.75, 20, 5.5, 1.25, 3.0, 0.75))

	w := nodeHandlerRequest(h.GetNodeMetrics, "GET", "/v1/admin/nodes/"+id.String()+"/metrics?since=1h",
		gin.Params{{Key: "id", Value: id.String()}})

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		NodeID  uuid.UUID           `json:"node_id"`
		Samples []nodes.MetricSample `json:"samples"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.NodeID != id {
		t.Errorf("node_id = %v, want %v", resp.NodeID, id)
	}
	if len(resp.Samples) != 1 {
		t.Fatalf("got %d samples, want 1", len(resp.Samples))
	}
	if resp.Samples[0].CPUUsedPercent != 42.5 || resp.Samples[0].MemoryUsedGB != 12.75 || resp.Samples[0].GPUUsedVRAMGB != 20 {
		t.Errorf("sample = %+v, want {42.5 12.75 20 ...}", resp.Samples[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An omitted ?since must use ListMetrics' own DefaultMetricsWindow, not zero
// (which would be misread as "beginning of time") and not MaxMetricsWindow
// (a 7-day query for a bare, unparameterized request) — this is gap #4 from
// the telemetry-pipeline audit, now proven from the HTTP layer down.
func TestGetNodeMetrics_OmittedSinceUsesDefaultWindow(t *testing.T) {
	h, mock, done := newNodeHandlerMock(t)
	defer done()

	id := uuid.New()
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM compute\.nodes WHERE id = \$1\)`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	wantCutoff := time.Now().Add(-nodes.DefaultMetricsWindow)
	mock.ExpectQuery(`SELECT recorded_at, cpu_used_percent, memory_used_gb, gpu_vram_used_gb`).
		WithArgs(id, nearCutoffArg{want: wantCutoff}).
		WillReturnRows(sqlmock.NewRows([]string{"recorded_at", "cpu_used_percent", "memory_used_gb", "gpu_vram_used_gb"}))

	w := nodeHandlerRequest(h.GetNodeMetrics, "GET", "/v1/admin/nodes/"+id.String()+"/metrics",
		gin.Params{{Key: "id", Value: id.String()}})

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (default window not applied?): %v", err)
	}
}
