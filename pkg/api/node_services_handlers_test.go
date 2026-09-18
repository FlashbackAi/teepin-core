// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/nodeservices"
)

func newNodeServicesHandlerMock(t *testing.T) (*NodeServicesHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewNodeServicesHandler(nodeservices.NewService(db)), mock, func() { db.Close() }
}

func nodeServiceRows() []string {
	return []string{
		"id", "node_id", "kind", "config", "desired_state", "observed_state",
		"observed_error", "observed_at", "created_by", "created_at", "updated_at",
	}
}

func TestMount_InvalidNodeID(t *testing.T) {
	h, _, done := newNodeServicesHandlerMock(t)
	defer done()

	body := []byte(`{"node_id":"not-a-uuid","kind":"inference_model","config":{}}`)
	w := jsonRequest(h.Mount, "POST", "/v1/admin/node-services", body, nil)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestMount_Success(t *testing.T) {
	h, mock, done := newNodeServicesHandlerMock(t)
	defer done()

	nodeID := uuid.New()
	id := uuid.New()
	config := `{"model_route":"teepin/x","engine":"vllm"}`

	mock.ExpectQuery(`INSERT INTO compute\.node_services`).
		WillReturnRows(sqlmock.NewRows(nodeServiceRows()).AddRow(
			id, nodeID, "inference_model", []byte(config), "mounted", "pending",
			nil, nil, "admin-api", time.Now(), time.Now(),
		))

	body := []byte(`{"node_id":"` + nodeID.String() + `","kind":"inference_model","config":` + config + `}`)
	w := jsonRequest(h.Mount, "POST", "/v1/admin/node-services", body, nil)

	if w.Code != 201 {
		t.Fatalf("status = %d, want 201, body: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestUnmount_InvalidID(t *testing.T) {
	h, _, done := newNodeServicesHandlerMock(t)
	defer done()

	w := nodeHandlerRequest(h.Unmount, "DELETE", "/v1/admin/node-services/not-a-uuid",
		gin.Params{{Key: "id", Value: "not-a-uuid"}})
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestUnmount_NotFound(t *testing.T) {
	h, mock, done := newNodeServicesHandlerMock(t)
	defer done()

	id := uuid.New()
	mock.ExpectExec(`UPDATE compute\.node_services`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := nodeHandlerRequest(h.Unmount, "DELETE", "/v1/admin/node-services/"+id.String(),
		gin.Params{{Key: "id", Value: id.String()}})
	if w.Code != 404 {
		t.Fatalf("status = %d, want 404, body: %s", w.Code, w.Body.String())
	}
}

func TestList_RequiresFilter(t *testing.T) {
	h, _, done := newNodeServicesHandlerMock(t)
	defer done()

	w := jsonRequest(h.List, "GET", "/v1/admin/node-services", nil, nil)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 when neither node_id nor kind is given", w.Code)
	}
}

func TestList_ByNodeID(t *testing.T) {
	h, mock, done := newNodeServicesHandlerMock(t)
	defer done()

	nodeID := uuid.New()
	mock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WithArgs(nodeID).
		WillReturnRows(sqlmock.NewRows(nodeServiceRows()).AddRow(
			uuid.New(), nodeID, "inference_model", []byte(`{}`), "mounted", "mounted",
			nil, time.Now(), "admin-api", time.Now(), time.Now(),
		))

	w := jsonRequest(h.List, "GET", "/v1/admin/node-services?node_id="+nodeID.String(), nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
}
