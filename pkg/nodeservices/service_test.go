// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nodeservices

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newMock(t *testing.T) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewService(db), mock, func() { db.Close() }
}

func TestMount_InsertsPendingRow(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	nodeID := uuid.New()
	id := uuid.New()
	now := time.Now()
	config := json.RawMessage(`{"model_route":"teepin/qwen3-omni-7b","engine":"vllm-omni"}`)

	mock.ExpectQuery(`INSERT INTO compute\.node_services`).
		WithArgs(nodeID, "inference_model", []byte(config), "op").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "node_id", "kind", "config", "desired_state", "observed_state",
			"observed_error", "observed_at", "created_by", "created_at", "updated_at",
		}).AddRow(
			id, nodeID, "inference_model", []byte(config), "mounted", "pending",
			nil, nil, "op", now, now,
		))

	ns, err := s.Mount(context.Background(), nodeID, KindInferenceModel, config, "op")
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if ns.DesiredState != DesiredMounted {
		t.Errorf("DesiredState = %q, want mounted", ns.DesiredState)
	}
	if ns.ObservedState != ObservedPending {
		t.Errorf("ObservedState = %q, want pending — a fresh mount has no report yet", ns.ObservedState)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestMount_RejectsInvalidKind(t *testing.T) {
	s := NewService(nil)
	if _, err := s.Mount(context.Background(), uuid.New(), Kind("bogus"), nil, "op"); err == nil {
		t.Error("invalid kind accepted")
	}
	if _, err := s.Mount(context.Background(), uuid.New(), KindInferenceModel, nil, ""); err == nil {
		t.Error("blank created_by accepted")
	}
}

func TestUnmount_FlipsDesiredStateOnly(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	id := uuid.New()
	mock.ExpectExec(`UPDATE compute\.node_services\s+SET desired_state = 'unmounted'`).
		WithArgs(id).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.Unmount(context.Background(), id); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestUnmount_NotFound(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectExec(`UPDATE compute\.node_services`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := s.Unmount(context.Background(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestReportObserved_ClearsErrorWhenStateIsNotError proves a stale error
// message can never linger once the state that caused it has moved on —
// passing a non-nil error alongside ObservedMounted must not persist it.
func TestReportObserved_ClearsErrorWhenStateIsNotError(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	id := uuid.New()
	mock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("mounted", nil, id).
		WillReturnResult(sqlmock.NewResult(0, 1))

	staleErr := "previous failure"
	if err := s.ReportObserved(context.Background(), id, ObservedMounted, &staleErr); err != nil {
		t.Fatalf("ReportObserved: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestReportObserved_KeepsErrorMessageWhenStateIsError(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	id := uuid.New()
	msg := "mlx server exited with code 1"
	mock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("error", &msg, id).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.ReportObserved(context.Background(), id, ObservedError, &msg); err != nil {
		t.Fatalf("ReportObserved: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestReportObserved_RejectsInvalidState(t *testing.T) {
	s := NewService(nil)
	if err := s.ReportObserved(context.Background(), uuid.New(), ObservedState("bogus"), nil); err == nil {
		t.Error("invalid observed state accepted")
	}
}

func TestListByKind_ReturnsMatchingRows(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	nodeID := uuid.New()
	id := uuid.New()
	now := time.Now()
	config := json.RawMessage(`{}`)

	mock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WithArgs("inference_model").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "node_id", "kind", "config", "desired_state", "observed_state",
			"observed_error", "observed_at", "created_by", "created_at", "updated_at",
		}).AddRow(
			id, nodeID, "inference_model", []byte(config), "mounted", "mounted",
			nil, now, "op", now, now,
		))

	list, err := s.ListByKind(context.Background(), KindInferenceModel)
	if err != nil {
		t.Fatalf("ListByKind: %v", err)
	}
	if len(list) != 1 || list[0].Kind != KindInferenceModel {
		t.Errorf("got %+v, want one inference_model row", list)
	}
}

// TestListByKind_NoMatchesReturnsEmptySliceNotNil is the regression test
// for a real bug found live 2026-09-18 (modelcatalog.ListModels had the
// identical bug, fixed alongside this one): a nil slice marshals to JSON
// `null` rather than `[]`, crashing any caller expecting an array — the
// exact state a node with nothing mounted on it is in by default.
func TestListByKind_NoMatchesReturnsEmptySliceNotNil(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WithArgs("inference_model").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "node_id", "kind", "config", "desired_state", "observed_state",
			"observed_error", "observed_at", "created_by", "created_at", "updated_at",
		}))

	list, err := s.ListByKind(context.Background(), KindInferenceModel)
	if err != nil {
		t.Fatalf("ListByKind: %v", err)
	}
	if list == nil {
		t.Fatal("ListByKind returned nil for no matches — must be []NodeService{} so it JSON-marshals to [], not null")
	}
	if len(list) != 0 {
		t.Errorf("got %d rows, want 0", len(list))
	}
}

func TestGet_NotFound(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WillReturnError(sql.ErrNoRows)

	if _, err := s.Get(context.Background(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}
