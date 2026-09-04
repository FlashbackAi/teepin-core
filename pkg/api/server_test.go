// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
	"github.com/FlashbackAi/teepin-core/pkg/gpu"
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
	"github.com/FlashbackAi/teepin-core/pkg/models"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// fakeCluster is an in-memory cluster.Client.
//
// It enforces the Scope predicate exactly as DirectClient does against
// real label selectors, because that is the behaviour these handler
// tests depend on: a handler that forgets to pass a scope must fail
// here, not silently pass because the fake ignores tenancy.
type fakeCluster struct {
	instances map[string]fakeInstance

	// failWith, when set, is returned by every operation. Used to test
	// how handlers degrade when GPU capacity is unreachable.
	failWith error

	// lastSpec captures the most recent CreateInstance spec, so placement
	// tests can assert what the handler resolved.
	lastSpec cluster.InstanceSpec

	// nextResult, when set, is returned verbatim (PodName defaulted if
	// empty) by the next CreateInstance call — lets endpoint-field tests
	// control exactly what the "agent" reports back.
	nextResult *cluster.InstanceResult
}

type fakeInstance struct {
	projectID string
	status    string
	message   string
	logs      string
	endpoint  string
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{instances: map[string]fakeInstance{}}
}

func (f *fakeCluster) add(instanceID, projectID, status string) {
	f.instances[instanceID] = fakeInstance{
		projectID: projectID,
		status:    status,
		logs:      "log line one\nlog line two\n",
	}
}

// setEndpoint mutates an already-added instance's reported EndpointURL —
// a separate setter rather than a wider add() so its 10 existing call
// sites (which never cared about an endpoint) stay untouched.
func (f *fakeCluster) setEndpoint(instanceID, url string) {
	inst := f.instances[instanceID]
	inst.endpoint = url
	f.instances[instanceID] = inst
}

// visible applies the tenancy predicate. Mirrors the label-selector
// filtering DirectClient delegates to Kubernetes.
func (f *fakeCluster) visible(scope cluster.Scope, id string) (fakeInstance, bool) {
	inst, ok := f.instances[id]
	if !ok {
		return fakeInstance{}, false
	}
	if scope.ProjectID != "" && inst.projectID != scope.ProjectID {
		return fakeInstance{}, false
	}
	return inst, true
}

func (f *fakeCluster) CreateInstance(_ context.Context, spec cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	f.lastSpec = spec
	if f.failWith != nil {
		return nil, f.failWith
	}
	f.instances[spec.InstanceID] = fakeInstance{
		projectID: spec.ProjectID,
		status:    compute.StatusPending,
	}
	if f.nextResult != nil {
		result := *f.nextResult
		if result.PodName == "" {
			result.PodName = spec.InstanceID + "-pod"
		}
		return &result, nil
	}
	return &cluster.InstanceResult{PodName: spec.InstanceID + "-pod"}, nil
}

func (f *fakeCluster) UpdateInstance(_ context.Context, _ cluster.Scope, spec cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	f.lastSpec = spec
	if f.failWith != nil {
		return nil, f.failWith
	}
	if _, ok := f.instances[spec.InstanceID]; !ok {
		return nil, cluster.ErrNotFound
	}
	f.instances[spec.InstanceID] = fakeInstance{
		projectID: spec.ProjectID,
		status:    compute.StatusPending,
	}
	if f.nextResult != nil {
		result := *f.nextResult
		if result.PodName == "" {
			result.PodName = spec.InstanceID + "-pod"
		}
		return &result, nil
	}
	return &cluster.InstanceResult{PodName: spec.InstanceID + "-pod"}, nil
}

func (f *fakeCluster) DeleteInstance(_ context.Context, scope cluster.Scope, id string) error {
	if f.failWith != nil {
		return f.failWith
	}
	if _, ok := f.visible(scope, id); ok {
		delete(f.instances, id)
	}
	// Idempotent: deleting what is not there (or not yours) is not an
	// error at this layer.
	return nil
}

func (f *fakeCluster) GetInstanceStatus(_ context.Context, scope cluster.Scope, id string) (*cluster.InstanceStatus, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	inst, ok := f.visible(scope, id)
	if !ok {
		return nil, cluster.ErrNotFound
	}
	return &cluster.InstanceStatus{
		InstanceID:  id,
		Status:      inst.status,
		Message:     inst.message,
		EndpointURL: inst.endpoint,
		ObservedAt:  time.Now().UTC(),
	}, nil
}

func (f *fakeCluster) ListInstanceStatuses(_ context.Context, scope cluster.Scope) ([]cluster.InstanceStatus, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	var out []cluster.InstanceStatus
	for id, inst := range f.instances {
		if scope.ProjectID != "" && inst.projectID != scope.ProjectID {
			continue
		}
		out = append(out, cluster.InstanceStatus{
			InstanceID: id,
			Status:     inst.status,
			Message:    inst.message,
			ObservedAt: time.Now().UTC(),
		})
	}
	return out, nil
}

func (f *fakeCluster) StreamLogs(_ context.Context, scope cluster.Scope, id string, _ cluster.LogOptions, w io.Writer) error {
	if f.failWith != nil {
		return f.failWith
	}
	inst, ok := f.visible(scope, id)
	if !ok {
		return cluster.ErrNotFound
	}
	_, err := io.WriteString(w, inst.logs)
	return err
}

func (f *fakeCluster) Inventory(context.Context) ([]cluster.NodeInventory, error) {
	return nil, f.failWith
}

func (f *fakeCluster) InstanceMetrics(context.Context) ([]cluster.InstanceMetric, error) {
	return nil, f.failWith
}

func (f *fakeCluster) Healthy(context.Context) bool { return f.failWith == nil }

func (f *fakeCluster) ResolveInstanceAddress(context.Context, string, int32) (string, error) {
	return "", f.failWith
}

// newTenantServer builds a Server with tenancy active (store present)
// over a fake cluster, with a permissive payment gate so tenancy tests
// are not blocked by billing.
func newTenantServer(t *testing.T, fc *fakeCluster) *Server {
	t.Helper()
	return newTenantServerWithGate(t, fc, allowGate{})
}

// newTenantServerWithGate is newTenantServer with an explicit gate, for
// the tests that assert payment-gate behaviour.
func newTenantServerWithGate(t *testing.T, fc *fakeCluster, gate ProvisionGate) *Server {
	t.Helper()

	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return NewServer(fc, nil, compute.NewStore(db), nil, gate)
}

