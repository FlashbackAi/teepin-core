// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inferencereconciler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/nodes"
	"github.com/FlashbackAi/teepin-core/pkg/nodeservices"
)

// fakeClusterClient implements cluster.Client with controllable
// Create/Delete behaviour — the only two methods this reconciler calls.
// Every other method is a stub; a test that exercised one would be
// testing the wrong package.
type fakeClusterClient struct {
	createFunc func(spec cluster.InstanceSpec) (*cluster.InstanceResult, error)
	deleteFunc func(instanceID string) error

	statusFunc func() (*cluster.InstanceStatus, error)
	statusByID func(id string) (*cluster.InstanceStatus, error)
	addrFunc   func() (string, error)

	lastCreateSpec cluster.InstanceSpec
	lastDeleteID   string
	createCalled   bool
	deleteCalled   bool
}

func (f *fakeClusterClient) CreateInstance(_ context.Context, spec cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	f.createCalled = true
	f.lastCreateSpec = spec
	if f.createFunc != nil {
		return f.createFunc(spec)
	}
	return &cluster.InstanceResult{EndpointURL: "http://10.0.0.42:8000"}, nil
}
func (f *fakeClusterClient) UpdateInstance(context.Context, cluster.Scope, cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeClusterClient) DeleteInstance(_ context.Context, _ cluster.Scope, instanceID string) error {
	f.deleteCalled = true
	f.lastDeleteID = instanceID
	if f.deleteFunc != nil {
		return f.deleteFunc(instanceID)
	}
	return nil
}
func (f *fakeClusterClient) GetInstanceStatus(_ context.Context, _ cluster.Scope, id string) (*cluster.InstanceStatus, error) {
	if f.statusByID != nil {
		return f.statusByID(id)
	}
	if f.statusFunc != nil {
		return f.statusFunc()
	}
	return nil, cluster.ErrNotFound
}
func (f *fakeClusterClient) ListInstanceStatuses(context.Context, cluster.Scope) ([]cluster.InstanceStatus, error) {
	return nil, nil
}
func (f *fakeClusterClient) StreamLogs(context.Context, cluster.Scope, string, cluster.LogOptions, io.Writer) error {
	return errors.New("not implemented in fake")
}
func (f *fakeClusterClient) Inventory(context.Context) ([]cluster.NodeInventory, error) {
	return nil, nil
}
func (f *fakeClusterClient) InstanceMetrics(context.Context) ([]cluster.InstanceMetric, error) {
	return nil, nil
}
func (f *fakeClusterClient) Healthy(context.Context) bool { return true }
func (f *fakeClusterClient) ResolveInstanceAddress(context.Context, string, int32) (string, error) {
	if f.addrFunc != nil {
		return f.addrFunc()
	}
	return "", cluster.ErrNotFound
}

var _ cluster.Client = (*fakeClusterClient)(nil)

// testDB backs one service (nodeservices or nodes) with its own sqlmock —
// separate instances are fine, since nothing in the reconciler joins
// across them at the SQL level; it only calls each service's own Go
// methods.
func newNodeServicesMock(t *testing.T) (*nodeservices.Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return nodeservices.NewService(db), mock, func() { db.Close() }
}

func newNodesMock(t *testing.T) (*nodes.Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return nodes.NewService(db), mock, func() { db.Close() }
}

func nodeServiceColumns() []string {
	return []string{
		"id", "node_id", "kind", "config", "desired_state", "observed_state",
		"observed_error", "observed_endpoint", "observed_at", "created_by", "created_at", "updated_at",
	}
}

func nodeColumns() []string {
	return []string{
		"id", "node_name", "provider_id", "class", "region",
		"cpu_cores", "memory_gb", "p_cores", "e_cores", "gpu_model",
		"gpu_count", "mig_capable", "os", "arch",
		"agent_version", "status", "last_seen_at", "revoked_at",
		"rentable_cpu_cores", "rentable_memory_gb", "k8s_ready",
		"latitude", "longitude", "location_label",
		"created_at", "updated_at",
	}
}

