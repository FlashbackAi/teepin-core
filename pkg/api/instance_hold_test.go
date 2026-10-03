// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
)

var holdProject = uuid.MustParse("00000000-0000-0000-0000-0000000000bb")

func recordRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_id", "project_id", "user_id", "name", "image",
		"instance_type_id", "status", "gpu_vram_gb",
		"cpu_units", "memory_gb", "endpoint",
		"k8s_pod_name", "k8s_namespace",
		"provider_id", "node_name", "dns_name", "public_ip", "tls_enabled", "tls_ready", "container_port",
		"storage_gb",
		"created_at", "updated_at", "started_at", "terminated_at", "build_session_id",
	})
}

// addRecord queues one instance row for a store read.
func addRecord(rows *sqlmock.Rows, id string, project uuid.UUID, status string, gpuVRAM int) *sqlmock.Rows {
	return rows.AddRow(id, testAccountID, project, uuid.New(), "app", "myorg/app:v1",
		"cpu.home", status, gpuVRAM, 2, 4, "",
		id+"-pod", "default", "provider-7", "node-a", "", "", false, false, 8080,
		50,
		time.Now(), time.Now(), nil, nil, nil)
}

func expectGet(mock sqlmock.Sqlmock, id string, project uuid.UUID, status string, gpuVRAM int) {
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances\s+WHERE id = \$1`).
		WillReturnRows(addRecord(recordRows(), id, project, status, gpuVRAM))
}

type holdHarness struct {
	server *Server
	mock   sqlmock.Sqlmock
	fc     *fakeCluster
	vault  *compute.SpecVault
}

func newHoldHarness(t *testing.T, balance float64) *holdHarness {
	t.Helper()
	mock, _, cStore := newMockBuildDB(t)
	vault, err := compute.NewSpecVault("test-platform-key-test-platform-key")
	if err != nil {
		t.Fatal(err)
	}
	fc := newFakeCluster()
	s := NewServer(fc, nil, cStore, nil, allowGate{}).WithSpecVault(vault).WithCreditGuard(&fakeCredit{balance: balance})
	return &holdHarness{server: s, mock: mock, fc: fc, vault: vault}
}

func (h *holdHarness) call(handler gin.HandlerFunc, method, path, id string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, nil)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Set(string(auth.ProjectIDKey), holdProject)
	c.Set(string(auth.AccountIDKey), testAccountID)
	handler(c)
	return w
}

func sealedFor(t *testing.T, h *holdHarness, id string) []byte {
	t.Helper()
	sealed, err := h.vault.Seal(cluster.InstanceSpec{
		InstanceID: id, Image: "myorg/app:v1", CPUUnits: 2, MemoryGB: 4, StorageGB: 50,
		Env: map[string]string{"SECRET": "s3cr3t"}, ProviderID: "provider-7", NodeName: "node-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestGetInstance_ShowsAStoppedInstanceThatHasNoPod(t *testing.T) {
	h := newHoldHarness(t, 0)
	expectGet(h.mock, "inst-held0001", holdProject, compute.StatusStopped, 0)

	w := h.call(h.server.GetInstance, http.MethodGet, "/", "inst-held0001")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["status"] != "stopped" || got["storage_gb"] != float64(50) {
		t.Errorf("body = %v, want a stopped instance with its disk", got)
	}
}

// Another project's stopped instance is indistinguishable from a missing one.
func TestGetInstance_StoppedInstanceOfAnotherProjectIs404(t *testing.T) {
	h := newHoldHarness(t, 0)
	expectGet(h.mock, "inst-held0002", uuid.New(), compute.StatusStopped, 0)

	if w := h.call(h.server.GetInstance, http.MethodGet, "/", "inst-held0002"); w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", w.Code)
	}
}

func TestListInstances_IncludesStoppedInstances(t *testing.T) {
	h := newHoldHarness(t, 0)
	h.fc.add("inst-live0001", holdProject.String(), compute.StatusRunning)
	rows := recordRows()
	addRecord(rows, "inst-live0001", holdProject, compute.StatusRunning, 0)
	addRecord(rows, "inst-held0003", holdProject, compute.StatusStopped, 0)
	h.mock.ExpectQuery(`FROM compute\.instances\s+WHERE account_id = \$1 AND project_id = \$2`).WillReturnRows(rows)

	w := h.call(h.server.ListInstances, http.MethodGet, "/", "")
	var got struct {
		Instances []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"instances"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	status := map[string]string{}
	for _, i := range got.Instances {
		status[i.ID] = i.Status
	}
	if status["inst-live0001"] != "running" || status["inst-held0003"] != "stopped" || len(status) != 2 {
		t.Errorf("listed = %v, want the running instance and the stopped one", status)
	}
}

// Deleting a stopped instance goes to the cluster with an unscoped delete (a
// stopped instance has no live status the scoped path could find) and ends
// its billing record.
func TestDeleteInstance_StoppedInstanceIsDeletedWithItsDisk(t *testing.T) {
	h := newHoldHarness(t, 0)
	expectGet(h.mock, "inst-held0004", holdProject, compute.StatusStopped, 0)
	h.mock.ExpectExec(`UPDATE compute\.instances\s+SET status = \$1, terminated_at = NOW\(\)`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := h.call(h.server.DeleteInstance, http.MethodDelete, "/", "inst-held0004")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if err := h.mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStartInstance_RelaunchesTheSameInstanceFromItsSealedSpec(t *testing.T) {
	h := newHoldHarness(t, 50)
	const id = "inst-held0005"
	expectGet(h.mock, id, holdProject, compute.StatusStopped, 0)
	h.mock.ExpectQuery(`SELECT launch_spec FROM compute\.instances`).
		WillReturnRows(sqlmock.NewRows([]string{"launch_spec"}).AddRow(sealedFor(t, h, id)))
	h.mock.ExpectExec(`resumed_at = NOW\(\)`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectGet(h.mock, id, holdProject, compute.StatusPending, 0)

	w := h.call(h.server.StartInstance, http.MethodPost, "/", id)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	spec := h.fc.lastSpec
	if spec.InstanceID != id || spec.Env["SECRET"] != "s3cr3t" || spec.StorageGB != 50 || spec.ProviderID != "provider-7" {
		t.Errorf("relaunched with %+v; want the stored spec (same id, env, disk, node)", spec)
	}
	if err := h.mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStartInstance_NeedsCreditAgain(t *testing.T) {
	h := newHoldHarness(t, 0)
	expectGet(h.mock, "inst-held0006", holdProject, compute.StatusStopped, 0)

	w := h.call(h.server.StartInstance, http.MethodPost, "/", "inst-held0006")
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status %d, want 402", w.Code)
	}
	if h.fc.lastSpec.InstanceID != "" {
		t.Error("an instance was launched for an account with no credit")
	}
}

func TestStartInstance_WithoutStoredSpecIsRefused(t *testing.T) {
	h := newHoldHarness(t, 50)
	expectGet(h.mock, "inst-held0007", holdProject, compute.StatusStopped, 0)
	h.mock.ExpectQuery(`SELECT launch_spec FROM compute\.instances`).
		WillReturnRows(sqlmock.NewRows([]string{"launch_spec"}).AddRow(nil))

	w := h.call(h.server.StartInstance, http.MethodPost, "/", "inst-held0007")
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", w.Code)
	}
	if h.fc.lastSpec.InstanceID != "" {
		t.Error("something was launched with no stored spec")
	}
}

// A spec sealed for a different instance must not launch this one.
func TestStartInstance_RejectsAnotherInstancesSpec(t *testing.T) {
	h := newHoldHarness(t, 50)
	expectGet(h.mock, "inst-held0008", holdProject, compute.StatusStopped, 0)
	h.mock.ExpectQuery(`SELECT launch_spec FROM compute\.instances`).
		WillReturnRows(sqlmock.NewRows([]string{"launch_spec"}).AddRow(sealedFor(t, h, "inst-someone-else")))

	if w := h.call(h.server.StartInstance, http.MethodPost, "/", "inst-held0008"); w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", w.Code)
	}
	if h.fc.lastSpec.InstanceID != "" {
		t.Error("another instance's spec was launched")
	}
}

func TestStartInstance_OnlyStoppedInstancesCanBeStarted(t *testing.T) {
	h := newHoldHarness(t, 50)
	expectGet(h.mock, "inst-live0002", holdProject, compute.StatusRunning, 0)

	if w := h.call(h.server.StartInstance, http.MethodPost, "/", "inst-live0002"); w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 for a running instance", w.Code)
	}
}

// If the pod comes up but the record cannot be updated, the caller is told it
// failed rather than left with an instance billing does not see.
func TestStartInstance_RecordNotUpdatedIsAnError(t *testing.T) {
	h := newHoldHarness(t, 50)
	const id = "inst-held0009"
	expectGet(h.mock, id, holdProject, compute.StatusStopped, 0)
	h.mock.ExpectQuery(`SELECT launch_spec FROM compute\.instances`).
		WillReturnRows(sqlmock.NewRows([]string{"launch_spec"}).AddRow(sealedFor(t, h, id)))
	h.mock.ExpectExec(`resumed_at = NOW\(\)`).WillReturnResult(sqlmock.NewResult(0, 0))

	if w := h.call(h.server.StartInstance, http.MethodPost, "/", id); w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", w.Code)
	}
}

// Only instances with a disk keep a launch spec (it holds secrets, and only
// disk-backed instances can be held).
func TestPersistLaunchSpec_OnlyForInstancesWithADisk(t *testing.T) {
	h := newHoldHarness(t, 50)
	ctx := t.Context()

	h.server.persistLaunchSpec(ctx, "inst-nodisk", cluster.InstanceSpec{InstanceID: "inst-nodisk"})
	// No query was expected for the diskless one.
	h.mock.ExpectExec(`UPDATE compute\.instances SET launch_spec = \$2`).
		WithArgs("inst-disk", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.server.persistLaunchSpec(ctx, "inst-disk", cluster.InstanceSpec{InstanceID: "inst-disk", StorageGB: 10, Env: map[string]string{"K": "V"}})
	if err := h.mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
