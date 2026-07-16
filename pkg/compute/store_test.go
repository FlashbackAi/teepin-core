// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package compute

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newMockStore(t *testing.T) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db), mock
}

func TestCreate_PersistsGPUInstance(t *testing.T) {
	store, mock := newMockStore(t)
	projectID, userID := uuid.New(), uuid.New()

	mock.ExpectQuery(`INSERT INTO compute\.instances`).
		WithArgs("inst-abc12345", projectID, userID, "my-app", "nginx:latest",
			"gpu.h100.2g.20gb", StatusPending, int64(20), 8, 32,
			nil, "my-app-x1y2z", "default").
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at"}).
			AddRow(time.Now(), time.Now()))

	rec := &InstanceRecord{
		ID:           "inst-abc12345",
		ProjectID:    projectID,
		UserID:       userID,
		Name:         "my-app",
		Image:        "nginx:latest",
		InstanceType: "gpu.h100.2g.20gb",
		Status:       StatusPending,
		GPUVRAMGB:    20,
		CPUUnits:     8,
		MemoryGB:     32,
		K8sPodName:   "my-app-x1y2z",
		K8sNamespace: "default",
	}

	if err := store.Create(context.Background(), rec); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if rec.CreatedAt.IsZero() {
		t.Error("CreatedAt not populated from RETURNING")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestCreate_CPUInstanceStoresNullVRAM(t *testing.T) {
	store, mock := newMockStore(t)
	projectID, userID := uuid.New(), uuid.New()

	// gpu_vram_gb must be NULL (not 0) for CPU-only instances.
	mock.ExpectQuery(`INSERT INTO compute\.instances`).
		WithArgs("inst-cpu00001", projectID, userID, "web", "nginx:latest",
			"", StatusPending, nil, 2, 4, nil, "web-abcde", "default").
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at"}).
			AddRow(time.Now(), time.Now()))

	rec := &InstanceRecord{
		ID: "inst-cpu00001", ProjectID: projectID, UserID: userID,
		Name: "web", Image: "nginx:latest", Status: StatusPending,
		CPUUnits: 2, MemoryGB: 4, K8sPodName: "web-abcde", K8sNamespace: "default",
	}

	if err := store.Create(context.Background(), rec); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestUpdateStatus(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs(StatusRunning, "inst-abc12345").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.UpdateStatus(context.Background(), "inst-abc12345", StatusRunning); err != nil {
		t.Fatalf("UpdateStatus failed: %v", err)
	}

	// Unknown or already-terminated instance is an error.
	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs(StatusRunning, "inst-missing0").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := store.UpdateStatus(context.Background(), "inst-missing0", StatusRunning); err == nil {
		t.Error("expected error for missing instance")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestMarkTerminated_Idempotent(t *testing.T) {
	store, mock := newMockStore(t)

	// Already terminated → 0 rows affected → still no error.
	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs(StatusTerminated, "inst-abc12345").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := store.MarkTerminated(context.Background(), "inst-abc12345"); err != nil {
		t.Fatalf("MarkTerminated must be idempotent, got: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func instanceRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "project_id", "user_id", "name", "image",
		"instance_type_id", "status", "gpu_vram_gb",
		"cpu_units", "memory_gb", "endpoint",
		"k8s_pod_name", "k8s_namespace",
		"created_at", "updated_at", "started_at", "terminated_at",
	})
}

func TestListActive(t *testing.T) {
	store, mock := newMockStore(t)
	projectID, userID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .+ FROM compute\.instances WHERE terminated_at IS NULL`).
		WillReturnRows(instanceRows().AddRow(
			"inst-abc12345", projectID, userID, "my-app", "nginx:latest",
			"gpu.h100.custom-25gb", StatusRunning, 25,
			8, 32, "https://inst-abc12345.teepin.io",
			"my-app-x1y2z", "default",
			time.Now(), time.Now(), time.Now(), nil,
		))

	instances, err := store.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive failed: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}

	inst := instances[0]
	if inst.GPUVRAMGB != 25 || inst.InstanceType != "gpu.h100.custom-25gb" {
		t.Errorf("got VRAM %d type %q, want 25 gpu.h100.custom-25gb", inst.GPUVRAMGB, inst.InstanceType)
	}
	if inst.TerminatedAt != nil {
		t.Error("TerminatedAt should be nil for active instance")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