// A fixed account for tenancy tests. requireScope now requires an
// account claim (an API key always carries one); the specific value does
// not matter to cluster-tenancy tests, only that it is present.
var testAccountID = uuid.MustParse("00000000-0000-0000-0000-0000000000aa")

// doRequest performs a request against the handler with the caller's
// project injected the way auth middleware would. It also injects an
// account, because every real compute credential carries one and
// requireScope now requires it.
func doRequest(handler gin.HandlerFunc, method, path string, params gin.Params, projectID uuid.UUID) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, nil)
	c.Params = params
	if projectID != uuid.Nil {
		c.Set(string(auth.ProjectIDKey), projectID)
		c.Set(string(auth.AccountIDKey), testAccountID)
	}
	handler(c)
	return w
}

// allowGate is a permissive ProvisionGate for cluster-tenancy tests,
// which are not about billing — it always allows, so the payment gate
// never interferes with what those tests actually assert.
type allowGate struct{}

func (allowGate) AccountCanProvision(context.Context, uuid.UUID) (bool, string, error) {
	return true, "", nil
}

// denyGate blocks provisioning, for the tests that assert the gate.
type denyGate struct {
	reason string
	err    error
}

func (g denyGate) AccountCanProvision(context.Context, uuid.UUID) (bool, string, error) {
	if g.err != nil {
		return false, "", g.err
	}
	return false, g.reason, nil
}

// The tenancy tests below are the reason this package has tests at all.
// An earlier version let any authenticated caller read another
// customer's instance by ID; these pin that boundary at the handler
// layer, where the scope is chosen.

