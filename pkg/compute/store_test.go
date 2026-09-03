// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package compute

import (
	"context"
	"database/sql"
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
	accountID, projectID, userID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery(`INSERT INTO compute\.instances`).
		WithArgs("inst-abc12345", accountID, projectID, userID, "my-app", "nginx:latest",
			"gpu.h100.2g.20gb", StatusPending, int64(20), 8, 32,
			nil, "my-app-x1y2z", "default", nil, nil, nil, nil, false, false, nil, 0, nil).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at"}).
			AddRow(time.Now(), time.Now()))

	rec := &InstanceRecord{
		ID:           "inst-abc12345",
		AccountID:    accountID,
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

// TestCreate_NilUserIDStoresNull proves a credential with no attributable
// human user (a Kumbha session token, a project API key — both leave
// InstanceRecord.UserID at uuid.Nil) inserts SQL NULL for user_id rather
// than the literal all-zeros UUID, which would violate
// instances_user_id_fkey (no user row has that id). Regression test for
// the live bug fixed by migration 031 + nullUUID.
func TestCreate_NilUserIDStoresNull(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID := uuid.New(), uuid.New()

	mock.ExpectQuery(`INSERT INTO compute\.instances`).
		WithArgs("inst-noattr001", accountID, projectID, nil, "web", "nginx:latest",
			"", StatusPending, nil, 2, 4, nil, "web-abcde", "default", nil, nil, nil, nil, false, false, nil, 0, nil).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at"}).
			AddRow(time.Now(), time.Now()))

	rec := &InstanceRecord{
		ID: "inst-noattr001", AccountID: accountID, ProjectID: projectID, UserID: uuid.Nil,
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

func TestCreate_CPUInstanceStoresNullVRAM(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID, userID := uuid.New(), uuid.New(), uuid.New()

	// gpu_vram_gb must be NULL (not 0) for CPU-only instances.
	mock.ExpectQuery(`INSERT INTO compute\.instances`).
		WithArgs("inst-cpu00001", accountID, projectID, userID, "web", "nginx:latest",
			"", StatusPending, nil, 2, 4, nil, "web-abcde", "default", nil, nil, nil, nil, false, false, nil, 0, nil).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at"}).
			AddRow(time.Now(), time.Now()))

	rec := &InstanceRecord{
		ID: "inst-cpu00001", AccountID: accountID, ProjectID: projectID, UserID: userID,
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

// TestListByKumbhaSession_ReturnsOnlyThatSessionsInstances proves the new
// tracking query round-trips a real kumbha_session_id — the linkage
// migration 032 added specifically so a Kumbha agent's create_instance
// fallback can no longer create an instance the session has no record of
// (found live 2026-08-30/31: two untracked instances from one build).
func TestListByKumbhaSession_ReturnsOnlyThatSessionsInstances(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID, sessionID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .+ FROM compute\.instances WHERE kumbha_session_id = \$1`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "user_id", "name", "image",
			"instance_type_id", "status", "gpu_vram_gb", "cpu_units", "memory_gb", "endpoint",
			"k8s_pod_name", "k8s_namespace", "provider_id", "node_name", "dns_name", "public_ip",
			"tls_enabled", "tls_ready", "container_port", "storage_gb",
			"created_at", "updated_at", "started_at", "terminated_at", "kumbha_session_id",
		}).AddRow("inst-broken01", accountID, projectID, uuid.Nil, "web", "nginx:1.27-alpine",
			"", StatusRunning, 0, 1, 1, "https://inst-broken01.teepin.com",
			"inst-broken01-pod", "default", "", "", "", "",
			true, true, 80, 0,
			time.Now(), time.Now(), nil, nil, sessionID))

	records, err := store.ListByKumbhaSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListByKumbhaSession failed: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}
	if records[0].KumbhaSessionID != sessionID {
		t.Errorf("KumbhaSessionID = %s, want %s", records[0].KumbhaSessionID, sessionID)
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

func TestUpdateImage(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs("new-image:v2", "inst-abc12345-pod", 8080, StatusPending, "inst-abc12345").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.UpdateImage(context.Background(), "inst-abc12345", "new-image:v2", "inst-abc12345-pod", 8080); err != nil {
		t.Fatalf("UpdateImage failed: %v", err)
	}

	// Unknown instance is an error — so a redeploy targeting a vanished
	// instance fails loudly rather than silently doing nothing. A
	// TERMINATED instance is deliberately NOT this case any more — see
	// TestUpdateImage_RevivesTerminatedInstance below.
	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs("new-image:v2", "inst-missing0-pod", 8080, StatusPending, "inst-missing0").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := store.UpdateImage(context.Background(), "inst-missing0", "new-image:v2", "inst-missing0-pod", 8080); err == nil {
		t.Error("expected error for missing instance")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestUpdateImage_RevivesTerminatedInstance guards the live incident from
// 2026-08-31: inst-5ed29952 was marked terminated by the reconciler
// during an earlier broken redeploy, and every subsequent redeploy
// silently created a real, healthy pod while UpdateImage's OLD guard
// clause (`AND terminated_at IS NULL`) matched zero rows — leaving the
// database permanently out of sync with a genuinely running workload.
// cluster.ProxyTarget.ResolveProvider checks terminated_at directly, so
// the customer-facing edge kept returning "this instance is not
// currently reachable" forever, on an instance that was actually up.
// UpdateImage's only caller (redeployKumbhaInstance) reaches this call
// having just gotten a successful cluster.UpdateInstance back — positive
// proof the pod is alive — so this must succeed and clear terminated_at
// even when the row was previously terminated, not silently no-op.
func TestUpdateImage_RevivesTerminatedInstance(t *testing.T) {
	store, mock := newMockStore(t)

	// The query itself no longer excludes a terminated row: only the
	// bound args (unchanged) are asserted here, matching the rest of this
	// file's convention of not asserting exact SQL text.
	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs("new-image:v3", "inst-revived-pod", 80, StatusPending, "inst-revived").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.UpdateImage(context.Background(), "inst-revived", "new-image:v3", "inst-revived-pod", 80); err != nil {
		t.Fatalf("UpdateImage on a previously-terminated instance failed: %v — a redeploy that just proved the workload is alive must be able to revive its record", err)
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

// instanceRows builds a result set matching selectColumns' shape — used by
// every test across this package that reads instances (also
// reconciler_test.go). Keep the column list here in sync with
// store.go:selectColumns.
func instanceRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_id", "project_id", "user_id", "name", "image",
		"instance_type_id", "status", "gpu_vram_gb",
		"cpu_units", "memory_gb", "endpoint",
		"k8s_pod_name", "k8s_namespace",
		"provider_id", "node_name", "dns_name", "public_ip", "tls_enabled", "tls_ready", "container_port",
		"storage_gb",
		"created_at", "updated_at", "started_at", "terminated_at", "kumbha_session_id",
	})
}

func TestPurgeFullyBilledTerminated(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectExec(`DELETE FROM compute\.instances`).
		WillReturnResult(sqlmock.NewResult(0, 3))

	n, err := store.PurgeFullyBilledTerminated(context.Background(), 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeFullyBilledTerminated failed: %v", err)
	}
	if n != 3 {
		t.Errorf("got %d purged, want 3", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestPurgeFullyBilledTerminated_PropagatesError(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectExec(`DELETE FROM compute\.instances`).
		WillReturnError(sql.ErrConnDone)

	if _, err := store.PurgeFullyBilledTerminated(context.Background(), 30*24*time.Hour); err == nil {
		t.Fatal("expected an error to propagate")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestListActive(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID, userID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .+ FROM compute\.instances WHERE terminated_at IS NULL`).
		WillReturnRows(instanceRows().AddRow(
			"inst-abc12345", accountID, projectID, userID, "my-app", "nginx:latest",
			"gpu.h100.custom-25gb", StatusRunning, 25,
			8, 32, "https://inst-abc12345.teepin.io",
			"my-app-x1y2z", "default",
			"", "", "", "", false, false, 0,
			0,
			time.Now(), time.Now(), time.Now(), nil, nil,
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

// TestListActive_ReadsBackNodeName is the regression test for the same
// class of bug provider_id already had (Stage 3 defect 8): Create
// resolves NodeName to a node_id FK via a sub-select, but selectColumns
// never read it back, so every GET/LIST saw an empty NodeName regardless
// of what was stored. Found live 2026-09-02.
func TestListActive_ReadsBackNodeName(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID, userID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .+ FROM compute\.instances WHERE terminated_at IS NULL`).
		WillReturnRows(instanceRows().AddRow(
			"inst-home0001", accountID, projectID, userID, "my-app", "nginx:latest",
			"cpu.home", StatusRunning, 0,
			2, 4, "https://inst-home0001.teepin.io",
			"my-app-x1y2z", "default",
			"", "srialla", "", "", false, false, 0,
			0,
			time.Now(), time.Now(), time.Now(), nil, nil,
		))

	instances, err := store.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive failed: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}
	if instances[0].NodeName != "srialla" {
		t.Errorf("NodeName = %q, want %q", instances[0].NodeName, "srialla")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