func validConfig() []byte {
	cfg := map[string]any{
		"model_route":  "teepin/qwen3-omni-7b",
		"engine":       "vllm-omni",
		"model_source": "https://huggingface.co/Qwen/Qwen3-Omni-7B-Instruct",
		"storage_gb":   200,
		"cpu_units":    8,
		"memory_gb":    32,
		"gpu_count":    1,
	}
	b, _ := json.Marshal(cfg)
	return b
}

func TestReconcile_MountsPendingRow(t *testing.T) {
	nsSvc, nsMock, done1 := newNodeServicesMock(t)
	defer done1()
	nodesSvc, nodesMock, done2 := newNodesMock(t)
	defer done2()

	rowID := uuid.New()
	nodeID := uuid.New()
	now := time.Now()
	cfg := validConfig()

	nsMock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WithArgs("inference_model").
		WillReturnRows(sqlmock.NewRows(nodeServiceColumns()).AddRow(
			rowID, nodeID, "inference_model", cfg, "mounted", "pending",
			nil, nil, nil, "op", now, now,
		))

	nodesMock.ExpectQuery(`SELECT id, node_name, provider_id, class`).
		WillReturnRows(sqlmock.NewRows(nodeColumns()).AddRow(
			nodeID, "srialla", "provider-srialla", "home", "home",
			30, 42, 8, 16, "",
			0, false, "linux", "amd64",
			"dev", "online", &now, nil,
			18, 42, true,
			nil, nil, "",
			now, now,
		))

	nsMock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("pending", nil, nil, rowID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	fake := &fakeClusterClient{}
	r := New(nsSvc, nodesSvc, fake, EngineConfig{VLLMOmniImage: "vllm-omni/vllm-omni:latest"})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !fake.createCalled {
		t.Fatal("CreateInstance was not called for a pending mount")
	}
	if fake.lastCreateSpec.NodeClass != "home" || fake.lastCreateSpec.ProviderID != "provider-srialla" {
		t.Errorf("spec routed as NodeClass=%q ProviderID=%q, want home/provider-srialla",
			fake.lastCreateSpec.NodeClass, fake.lastCreateSpec.ProviderID)
	}
	if err := nsMock.ExpectationsWereMet(); err != nil {
		t.Errorf("nodeservices unmet: %v", err)
	}
	if err := nodesMock.ExpectationsWereMet(); err != nil {
		t.Errorf("nodes unmet: %v", err)
	}
}

func TestReconcile_UnmountsMountedRow(t *testing.T) {
	nsSvc, nsMock, done1 := newNodeServicesMock(t)
	defer done1()
	nodesSvc, nodesMock, done2 := newNodesMock(t)
	defer done2()

	rowID := uuid.New()
	nodeID := uuid.New()
	now := time.Now()
	endpoint := "http://10.0.0.42:8000"

	nsMock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WithArgs("inference_model").
		WillReturnRows(sqlmock.NewRows(nodeServiceColumns()).AddRow(
			rowID, nodeID, "inference_model", validConfig(), "unmounted", "mounted",
			nil, &endpoint, now, "op", now, now,
		))

	nodesMock.ExpectQuery(`SELECT id, node_name, provider_id, class`).
		WillReturnRows(sqlmock.NewRows(nodeColumns())) // unmount never needs to look at nodes

	nsMock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("unmounted", nil, nil, rowID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	fake := &fakeClusterClient{}
	r := New(nsSvc, nodesSvc, fake, EngineConfig{VLLMOmniImage: "vllm-omni/vllm-omni:latest"})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !fake.deleteCalled {
		t.Fatal("DeleteInstance was not called for a row desired-unmounted")
	}
	if fake.lastDeleteID != instanceIDFor(rowID.String()) {
		t.Errorf("deleted instance id = %q, want %q", fake.lastDeleteID, instanceIDFor(rowID.String()))
	}
}

// TestReconcile_AlreadyConvergedDoesNothing proves a row whose desired and
// observed state already agree triggers neither Create nor Delete — the
// reconciler must be idempotent against its own prior success, not
// re-create a real instance on every single pass.
func TestReconcile_AlreadyConvergedDoesNothing(t *testing.T) {
	nsSvc, nsMock, done1 := newNodeServicesMock(t)
	defer done1()
	nodesSvc, nodesMock, done2 := newNodesMock(t)
	defer done2()

	rowID := uuid.New()
	nodeID := uuid.New()
	now := time.Now()
	endpoint := "http://10.0.0.42:8000"

	nsMock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WithArgs("inference_model").
		WillReturnRows(sqlmock.NewRows(nodeServiceColumns()).AddRow(
			rowID, nodeID, "inference_model", validConfig(), "mounted", "mounted",
			nil, &endpoint, now, "op", now, now,
		))
	nodesMock.ExpectQuery(`SELECT id, node_name, provider_id, class`).
		WillReturnRows(sqlmock.NewRows(nodeColumns()))

	fake := &fakeClusterClient{statusFunc: runningStatus}
	r := New(nsSvc, nodesSvc, fake, EngineConfig{VLLMOmniImage: "vllm-omni/vllm-omni:latest"})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if fake.createCalled || fake.deleteCalled {
		t.Error("an already-converged row triggered Create or Delete")
	}
	if err := nsMock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet — a converged row must not attempt any observed-state write: %v", err)
	}
}

func TestReconcile_NodeNotFoundReportsErrorWithoutCallingCluster(t *testing.T) {
	nsSvc, nsMock, done1 := newNodeServicesMock(t)
	defer done1()
	nodesSvc, nodesMock, done2 := newNodesMock(t)
	defer done2()

	rowID := uuid.New()
	missingNodeID := uuid.New()
	now := time.Now()

	nsMock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WithArgs("inference_model").
		WillReturnRows(sqlmock.NewRows(nodeServiceColumns()).AddRow(
			rowID, missingNodeID, "inference_model", validConfig(), "mounted", "pending",
			nil, nil, nil, "op", now, now,
		))
	nodesMock.ExpectQuery(`SELECT id, node_name, provider_id, class`).
		WillReturnRows(sqlmock.NewRows(nodeColumns())) // no matching node

	nsMock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("error", sqlmock.AnyArg(), nil, rowID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	fake := &fakeClusterClient{}
	r := New(nsSvc, nodesSvc, fake, EngineConfig{VLLMOmniImage: "vllm-omni/vllm-omni:latest"})

	err := r.Reconcile(context.Background())
	if err == nil {
		t.Fatal("expected Reconcile to report an error for a missing node")
	}
	if fake.createCalled {
		t.Error("CreateInstance called despite the node not being found")
	}
}

func TestReconcile_ClusterFailureReportsErrorState(t *testing.T) {
	nsSvc, nsMock, done1 := newNodeServicesMock(t)
	defer done1()
	nodesSvc, nodesMock, done2 := newNodesMock(t)
	defer done2()

	rowID := uuid.New()
	nodeID := uuid.New()
	now := time.Now()

	nsMock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WithArgs("inference_model").
		WillReturnRows(sqlmock.NewRows(nodeServiceColumns()).AddRow(
			rowID, nodeID, "inference_model", validConfig(), "mounted", "pending",
			nil, nil, nil, "op", now, now,
		))
	nodesMock.ExpectQuery(`SELECT id, node_name, provider_id, class`).
		WillReturnRows(sqlmock.NewRows(nodeColumns()).AddRow(
			nodeID, "srialla", "provider-srialla", "home", "home",
			30, 42, 8, 16, "",
			0, false, "linux", "amd64",
			"dev", "online", &now, nil,
			18, 42, true,
			nil, nil, "",
			now, now,
		))
	nsMock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("error", sqlmock.AnyArg(), nil, rowID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	fake := &fakeClusterClient{createFunc: func(cluster.InstanceSpec) (*cluster.InstanceResult, error) {
		return nil, errors.New("gpu resource exhausted")
	}}
	r := New(nsSvc, nodesSvc, fake, EngineConfig{VLLMOmniImage: "vllm-omni/vllm-omni:latest"})

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("expected Reconcile to surface the cluster failure")
	}
	if err := nsMock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet — the failure must still be reported via ReportObserved: %v", err)
	}
}

func runningStatus() (*cluster.InstanceStatus, error) {
	return &cluster.InstanceStatus{Status: "running"}, nil
}

// expectRowAndNode primes both mocks with one inference_model row (given
// desired/observed states) and one node of the given class.
func expectRowAndNode(nsMock, nodesMock sqlmock.Sqlmock, rowID, nodeID uuid.UUID, engineCfg []byte, desired, observed, class string) {
	now := time.Now()
	nsMock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WithArgs("inference_model").
		WillReturnRows(sqlmock.NewRows(nodeServiceColumns()).AddRow(
			rowID, nodeID, "inference_model", engineCfg, desired, observed,
			nil, nil, nil, "op", now, now,
		))
	nodesMock.ExpectQuery(`SELECT id, node_name, provider_id, class`).
		WillReturnRows(sqlmock.NewRows(nodeColumns()).AddRow(
			nodeID, "mac", "provider-mac", class, class,
			10, 24, 4, 6, "",
			0, false, "darwin", "arm64",
			"dev", "online", &now, nil,
			8, 24, false,
			nil, nil, "",
			now, now,
		))
}

func mlxConfig() []byte {
	b, _ := json.Marshal(map[string]any{
		"model_route":  "teepin/k2",
		"engine":       "mlx",
		"model_source": "https://huggingface.co/DreamFoundries/K2-Horizon-MoVA-36B-A4B-MLX-4bit",
		"memory_gb":    8,
	})
	return b
}

// A pending row whose instance is now running becomes Mounted, with a
// tunnel:// endpoint for a home node (the gateway cannot dial a NAT'd Mac).
func TestReconcile_RunningHomeInstanceBecomesMountedWithTunnelEndpoint(t *testing.T) {
	nsSvc, nsMock, d1 := newNodeServicesMock(t)
	defer d1()
	nodesSvc, nodesMock, d2 := newNodesMock(t)
	defer d2()
	rowID, nodeID := uuid.New(), uuid.New()
	expectRowAndNode(nsMock, nodesMock, rowID, nodeID, mlxConfig(), "mounted", "pending", "home")

	want := "tunnel://provider-mac/" + instanceIDFor(rowID.String()) + ":8000"
	nsMock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("mounted", nil, want, rowID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	fake := &fakeClusterClient{statusFunc: runningStatus}
	r := New(nsSvc, nodesSvc, fake, EngineConfig{})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if fake.createCalled {
		t.Error("CreateInstance called for an instance that is already running")
	}
	if err := nsMock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A still-starting instance stays Pending (no endpoint advertised).
func TestReconcile_PendingInstanceStaysPending(t *testing.T) {
	nsSvc, nsMock, d1 := newNodeServicesMock(t)
	defer d1()
	nodesSvc, nodesMock, d2 := newNodesMock(t)
	defer d2()
	rowID, nodeID := uuid.New(), uuid.New()
	expectRowAndNode(nsMock, nodesMock, rowID, nodeID, mlxConfig(), "mounted", "pending", "home")
	nsMock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("pending", nil, nil, rowID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	fake := &fakeClusterClient{statusFunc: func() (*cluster.InstanceStatus, error) {
		return &cluster.InstanceStatus{Status: "pending"}, nil
	}}
	r := New(nsSvc, nodesSvc, fake, EngineConfig{})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if fake.createCalled {
		t.Error("CreateInstance called for an instance that already exists")
	}
	if err := nsMock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A failed instance is cleaned up and reported as an error with its message.
func TestReconcile_FailedInstanceReportsErrorAndCleansUp(t *testing.T) {
	nsSvc, nsMock, d1 := newNodeServicesMock(t)
	defer d1()
	nodesSvc, nodesMock, d2 := newNodesMock(t)
	defer d2()
	rowID, nodeID := uuid.New(), uuid.New()
	expectRowAndNode(nsMock, nodesMock, rowID, nodeID, mlxConfig(), "mounted", "pending", "home")
	nsMock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("error", sqlmock.AnyArg(), nil, rowID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	fake := &fakeClusterClient{statusFunc: func() (*cluster.InstanceStatus, error) {
		return &cluster.InstanceStatus{Status: "failed", Message: "mlx_lm.server: command not found"}, nil
	}}
	r := New(nsSvc, nodesSvc, fake, EngineConfig{})
	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("expected the instance failure to be surfaced")
	}
	if !fake.deleteCalled {
		t.Error("failed instance was not deleted, so the next pass could not retry cleanly")
	}
	if err := nsMock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Drift: a row recorded as mounted whose instance vanished is remounted.
func TestReconcile_MountedRowWithVanishedInstanceIsRemounted(t *testing.T) {
	nsSvc, nsMock, d1 := newNodeServicesMock(t)
	defer d1()
	nodesSvc, nodesMock, d2 := newNodesMock(t)
	defer d2()
	rowID, nodeID := uuid.New(), uuid.New()
	expectRowAndNode(nsMock, nodesMock, rowID, nodeID, mlxConfig(), "mounted", "mounted", "home")
	nsMock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("pending", nil, nil, rowID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	fake := &fakeClusterClient{} // default status: ErrNotFound
	r := New(nsSvc, nodesSvc, fake, EngineConfig{})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !fake.createCalled {
		t.Fatal("vanished instance was not recreated")
	}
	if len(fake.lastCreateSpec.Command) == 0 || fake.lastCreateSpec.NodeClass != "home" {
		t.Errorf("recreated spec = %+v, want a native home spec", fake.lastCreateSpec)
	}
}

// A status-lookup error on a healthy mounted row must not flap it.
func TestReconcile_TransientStatusErrorDoesNotTouchMountedRow(t *testing.T) {
	nsSvc, nsMock, d1 := newNodeServicesMock(t)
	defer d1()
	nodesSvc, nodesMock, d2 := newNodesMock(t)
	defer d2()
	rowID, nodeID := uuid.New(), uuid.New()
	expectRowAndNode(nsMock, nodesMock, rowID, nodeID, mlxConfig(), "mounted", "mounted", "home")

	fake := &fakeClusterClient{statusFunc: func() (*cluster.InstanceStatus, error) {
		return nil, errors.New("tunnel timeout")
	}}
	r := New(nsSvc, nodesSvc, fake, EngineConfig{})
	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("expected the status error to be returned")
	}
	if fake.createCalled || fake.deleteCalled {
		t.Error("a transient status error triggered a create/delete")
	}
	if err := nsMock.ExpectationsWereMet(); err != nil {
		t.Errorf("row was written despite a transient error: %v", err)
	}
}

func mlxConfigGB(gb int) []byte {
	b, _ := json.Marshal(map[string]any{
		"model_route":  "teepin/m",
		"engine":       "mlx",
		"model_source": "https://huggingface.co/a/b",
		"memory_gb":    gb,
	})
	return b
}

func TestReconcile_MLXRefusedWhenModelExceedsNodeBudget(t *testing.T) {
	nsSvc, nsMock, d1 := newNodeServicesMock(t)
	defer d1()
	nodesSvc, nodesMock, d2 := newNodesMock(t)
	defer d2()
	rowID, nodeID := uuid.New(), uuid.New()
	// node has 24GB, default reserve 6 -> 18GB budget; 21GB model cannot fit.
	expectRowAndNode(nsMock, nodesMock, rowID, nodeID, mlxConfigGB(21), "mounted", "pending", "home")
	nsMock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("error", sqlmock.AnyArg(), nil, rowID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	fake := &fakeClusterClient{}
	r := New(nsSvc, nodesSvc, fake, EngineConfig{})
	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("21GB model admitted on an 18GB budget")
	}
	if fake.createCalled {
		t.Error("instance created despite failing admission")
	}
}

func TestReconcile_MLXBudgetOverrideAdmitsLargerModel(t *testing.T) {
	nsSvc, nsMock, d1 := newNodeServicesMock(t)
	defer d1()
	nodesSvc, nodesMock, d2 := newNodesMock(t)
	defer d2()
	rowID, nodeID := uuid.New(), uuid.New()
	expectRowAndNode(nsMock, nodesMock, rowID, nodeID, mlxConfigGB(21), "mounted", "pending", "home")
	nsMock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("pending", nil, nil, rowID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	fake := &fakeClusterClient{}
	r := New(nsSvc, nodesSvc, fake, EngineConfig{NodeReserveGB: 2}) // 22GB budget
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !fake.createCalled {
		t.Error("model within the overridden budget was not started")
	}
}

// Mounting a new model that does not fit alongside an older one evicts the
// older one first (delete before create), then starts the new one.
func TestReconcile_MLXEvictsOlderModelToMakeRoom(t *testing.T) {
	nsSvc, nsMock, d1 := newNodeServicesMock(t)
	defer d1()
	nodesSvc, nodesMock, d2 := newNodesMock(t)
	defer d2()
	oldID, newID, nodeID := uuid.New(), uuid.New(), uuid.New()
	old := time.Now().Add(-time.Hour)
	now := time.Now()

	nsMock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WithArgs("inference_model").
		WillReturnRows(sqlmock.NewRows(nodeServiceColumns()).
			AddRow(oldID, nodeID, "inference_model", mlxConfigGB(12), "mounted", "mounted", nil, nil, nil, "op", old, old).
			AddRow(newID, nodeID, "inference_model", mlxConfigGB(12), "mounted", "pending", nil, nil, nil, "op", now, now))
	nodesMock.ExpectQuery(`SELECT id, node_name, provider_id, class`).
		WillReturnRows(sqlmock.NewRows(nodeColumns()).AddRow(
			nodeID, "mac", "provider-mac", "home", "home",
			10, 24, 4, 6, "", 0, false, "darwin", "arm64",
			"dev", "online", &now, nil, 8, 24, false, nil, nil, "", now, now))

	// Order in this pass: the old row is reconciled first only for drift
	// (running -> nothing); the new row then evicts it.
	nsMock.ExpectExec(`UPDATE compute\.node_services\s+SET desired_state`).
		WithArgs(oldID).WillReturnResult(sqlmock.NewResult(0, 1))
	nsMock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("unmounted", nil, nil, oldID).WillReturnResult(sqlmock.NewResult(0, 1))
	nsMock.ExpectExec(`UPDATE compute\.node_services\s+SET observed_state`).
		WithArgs("pending", nil, nil, newID).WillReturnResult(sqlmock.NewResult(0, 1))

	var calls []string
	fake := &fakeClusterClient{
		statusByID: func(id string) (*cluster.InstanceStatus, error) {
			if id == instanceIDFor(oldID.String()) {
				return &cluster.InstanceStatus{Status: "running"}, nil
			}
			return nil, cluster.ErrNotFound
		},
		deleteFunc: func(string) error { calls = append(calls, "delete"); return nil },
		createFunc: func(cluster.InstanceSpec) (*cluster.InstanceResult, error) {
			calls = append(calls, "create")
			return &cluster.InstanceResult{}, nil
		},
	}
	r := New(nsSvc, nodesSvc, fake, EngineConfig{})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	di, ci := -1, -1
	for i, c := range calls {
		if c == "delete" && di < 0 {
			di = i
		}
		if c == "create" && ci < 0 {
			ci = i
		}
	}
	if di < 0 || ci < 0 || di > ci {
		t.Errorf("call order = %v, want delete before create", calls)
	}
	if err := nsMock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