func TestListInstances_ScopedToCallerProject(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()

	fc := newFakeCluster()
	fc.add("inst-aaaa1111", tenantA.String(), compute.StatusRunning)
	fc.add("inst-bbbb2222", tenantB.String(), compute.StatusRunning)

	server := newTenantServer(t, fc)

	w := doRequest(server.ListInstances, http.MethodGet, "/v1/compute/instances", nil, tenantA)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	var resp struct {
		Count     int `json:"count"`
		Instances []struct {
			ID string `json:"id"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	if resp.Count != 1 || len(resp.Instances) != 1 || resp.Instances[0].ID != "inst-aaaa1111" {
		t.Errorf("tenant A must see only its own instance, got %s", w.Body.String())
	}
}

func TestListInstances_RequiresAuthWhenTenancyActive(t *testing.T) {
	server := newTenantServer(t, newFakeCluster())

	w := doRequest(server.ListInstances, http.MethodGet, "/v1/compute/instances", nil, uuid.Nil)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without project context", w.Code)
	}
}

func TestGetInstance_CrossTenantIs404(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()

	fc := newFakeCluster()
	fc.add("inst-aaaa1111", tenantA.String(), compute.StatusRunning)
	server := newTenantServer(t, fc)

	params := gin.Params{{Key: "id", Value: "inst-aaaa1111"}}

	// Owner sees it.
	if w := doRequest(server.GetInstance, http.MethodGet, "/", params, tenantA); w.Code != http.StatusOK {
		t.Errorf("owner GET status = %d, want 200", w.Code)
	}

	// Another tenant gets 404 — indistinguishable from nonexistent, so
	// existence does not leak.
	if w := doRequest(server.GetInstance, http.MethodGet, "/", params, tenantB); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant GET status = %d, want 404", w.Code)
	}
}

func TestDeleteInstance_CrossTenantIs404AndDoesNotDelete(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()

	fc := newFakeCluster()
	fc.add("inst-aaaa1111", tenantA.String(), compute.StatusRunning)
	server := newTenantServer(t, fc)

	params := gin.Params{{Key: "id", Value: "inst-aaaa1111"}}

	if w := doRequest(server.DeleteInstance, http.MethodDelete, "/", params, tenantB); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant DELETE status = %d, want 404", w.Code)
	}

	// The 404 must not be cosmetic: the instance has to survive.
	if _, ok := fc.instances["inst-aaaa1111"]; !ok {
		t.Error("another tenant's instance was deleted despite the 404")
	}
}

func TestDeleteInstance_NonexistentIs404(t *testing.T) {
	server := newTenantServer(t, newFakeCluster())

	params := gin.Params{{Key: "id", Value: "inst-nope0000"}}
	if w := doRequest(server.DeleteInstance, http.MethodDelete, "/", params, uuid.New()); w.Code != http.StatusNotFound {
		t.Errorf("DELETE nonexistent status = %d, want 404 (was silently 200 before)", w.Code)
	}
}

func TestGetInstanceLogs_CrossTenantIs404(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()

	fc := newFakeCluster()
	fc.add("inst-aaaa1111", tenantA.String(), compute.StatusRunning)
	server := newTenantServer(t, fc)

	params := gin.Params{{Key: "id", Value: "inst-aaaa1111"}}

	// Logs routinely carry credentials, prompts and customer data.
	w := doRequest(server.GetInstanceLogs, http.MethodGet, "/", params, tenantB)
	if w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant logs status = %d, want 404", w.Code)
	}
	if strings.Contains(w.Body.String(), "log line") {
		t.Error("another tenant's log content leaked into the response")
	}
}

func TestGetInstanceLogs_OwnerGetsContent(t *testing.T) {
	tenant := uuid.New()

	fc := newFakeCluster()
	fc.add("inst-aaaa1111", tenant.String(), compute.StatusRunning)
	server := newTenantServer(t, fc)

	params := gin.Params{{Key: "id", Value: "inst-aaaa1111"}}
	w := doRequest(server.GetInstanceLogs, http.MethodGet, "/", params, tenant)

	if w.Code != http.StatusOK {
		t.Fatalf("owner logs status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "log line one") {
		t.Errorf("owner should receive log content, got %s", w.Body.String())
	}
}

// GetInstanceMetrics is scoped the SAME way GetInstanceLogs already is —
// a cross-tenant request never learns an instance exists.
func TestGetInstanceMetrics_CrossTenantIs404(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()

	fc := newFakeCluster()
	fc.add("inst-aaaa1111", tenantA.String(), compute.StatusRunning)
	server := newTenantServer(t, fc)

	params := gin.Params{{Key: "id", Value: "inst-aaaa1111"}}
	w := doRequest(server.GetInstanceMetrics, http.MethodGet, "/", params, tenantB)
	if w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant metrics status = %d, want 404", w.Code)
	}
}

func TestGetInstanceMetrics_UnknownInstanceIs404(t *testing.T) {
	server := newTenantServer(t, newFakeCluster())

	params := gin.Params{{Key: "id", Value: "inst-nope0000"}}
	w := doRequest(server.GetInstanceMetrics, http.MethodGet, "/", params, uuid.New())
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown instance status = %d, want 404", w.Code)
	}
}

// Standalone mode (no database — store is nil) has no history to read at
// all, which is an honest 501, not a 200 with an empty/fabricated list.
func TestGetInstanceMetrics_StandaloneModeIs501(t *testing.T) {
	fc := newFakeCluster()
	fc.add("inst-aaaa1111", "", compute.StatusRunning)
	server := NewServer(fc, nil, nil, nil, allowGate{})

	params := gin.Params{{Key: "id", Value: "inst-aaaa1111"}}
	w := doRequest(server.GetInstanceMetrics, http.MethodGet, "/", params, uuid.New())
	if w.Code != http.StatusNotImplemented {
		t.Errorf("standalone mode status = %d, want 501", w.Code)
	}
}

// The owner gets their instance's real samples, oldest first.
func TestGetInstanceMetrics_OwnerGetsSamples(t *testing.T) {
	tenant := uuid.New()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	fc := newFakeCluster()
	fc.add("inst-aaaa1111", tenant.String(), compute.StatusRunning)
	server := NewServer(fc, nil, compute.NewStore(db), nil, allowGate{})

	recordedAt := time.Now().Add(-5 * time.Minute)
	mock.ExpectQuery(`SELECT recorded_at, cpu_used_percent, memory_used_gb, network_rx_mbps, network_tx_mbps, storage_used_gb`).
		WithArgs("inst-aaaa1111", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"recorded_at", "cpu_used_percent", "memory_used_gb", "network_rx_mbps", "network_tx_mbps", "storage_used_gb"}).
			AddRow(recordedAt, 55.5, 2.25, 4.0, 1.0, 3.5))

	params := gin.Params{{Key: "id", Value: "inst-aaaa1111"}}
	w := doRequest(server.GetInstanceMetrics, http.MethodGet, "/?since=1h", params, tenant)

	if w.Code != http.StatusOK {
		t.Fatalf("owner metrics status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		InstanceID string                        `json:"instance_id"`
		Samples    []compute.InstanceMetricSample `json:"samples"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.InstanceID != "inst-aaaa1111" {
		t.Errorf("instance_id = %q, want inst-aaaa1111", resp.InstanceID)
	}
	if len(resp.Samples) != 1 {
		t.Fatalf("got %d samples, want 1", len(resp.Samples))
	}
	if resp.Samples[0].CPUUsedPercent != 55.5 || resp.Samples[0].StorageUsedGB != 3.5 {
		t.Errorf("sample = %+v, want {55.5 ... 3.5}", resp.Samples[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestClusterUnavailable_IsNot500 pins the behaviour the whole
// control-plane split exists for: when GPU capacity is unreachable, the
// API says so in a way clients retry, rather than reporting a generic
// server error that reads like data loss.
func TestClusterUnavailable_IsNot500(t *testing.T) {
	fc := newFakeCluster()
	fc.failWith = cluster.ErrClusterUnavailable
	server := newTenantServer(t, fc)

	w := doRequest(server.ListInstances, http.MethodGet, "/", nil, uuid.New())

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when the cluster is unreachable", w.Code)
	}
	// The customer must not be led to believe their instances are gone.
	if strings.Contains(strings.ToLower(w.Body.String()), "not found") {
		t.Errorf("unreachable cluster must not be reported as missing instances: %s", w.Body.String())
	}
}

func TestStatusToInstance_UsesRecordForCommercialFields(t *testing.T) {
	st := cluster.InstanceStatus{
		InstanceID: "inst-aaaa1111",
		Status:     compute.StatusRunning,
		ObservedAt: time.Now().UTC(),
	}
	record := &compute.InstanceRecord{
		ID:           "inst-aaaa1111",
		Name:         "trainer",
		Image:        "nginx:latest",
		InstanceType: "gpu.h100.custom-25gb",
		GPUVRAMGB:    25,
		CPUUnits:     8,
		MemoryGB:     32,
	}

	inst := statusToInstance(st, record, gpu.DefaultPricePerGBHour, "")

	// Status comes from the cluster (it observes reality)...
	if inst.Status != compute.StatusRunning {
		t.Errorf("Status = %q, want running", inst.Status)
	}
	// ...while the commercial facts come from the record, so a
	// rescheduled pod cannot change what the customer agreed to pay.
	if inst.GPUVRAM != "25GB" || inst.AllocatedVRAM != "25GB" {
		t.Errorf("VRAM = %q/%q, want 25GB/25GB", inst.GPUVRAM, inst.AllocatedVRAM)
	}
	if inst.PricePerHour != 2.50 {
		t.Errorf("PricePerHour = %.2f, want 2.50", inst.PricePerHour)
	}
	if inst.InstanceType != "gpu.h100.custom-25gb" {
		t.Errorf("InstanceType = %q, want gpu.h100.custom-25gb", inst.InstanceType)
	}
	if inst.Image != "nginx:latest" {
		t.Errorf("Image = %q, want nginx:latest", inst.Image)
	}
}

// TestStatusToInstance_HomeStorageGetsDurabilityWarning is the
// regression test for the gap pkg/cluster/direct.go's buildPod comment
// already flagged ("the console must say so") but nothing implemented:
// a home-class instance with a persistent volume must carry a warning
// that its data lives on one consumer node's own disk, unlike datacenter
// network storage. Found live 2026-09-02.
func TestStatusToInstance_HomeStorageGetsDurabilityWarning(t *testing.T) {
	st := cluster.InstanceStatus{InstanceID: "inst-home0001", Status: compute.StatusRunning, ObservedAt: time.Now().UTC()}
	record := &compute.InstanceRecord{
		ID: "inst-home0001", Name: "scraper", Image: "python:3.12-slim",
		InstanceType: "cpu.home", CPUUnits: 2, MemoryGB: 4, StorageGB: 20,
	}

	inst := statusToInstance(st, record, gpu.DefaultPricePerGBHour, "")

	if inst.StorageWarning == "" {
		t.Error("a home-class instance with storage_gb > 0 must carry StorageWarning, got empty")
	}
}

// TestStatusToInstance_DatacenterStorageGetsNoWarning proves the warning
// is conditional, not blanket: a datacenter instance's storage is real
// network storage, not tied to one physical machine's uptime, so it must
// never carry the home-specific warning text.
func TestStatusToInstance_DatacenterStorageGetsNoWarning(t *testing.T) {
	st := cluster.InstanceStatus{InstanceID: "inst-dc000001", Status: compute.StatusRunning, ObservedAt: time.Now().UTC()}
	record := &compute.InstanceRecord{
		ID: "inst-dc000001", Name: "trainer", Image: "nginx:latest",
		InstanceType: "gpu.h100.custom-25gb", GPUVRAMGB: 25, CPUUnits: 8, MemoryGB: 32, StorageGB: 100,
	}

	inst := statusToInstance(st, record, gpu.DefaultPricePerGBHour, "")

	if inst.StorageWarning != "" {
		t.Errorf("a datacenter instance must never carry the home storage warning, got %q", inst.StorageWarning)
	}
}

func TestStatusToInstance_SurfacesFailureReason(t *testing.T) {
	st := cluster.InstanceStatus{
		InstanceID: "inst-badimage",
		Status:     compute.StatusFailed,
		Message:    "manifest unknown",
		ObservedAt: time.Now().UTC(),
	}

	inst := statusToInstance(st, nil, gpu.DefaultPricePerGBHour, "")

	// "failed" with no reason sends the customer to support to learn
	// something the cluster already knew.
	if inst.StatusMessage != "manifest unknown" {
		t.Errorf("StatusMessage = %q, want the cluster's explanation", inst.StatusMessage)
	}
}

func TestStatusToInstance_NoRecordIsNotAPanic(t *testing.T) {
	// Standalone mode (no database): the cluster view alone must still
	// produce a usable response rather than nil-dereferencing.
	st := cluster.InstanceStatus{InstanceID: "inst-solo0001", Status: compute.StatusRunning}

	inst := statusToInstance(st, nil, gpu.DefaultPricePerGBHour, "")

	if inst.ID != "inst-solo0001" || inst.Status != compute.StatusRunning {
		t.Errorf("standalone conversion lost data: %+v", inst)
	}
}

func TestParseMemoryGB(t *testing.T) {
	cases := map[string]int{
		"32GB":  32,
		"512MB": 1, // rounds up
		"4G":    4,
		"weird": 0,
		"":      0,
	}
	for in, want := range cases {
		if got := parseMemoryGB(in); got != want {
			t.Errorf("parseMemoryGB(%q) = %d, want %d", in, got, want)
		}
	}
}

// The payment gate: an account with no validated card cannot launch.
func TestCreateInstance_BlockedWithoutPaymentMethod(t *testing.T) {
	server := newTenantServerWithGate(t, newFakeCluster(),
		denyGate{reason: "add a validated payment method before launching resources"})

	w := createInstanceReq(server, uuid.New())
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", w.Code)
	}
	if !strings.Contains(w.Body.String(), "payment_method_required") {
		t.Errorf("body missing machine code: %s", w.Body.String())
	}
}

// The gate must fail CLOSED: a billing-check error denies with 503, it
// does not hand out a GPU.
func TestCreateInstance_GateErrorFailsClosed(t *testing.T) {
	server := newTenantServerWithGate(t, newFakeCluster(),
		denyGate{err: context.DeadlineExceeded})

	w := createInstanceReq(server, uuid.New())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// createInstanceReq issues a POST /instances with an account+project in
// context and a minimal CPU-only body (no GPU, so it needs no allocator).
func createInstanceReq(server *Server, projectID uuid.UUID) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"name":"t","image":"nginx","cpu_units":1,"memory":"1GB"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(auth.ProjectIDKey), projectID)
	c.Set(string(auth.AccountIDKey), testAccountID)
	server.CreateInstance(c)
	return w
}

// --- Home-class placement (Stage 2) --------------------------------------

// fakePlacer records whether PlaceCPU was called — the crux of the opt-in
// guarantee — and returns a canned placement or a chosen error.
type fakePlacer struct {
	called    bool
	lastArch  string
	lastCPU   int
	lastMem   int
	nodeName  string
	provider  string
	arch      string
	err       error
	noCap     bool
	archUnav  bool
	insuffCap bool
}

func (p *fakePlacer) PlaceCPU(_ context.Context, arch string, cpuUnits, memoryGB int) (string, string, string, error) {
	p.called = true
	p.lastArch = arch
	p.lastCPU = cpuUnits
	p.lastMem = memoryGB
	if p.err != nil {
		return "", "", "", p.err
	}
	return p.nodeName, p.provider, p.arch, nil
}
func (p *fakePlacer) IsNoCapacity(error) bool           { return p.noCap }
func (p *fakePlacer) IsArchUnavailable(error) bool      { return p.archUnav }
func (p *fakePlacer) IsInsufficientCapacity(error) bool { return p.insuffCap }

func createInstanceReqBody(server *Server, projectID uuid.UUID, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(auth.ProjectIDKey), projectID)
	c.Set(string(auth.AccountIDKey), testAccountID)
	server.CreateInstance(c)
	return w
}

// THE opt-in guarantee: a normal request (no node_class) must NEVER consult
// the home placer. A bug that placed regular workloads on home nodes would
// be caught here.
func TestCreateInstance_DefaultNeverPlacesHome(t *testing.T) {
	placer := &fakePlacer{nodeName: "home-1", provider: "p1"}
	server := newTenantServer(t, newFakeCluster()).WithNodePlacer(placer)

	createInstanceReqBody(server, uuid.New(),
		`{"name":"t","image":"nginx","cpu_units":1,"memory":"1GB"}`)

	if placer.called {
		t.Fatal("home placer was called for a request that did not ask for node_class:home")
	}
}

// A home request resolves through the placer and the create reaches the
// cluster with the resolved provider/node on the spec. Uses a store-less
// server (standalone) so the assertion is purely about placement, not
// persistence — but the account/project are still injected so the gate and
// scope run. Standalone mode (store==nil) skips persistence, so the spec the
// cluster received is exactly what we assert.
func TestCreateInstance_HomePlacesAndCarriesProvider(t *testing.T) {
	placer := &fakePlacer{nodeName: "home-1", provider: "prov-home", arch: "amd64"}
	fc := newFakeCluster()
	// store=nil, gate=nil → standalone: no persistence, no gate; placement
	// still runs because the placer is set.
	server := NewServer(fc, nil, nil, nil, nil).WithNodePlacer(placer)

	w := createInstanceReqBody(server, uuid.New(),
		`{"name":"t","image":"nginx","cpu_units":2,"memory":"4GB","node_class":"home"}`)

	if !placer.called {
		t.Fatal("home request did not consult the placer")
	}
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if fc.lastSpec.ProviderID != "prov-home" || fc.lastSpec.NodeName != "home-1" || fc.lastSpec.NodeClass != "home" {
		t.Errorf("spec did not carry home placement: %+v", fc.lastSpec)
	}
}

// A home request with no placer configured (feature off) is refused cleanly.
func TestCreateInstance_HomeWithoutPlacerRefused(t *testing.T) {
	server := newTenantServer(t, newFakeCluster()) // no WithNodePlacer

	w := createInstanceReqBody(server, uuid.New(),
		`{"name":"t","image":"nginx","cpu_units":1,"memory":"1GB","node_class":"home"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (home compute not enabled)", w.Code)
	}
}

// No home capacity → 503 (retryable), like GPU exhaustion.
func TestCreateInstance_HomeNoCapacity(t *testing.T) {
	placer := &fakePlacer{err: errTest, noCap: true}
	server := newTenantServer(t, newFakeCluster()).WithNodePlacer(placer)

	w := createInstanceReqBody(server, uuid.New(),
		`{"name":"t","image":"nginx","cpu_units":1,"memory":"1GB","node_class":"home"}`)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// Arch mismatch → 400 (a request problem, distinct from capacity).
func TestCreateInstance_HomeArchMismatch(t *testing.T) {
	placer := &fakePlacer{err: errTest, archUnav: true}
	server := newTenantServer(t, newFakeCluster()).WithNodePlacer(placer)

	w := createInstanceReqBody(server, uuid.New(),
		`{"name":"t","image":"nginx","cpu_units":1,"memory":"1GB","node_class":"home","arch":"arm64"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// A tier too large for any node's free capacity → 503 (retryable), and the
// requested size is threaded through to the placer.
func TestCreateInstance_HomeInsufficientCapacity(t *testing.T) {
	placer := &fakePlacer{err: errTest, insuffCap: true}
	server := newTenantServer(t, newFakeCluster()).WithNodePlacer(placer)

	w := createInstanceReqBody(server, uuid.New(),
		`{"name":"t","image":"nginx","cpu_units":8,"memory":"16GB","node_class":"home"}`)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if placer.lastCPU != 8 || placer.lastMem != 16 {
		t.Errorf("placer got size %d vCPU / %d GB, want 8 / 16", placer.lastCPU, placer.lastMem)
	}
}

var errTest = context.Canceled // any non-nil error; classification is by the Is* flags

func TestRequireProjectScope_StandaloneModeAllowsAll(t *testing.T) {
	// Without a store (no database) there is no tenancy: requests
	// proceed unscoped, preserving the zero-dependency local dev flow.
	server := NewServer(newFakeCluster(), nil, nil, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	projectID, ok := server.requireProjectScope(c)
	if !ok || projectID != uuid.Nil {
		t.Errorf("standalone mode must allow unscoped access, got ok=%v id=%s", ok, projectID)
	}
	if strings.Contains(w.Body.String(), "error") {
		t.Errorf("no error should be written in standalone mode")
	}
}

func TestScopeFor_StandaloneIsUnrestricted(t *testing.T) {
	// uuid.Nil means standalone mode, where there is no tenancy to
	// enforce. Any other value must produce a restricted scope — a bug
	// here would silently disable tenant isolation everywhere.
	if scopeFor(uuid.Nil).IsRestricted() {
		t.Error("standalone scope must be unrestricted")
	}
	if !scopeFor(uuid.New()).IsRestricted() {
		t.Error("a project scope must be restricted")
	}
}

// TestCreateInstance_ResponsePopulatesEndpointFields is Stage 3 defect 1,
// closed on the response path: the create response must carry every
// endpoint field straight from cluster.CreateInstance's result — never
// from a separate networkingService.GetEndpointInfo call (removed
// entirely; see the comment above this block in server.go).
func TestCreateInstance_ResponsePopulatesEndpointFields(t *testing.T) {
	fc := newFakeCluster()
	fc.nextResult = &cluster.InstanceResult{
		EndpointURL: "https://inst-resp0001.teepin.com",
		PublicIP:    "203.0.113.7",
		DNSName:     "inst-resp0001.teepin.com",
		TLSEnabled:  true,
		TLSReady:    false,
	}
	// store=nil: exercises the response-building path independent of
	// persistence, which pkg/compute's own tests already cover.
	server := NewServer(fc, nil, nil, nil, nil)

	w := createInstanceReqBody(server, uuid.New(),
		`{"name":"t","image":"nginx","cpu_units":1,"memory":"2GB","ports":[{"container":8080}]}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", w.Code, w.Body.String())
	}

	var resp struct {
		Endpoint   string `json:"endpoint"`
		PublicIP   string `json:"public_ip"`
		DNSName    string `json:"dns_name"`
		TLSEnabled bool   `json:"tls_enabled"`
		TLSReady   bool   `json:"tls_ready"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	if resp.Endpoint != "https://inst-resp0001.teepin.com" {
		t.Errorf("endpoint = %q, want the result's EndpointURL", resp.Endpoint)
	}
	if resp.PublicIP != "203.0.113.7" {
		t.Errorf("public_ip = %q, want 203.0.113.7", resp.PublicIP)
	}
	if resp.DNSName != "inst-resp0001.teepin.com" {
		t.Errorf("dns_name = %q, want inst-resp0001.teepin.com", resp.DNSName)
	}
	if !resp.TLSEnabled {
		t.Error("tls_enabled = false, want true")
	}
	if resp.TLSReady {
		t.Error("tls_ready = true, want false (cert-manager has not issued yet)")
	}
}

// TestCreateInstance_PublicPortRejected covers Stage 3 A8: the platform
// has no per-instance public port (every instance is reached by hostname
// on 443 through the shared edge) — accepting a Public field the platform
// cannot honour would be worse than rejecting it outright.
func TestCreateInstance_PublicPortRejected(t *testing.T) {
	server := NewServer(newFakeCluster(), nil, nil, nil, nil)

	w := createInstanceReqBody(server, uuid.New(),
		`{"name":"t","image":"nginx","cpu_units":1,"memory":"2GB","ports":[{"container":8080,"public":8080}]}`)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a nonzero Public port", w.Code)
	}
}

// TestCreateInstance_ContainerPortOutOfRangeRejected: a container port
// outside 1-65535 is a client bug (0, negative, or >65535 cannot be a
// real port) and must be rejected up front rather than silently reaching
// the pod builder with a value it was never validated against.
func TestCreateInstance_ContainerPortOutOfRangeRejected(t *testing.T) {
	server := NewServer(newFakeCluster(), nil, nil, nil, nil)

	for _, body := range []string{
		`{"name":"t","image":"nginx","cpu_units":1,"memory":"2GB","ports":[{"container":0}]}`,
		`{"name":"t","image":"nginx","cpu_units":1,"memory":"2GB","ports":[{"container":70000}]}`,
	} {
		w := createInstanceReqBody(server, uuid.New(), body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body=%s: status = %d, want 400", body, w.Code)
		}
	}
}

// TestCreateInstance_ContainerPortBelow1024Allowed confirms the range
// check is a sanity bound, not a privilege restriction: a container is
// free to listen on well-known ports like 80 (nginx's default) — that is
// a container-namespace bind, not a host one, and carries none of a bare
// process's root requirement.
func TestCreateInstance_ContainerPortBelow1024Allowed(t *testing.T) {
	server := NewServer(newFakeCluster(), nil, nil, nil, nil)

	w := createInstanceReqBody(server, uuid.New(),
		`{"name":"t","image":"nginx","cpu_units":1,"memory":"2GB","ports":[{"container":80}]}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 for port 80 (body: %s)", w.Code, w.Body.String())
	}
}

// TestCreateInstance_ResponseIncludesContainerPort: the port the customer
// requested must be visible on the create response — before this fix it
// was persisted to compute.instances but never copied onto the
// customer-facing models.Instance, so a customer (and the console) had no
// way to confirm what port their instance was actually listening on.
func TestCreateInstance_ResponseIncludesContainerPort(t *testing.T) {
	server := NewServer(newFakeCluster(), nil, nil, nil, nil)

	w := createInstanceReqBody(server, uuid.New(),
		`{"name":"t","image":"nginx","cpu_units":1,"memory":"2GB","ports":[{"container":8080}]}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", w.Code, w.Body.String())
	}

	var resp struct {
		ContainerPort int `json:"container_port"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	if resp.ContainerPort != 8080 {
		t.Errorf("container_port = %d, want 8080", resp.ContainerPort)
	}
}

// TestCreateInstance_ProtocolPassedThrough covers the other half of A8:
// Protocol must reach the cluster spec, not be silently hardcoded to
// "tcp" regardless of what the customer requested.
func TestCreateInstance_ProtocolPassedThrough(t *testing.T) {
	fc := newFakeCluster()
	server := NewServer(fc, nil, nil, nil, nil)

	w := createInstanceReqBody(server, uuid.New(),
		`{"name":"t","image":"nginx","cpu_units":1,"memory":"2GB","ports":[{"container":53,"protocol":"udp"}]}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", w.Code, w.Body.String())
	}
	if len(fc.lastSpec.Ports) != 1 || fc.lastSpec.Ports[0].Protocol != "udp" {
		t.Errorf("spec.Ports = %+v, want protocol udp passed through", fc.lastSpec.Ports)
	}
}

// TestStatusToInstance_PopulatesAllEndpointFields is the pure-function
// version of the same guarantee: every endpoint field on the stored
// record must reach models.Instance. This is what GetInstance and
// ListInstances both funnel through, so one test covers both read paths.
func TestStatusToInstance_PopulatesAllEndpointFields(t *testing.T) {
	record := &compute.InstanceRecord{
		Name:       "my-app",
		Image:      "nginx:latest",
		Endpoint:   "https://inst-read0001.teepin.com",
		DNSName:    "inst-read0001.teepin.com",
		PublicIP:   "203.0.113.11",
		TLSEnabled: true,
		TLSReady:   true,
	}
	status := cluster.InstanceStatus{InstanceID: "inst-read0001", Status: compute.StatusRunning}

	instance := statusToInstance(status, record, 0, "")

	if instance.Endpoint != record.Endpoint {
		t.Errorf("Endpoint = %q, want %q", instance.Endpoint, record.Endpoint)
	}
	if instance.DNSName != record.DNSName {
		t.Errorf("DNSName = %q, want %q", instance.DNSName, record.DNSName)
	}
	if instance.PublicIP != record.PublicIP {
		t.Errorf("PublicIP = %q, want %q", instance.PublicIP, record.PublicIP)
	}
	if instance.TLSEnabled != record.TLSEnabled {
		t.Errorf("TLSEnabled = %v, want %v", instance.TLSEnabled, record.TLSEnabled)
	}
	if instance.TLSReady != record.TLSReady {
		t.Errorf("TLSReady = %v, want %v", instance.TLSReady, record.TLSReady)
	}
}

// TestStatusToInstance_NilRecordOmitsEndpointFields: a status with no
// backing record (persistence disabled, or a race with the reconciler)
// must not panic and must simply omit the endpoint fields rather than
// fabricate them.
func TestStatusToInstance_NilRecordOmitsEndpointFields(t *testing.T) {
	status := cluster.InstanceStatus{InstanceID: "inst-norecord1", Status: compute.StatusPending}

	instance := statusToInstance(status, nil, 0, "")

	if instance.Endpoint != "" || instance.DNSName != "" || instance.TLSEnabled || instance.TLSReady {
		t.Errorf("expected zero-value endpoint fields with a nil record, got %+v", instance)
	}
	if instance.ID != "inst-norecord1" || instance.Status != compute.StatusPending {
		t.Errorf("basic status fields lost with a nil record: %+v", instance)
	}
}

// TestStatusToInstance_DerivesEndpointWhenRecordIsEmpty is the regression
// test for a real bug found live (2026-08-21): the tunnel routes by
// hostname convention and never consults record.Endpoint, so an instance
// can be genuinely reachable at https://<id>.<domain> while its stored
// endpoint/dns_name/tls_ready are all NULL — confirmed against a real
// instance (inst-0f0bdb64) that returned null fields from the API despite
// curl succeeding against its derived hostname. Every already-created
// instance with NULL endpoint columns is fixed by this with no migration
// and no backfill.
func TestStatusToInstance_DerivesEndpointWhenRecordIsEmpty(t *testing.T) {
	record := &compute.InstanceRecord{
		ID:            "inst-0f0bdb64",
		Name:          "scrapper1",
		Image:         "nginx",
		ContainerPort: 80,
		// Endpoint/DNSName/TLSEnabled/TLSReady deliberately left at their
		// zero values — this is exactly the NULL-from-the-database state.
	}
	status := cluster.InstanceStatus{InstanceID: "inst-0f0bdb64", Status: compute.StatusRunning}

	instance := statusToInstance(status, record, 0, "dev.teepin.com")

	want := "https://inst-0f0bdb64.dev.teepin.com"
	if instance.Endpoint != want {
		t.Errorf("Endpoint = %q, want derived %q", instance.Endpoint, want)
	}
	if instance.DNSName != "inst-0f0bdb64.dev.teepin.com" {
		t.Errorf("DNSName = %q, want the derived hostname", instance.DNSName)
	}
	if !instance.TLSEnabled || !instance.TLSReady {
		t.Errorf("TLSEnabled/TLSReady = %v/%v, want both true (the ACM wildcard is always ready)", instance.TLSEnabled, instance.TLSReady)
	}
}

// TestStatusToInstance_NeverOverridesAPopulatedEndpoint guards the other
// half of the fix: a datacenter instance's real cert-manager-issued
// endpoint must never be replaced by the derived fallback, even when a
// domain is configured and a container port is set.
func TestStatusToInstance_NeverOverridesAPopulatedEndpoint(t *testing.T) {
	record := &compute.InstanceRecord{
		ID:            "inst-real0001",
		Endpoint:      "https://inst-real0001.teepin.com",
		DNSName:       "inst-real0001.teepin.com",
		ContainerPort: 8080,
		TLSEnabled:    true,
		TLSReady:      false, // cert-manager still issuing
	}
	status := cluster.InstanceStatus{InstanceID: "inst-real0001", Status: compute.StatusRunning}

	instance := statusToInstance(status, record, 0, "dev.teepin.com")

	if instance.Endpoint != record.Endpoint {
		t.Errorf("Endpoint = %q, want the stored value %q untouched", instance.Endpoint, record.Endpoint)
	}
	if instance.TLSReady {
		t.Error("TLSReady was flipped to true by the fallback despite a real stored value of false")
	}
}

// TestStatusToInstance_NoContainerPortNeverDerivesAnEndpoint: an instance
// with no exposed port genuinely has nothing to reach — deriving a
// hostname for it would be a broken link, not a helpful fallback.
func TestStatusToInstance_NoContainerPortNeverDerivesAnEndpoint(t *testing.T) {
	record := &compute.InstanceRecord{ID: "inst-noport001"}
	status := cluster.InstanceStatus{InstanceID: "inst-noport001", Status: compute.StatusRunning}

	instance := statusToInstance(status, record, 0, "dev.teepin.com")

	if instance.Endpoint != "" {
		t.Errorf("Endpoint = %q, want empty (no port was ever exposed)", instance.Endpoint)
	}
}

// TestCreateInstance_PersistsEndpointFields drives CreateInstance with a
// real *compute.Store over sqlmock, asserting the endpoint columns in the
// INSERT match what cluster.CreateInstance returned — the persistence
// half of defect 1, distinct from the response half tested above (a bug
// could populate the JSON response correctly while never writing the
// columns the reconciler and every SUBSEQUENT read depend on).
func TestCreateInstance_PersistsEndpointFields(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	fc := newFakeCluster()
	fc.nextResult = &cluster.InstanceResult{
		EndpointURL: "https://inst-persist01.teepin.com",
		PublicIP:    "203.0.113.20",
		DNSName:     "inst-persist01.teepin.com",
		TLSEnabled:  true,
		TLSReady:    false,
	}
	server := NewServer(fc, nil, compute.NewStore(db), nil, allowGate{})

	// The INSERT's exact column count/order is pkg/compute/store_test.go's
	// contract to maintain; here we only need to assert the endpoint-field
	// ARGS are the ones matching() would reject if wrong, via a custom
	// matcher rather than a full literal WithArgs list (which would
	// duplicate store_test.go and break every time an unrelated column is
	// added there).
	mock.ExpectQuery(`INSERT INTO compute\.instances`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), "203.0.113.20", true, false, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at"}).
			AddRow(time.Now(), time.Now()))

	w := createInstanceReqBody(server, uuid.New(),
		`{"name":"t","image":"nginx","cpu_units":1,"memory":"2GB","ports":[{"container":8080}]}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("INSERT did not carry the expected endpoint-field args: %v", err)
	}
}

// TestCreateInstance_CheckspointsKumbhaWorkspaceWhenSessionLinked is the
// regression test for a live 2026-08-31 incident: a Kumbha agent's real,
// successfully-built workspace never showed up in the console's History
// list because it was never checkpointed — CheckpointWorkspace only ever
// ran inside DeployKumbhaSession's own handler, so an instance created via
// the create_instance MCP tool (used as a workaround when `deploy` itself
// was erroring) left the customer's only saved draft permanently filtered
// out of ListVersions' is_checkpoint-only query, even though the file
// content was safe in Postgres the whole time. CreateInstance must now
// checkpoint the calling session's workspace whenever the request carries
// a Kumbha session credential (auth.SessionIDKey) — the same signal
// migration 032's instance/session linkage already introduced.
func TestCreateInstance_CheckspointsKumbhaWorkspaceWhenSessionLinked(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{in: 1, out: 1}, noopUsageRecorder{})
	server := NewServer(newFakeCluster(), nil, cStore, nil, allowGate{}).WithKumbha(gw)

	sessionID := uuid.New()

	mock.ExpectQuery(`INSERT INTO compute\.instances`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at"}).
			AddRow(time.Now(), time.Now()))
	mock.ExpectExec(`UPDATE billing\.kumbha_workspace_versions`).
		WithArgs(sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.inference_sessions\s+SET last_deployed_version = current_workspace_version`).
		WithArgs(sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"name":"t","image":"nginx","cpu_units":1,"memory":"2GB"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(auth.ProjectIDKey), uuid.New())
	c.Set(string(auth.AccountIDKey), testAccountID)
	c.Set(string(auth.SessionIDKey), sessionID)
	server.CreateInstance(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected the workspace checkpoint UPDATE to run: %v", err)
	}
}

// TestImagePorts_MissingImageParamIs400: the one piece of validation this
// handler owns directly (everything past that is pkg/imageinfo's own
// tested behaviour) — an empty/missing image query param is a client
// error, not an empty successful lookup.
func TestImagePorts_MissingImageParamIs400(t *testing.T) {
	server := NewServer(newFakeCluster(), nil, nil, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/compute/image-ports", nil)
	server.ImagePorts(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a missing image param", w.Code)
	}
}

// TestImagePorts_UnresolvableImageReturns200WithEmptyPorts: this endpoint
// is a convenience default, never a hard dependency — an image on a
// registry outside the allowlist (see pkg/imageinfo's SSRF guard) must
// degrade to 200 with an empty list, not an error the console would have
// to handle specially.
func TestImagePorts_UnresolvableImageReturns200WithEmptyPorts(t *testing.T) {
	server := NewServer(newFakeCluster(), nil, nil, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/compute/image-ports?image=internal.example.com/some-service:latest", nil)
	server.ImagePorts(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	var resp struct {
		Ports []struct {
			Port int `json:"port"`
		} `json:"ports"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	if len(resp.Ports) != 0 {
		t.Errorf("ports = %+v, want empty for a non-allowlisted registry", resp.Ports)
	}
}

// TestInstanceSpec_AutoAttachesKumbhaBuildPullSecretOnlyForItsOwnImages
// is the regression test for a real 2026-08-26 incident: a Kumbha
// deploy built and pushed its own image cleanly, then failed to create
// the resulting instance with "pull access denied ... no basic auth
// credentials" — nothing had ever wired a pull credential for it.
// Confirms the fix (instanceSpec auto-attaching
// kumbhaBuildImagePullSecret) is scoped strictly to images whose
// reference starts with the configured Kumbha build registry prefix —
// an ordinary customer image (any other registry) must never get it.
func TestInstanceSpec_AutoAttachesKumbhaBuildPullSecretOnlyForItsOwnImages(t *testing.T) {
	s := (&Server{}).WithKumbhaBuildImagePullSecret(
		"880254196251.dkr.ecr.us-east-1.amazonaws.com/teepin/kumbha-builds-dev",
		"teepin-kumbha-ecr",
	)

	kumbhaSpec := s.instanceSpec("inst-1", uuid.New(), uuid.New(), uuid.New(), &models.CreateInstanceRequest{
		Image: "880254196251.dkr.ecr.us-east-1.amazonaws.com/teepin/kumbha-builds-dev:4ab155f0",
	}, nil)
	if kumbhaSpec.ImagePullSecret != "teepin-kumbha-ecr" {
		t.Errorf("Kumbha-built image: ImagePullSecret = %q, want \"teepin-kumbha-ecr\"", kumbhaSpec.ImagePullSecret)
	}

	ordinarySpec := s.instanceSpec("inst-2", uuid.New(), uuid.New(), uuid.New(), &models.CreateInstanceRequest{
		Image: "nginx:latest",
	}, nil)
	if ordinarySpec.ImagePullSecret != "" {
		t.Errorf("ordinary customer image: ImagePullSecret = %q, want empty — must never borrow the Kumbha build secret", ordinarySpec.ImagePullSecret)
	}
}

func TestInstanceSpec_NoPullSecretWhenNotConfigured(t *testing.T) {
	s := &Server{} // WithKumbhaBuildImagePullSecret never called

	spec := s.instanceSpec("inst-1", uuid.New(), uuid.New(), uuid.New(), &models.CreateInstanceRequest{
		Image: "nginx:latest",
	}, nil)
	if spec.ImagePullSecret != "" {
		t.Errorf("ImagePullSecret = %q, want empty when the feature is not configured at all", spec.ImagePullSecret)
	}
}

// TestInstanceSpec_GrantsFilesystemOwnershipChangesToEveryCustomerInstance
// is the regression test for a real 2026-08-26 finding: nginx's own
// completely standard docker-entrypoint startup dance (chown to drop
// from root to its own less-privileged user) failed outright under the
// platform's "drop ALL capabilities" pod policy — the same failure class
// already fixed narrowly for Kaniko. Confirmed directly with the
// platform owner as the intended trade-off (not decided unilaterally)
// before broadening it here to every ordinary customer instance, since
// it weakens isolation for customer-controlled images, not just Teepin's
// own trusted build tooling.
func TestInstanceSpec_GrantsFilesystemOwnershipChangesToEveryCustomerInstance(t *testing.T) {
	s := &Server{}

	spec := s.instanceSpec("inst-1", uuid.New(), uuid.New(), uuid.New(), &models.CreateInstanceRequest{
		Image: "nginx:alpine",
	}, nil)

	if !spec.AllowFilesystemOwnershipChanges {
		t.Error("AllowFilesystemOwnershipChanges = false, want true for every customer instance — nginx's own startup chown would fail otherwise")
	}
}

// TestEndpointUUIDFor_DeterministicFromInstanceID is the property the
// whole Kumbha in-place-redeploy path depends on (see
// redeployKumbhaInstance in kumbha_handlers.go): a redeploy recomputes
// this value from nothing but the instance's already-known ID and must
// land on the EXACT same UUID the original create used, or
// UpdateInstance's endpoint provisioning would create a second,
// differently-named Service/Ingress instead of reusing the live one.
func TestEndpointUUIDFor_DeterministicFromInstanceID(t *testing.T) {
	a := endpointUUIDFor("inst-6fea56ce")
	b := endpointUUIDFor("inst-6fea56ce")
	if a != b {
		t.Errorf("endpointUUIDFor is not deterministic: %s != %s for the same instance ID", a, b)
	}
}

// TestEndpointUUIDFor_DistinctInstancesDoNotCollide guards the other half:
// two different instance IDs must not derive the same endpoint UUID,
// which would make their Service/Ingress objects collide.
func TestEndpointUUIDFor_DistinctInstancesDoNotCollide(t *testing.T) {
	a := endpointUUIDFor("inst-aaaaaaaa")
	b := endpointUUIDFor("inst-bbbbbbbb")
	if a == b {
		t.Error("two distinct instance IDs derived the same endpoint UUID")
	}
}
