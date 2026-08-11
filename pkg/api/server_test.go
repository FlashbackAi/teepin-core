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
}

type fakeInstance struct {
	projectID string
	status    string
	message   string
	logs      string
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
	if f.failWith != nil {
		return nil, f.failWith
	}
	f.instances[spec.InstanceID] = fakeInstance{
		projectID: spec.ProjectID,
		status:    compute.StatusPending,
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
		InstanceID: id,
		Status:     inst.status,
		Message:    inst.message,
		ObservedAt: time.Now().UTC(),
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

func (f *fakeCluster) Healthy(context.Context) bool { return f.failWith == nil }

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

	return NewServer(fc, nil, nil, compute.NewStore(db), nil, gate)
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

	inst := statusToInstance(st, record, gpu.DefaultPricePerGBHour)

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

func TestStatusToInstance_SurfacesFailureReason(t *testing.T) {
	st := cluster.InstanceStatus{
		InstanceID: "inst-badimage",
		Status:     compute.StatusFailed,
		Message:    "manifest unknown",
		ObservedAt: time.Now().UTC(),
	}

	inst := statusToInstance(st, nil, gpu.DefaultPricePerGBHour)

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

	inst := statusToInstance(st, nil, gpu.DefaultPricePerGBHour)

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

func TestRequireProjectScope_StandaloneModeAllowsAll(t *testing.T) {
	// Without a store (no database) there is no tenancy: requests
	// proceed unscoped, preserving the zero-dependency local dev flow.
	server := NewServer(newFakeCluster(), nil, nil, nil, nil, nil)

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
