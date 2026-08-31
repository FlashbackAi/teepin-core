// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
	"github.com/FlashbackAi/teepin-core/pkg/build"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
	"github.com/FlashbackAi/teepin-core/pkg/models"
)

// nowStub / sqlNoRows are tiny readability helpers so the sqlmock setup
// below reads as "a timestamp" / "not found" rather than bare package
// calls repeated at every call site.
func nowStub() time.Time { return time.Now() }
func sqlNoRows() error   { return sql.ErrNoRows }

// noopUsageRecorder satisfies kumbha.UsageRecorder for handler tests that
// never reach session close (settlement is covered at the pkg/kumbha
// level, not re-tested here).
type noopUsageRecorder struct{}

func (noopUsageRecorder) RecordUsage(context.Context, *billing.UsageRecord) error { return nil }
func (noopUsageRecorder) ConsumeCredit(context.Context, uuid.UUID, uuid.UUID, float64) (float64, error) {
	return 0, nil
}

// stubProvider is a minimal inference.Provider for exercising the HTTP
// layer's success path without touching a real backend.
type stubProvider struct {
	body  []byte
	usage inference.Usage
	err   error
}

func (s *stubProvider) Name() string { return "vllm" }
func (s *stubProvider) Complete(context.Context, inference.Request) (*inference.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &inference.Response{Usage: s.usage, Model: "qwen3-coder-30b", Body: s.body}, nil
}
func (s *stubProvider) Stream(context.Context, inference.Request, func(inference.Chunk) error) error {
	return nil
}
func (s *stubProvider) Capabilities() inference.Capabilities { return inference.Capabilities{} }

// kumbhaRequest performs a request against a Kumbha handler with the
// caller's project/account injected the way auth middleware would (same
// idiom as doRequest in server_test.go, extended with a body and headers
// since every Kumbha endpoint needs both).
func kumbhaRequest(handler gin.HandlerFunc, method, path string, params gin.Params, projectID uuid.UUID, body any, headers map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	var bodyReader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	c.Request = httptest.NewRequest(method, path, bodyReader)
	c.Request.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		c.Request.Header.Set(k, v)
	}
	c.Params = params
	if projectID != uuid.Nil {
		c.Set(string(auth.ProjectIDKey), projectID)
		c.Set(string(auth.AccountIDKey), testAccountID)
	}
	handler(c)
	return w
}

// newMockKumbhaDB returns a mocked DB plus stores built on it for both
// kumbha (what the test actually drives) and compute (needed only to make
// Server.store non-nil — see the comment on newTestServerWithKumbha). The
// compute store is never queried in these tests.
func newMockKumbhaDB(t *testing.T) (sqlmock.Sqlmock, *kumbha.Store, *compute.Store) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return mock, kumbha.NewStore(db), compute.NewStore(db)
}

type fakeKPricing struct{ in, out float64 }

func (f *fakeKPricing) LLMPriceInputPerMillion(context.Context) float64  { return f.in }
func (f *fakeKPricing) LLMPriceOutputPerMillion(context.Context) float64 { return f.out }

// newTestServerWithKumbha builds a Server with a non-nil store (so
// requireScope actually resolves tenancy from the gin context instead of
// short-circuiting into "standalone mode", which requireScope treats a
// nil store as meaning) plus a wired Kumbha Gateway.
func newTestServerWithKumbha(t *testing.T, gate kumbha.ProvisionGate, router *kumbha.Router) *Server {
	t.Helper()
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, router, gate, &fakeKPricing{in: 1, out: 1}, noopUsageRecorder{})
	return (&Server{store: cStore}).WithKumbha(gw)
}

func TestCreateKumbhaSession_NotAvailableWhenKumbhaNil(t *testing.T) {
	server := &Server{}
	w := kumbhaRequest(server.CreateKumbhaSession, http.MethodPost, "/v1/kumbha/sessions", nil,
		uuid.New(), createKumbhaSessionRequest{Budget: 5}, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestCreateKumbhaSession_RequiresProjectScope(t *testing.T) {
	server := newTestServerWithKumbha(t, allowGate{}, kumbha.NewRouter(nil))
	w := kumbhaRequest(server.CreateKumbhaSession, http.MethodPost, "/v1/kumbha/sessions", nil,
		uuid.Nil, createKumbhaSessionRequest{Budget: 5}, nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (no project scope)", w.Code)
	}
}

func TestCreateKumbhaSession_PaymentRequiredMapsTo402(t *testing.T) {
	server := newTestServerWithKumbha(t, denyGate{reason: "no card on file"}, kumbha.NewRouter(nil))
	w := kumbhaRequest(server.CreateKumbhaSession, http.MethodPost, "/v1/kumbha/sessions", nil,
		uuid.New(), createKumbhaSessionRequest{Budget: 5}, nil)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"] != "payment_method_required" {
		t.Errorf("code = %v, want payment_method_required", resp["code"])
	}
}

func TestCreateKumbhaSession_InvalidBudgetIs400(t *testing.T) {
	server := newTestServerWithKumbha(t, allowGate{}, kumbha.NewRouter(nil))
	w := kumbhaRequest(server.CreateKumbhaSession, http.MethodPost, "/v1/kumbha/sessions", nil,
		uuid.New(), createKumbhaSessionRequest{Budget: 0}, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a non-positive budget", w.Code)
	}
}

func TestCreateKumbhaSession_Success(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	projectID := uuid.New()
	sessionID := uuid.New()
	mock.ExpectQuery(`INSERT INTO billing\.inference_sessions`).
		WithArgs(testAccountID, projectID, 5.0, "test build").
		WillReturnRows(sqlmock.NewRows([]string{"id", "spent", "status", "started_at"}).
			AddRow(sessionID, 0.0, "open", nowStub()))

	w := kumbhaRequest(server.CreateKumbhaSession, http.MethodPost, "/v1/kumbha/sessions", nil,
		projectID, createKumbhaSessionRequest{Budget: 5, Label: "test build"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "open" {
		t.Errorf("status field = %v, want open", resp["status"])
	}
	if resp["agent_running"] != false {
		t.Errorf("agent_running = %v, want false (no prompt was sent)", resp["agent_running"])
	}
}

// A prompt is supplied but this test server was never given WithAgent
// (see newTestServerWithKumbha) — CreateKumbhaSession must not report 201
// success for a build that never actually started.
func TestCreateKumbhaSession_PromptWithoutAgentConfiguredIs503(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	projectID := uuid.New()
	sessionID := uuid.New()
	mock.ExpectQuery(`INSERT INTO billing\.inference_sessions`).
		WithArgs(testAccountID, projectID, 5.0, nil).
		WillReturnRows(sqlmock.NewRows([]string{"id", "spent", "status", "started_at"}).
			AddRow(sessionID, 0.0, "open", nowStub()))

	w := kumbhaRequest(server.CreateKumbhaSession, http.MethodPost, "/v1/kumbha/sessions", nil,
		projectID, createKumbhaSessionRequest{Budget: 5, Prompt: "build me a booking app"}, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"] != "agent_not_configured" {
		t.Errorf("code = %v, want agent_not_configured", resp["code"])
	}
}

func TestKumbhaChatCompletions_NotAvailableWhenKumbhaNil(t *testing.T) {
	server := &Server{}
	w := kumbhaRequest(server.KumbhaChatCompletions, http.MethodPost, "/v1/kumbha/chat/completions", nil,
		uuid.New(), map[string]any{"model": "teepin/fast"}, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestKumbhaChatCompletions_MissingSessionHeaderIs400(t *testing.T) {
	server := newTestServerWithKumbha(t, allowGate{}, kumbha.NewRouter(nil))
	w := kumbhaRequest(server.KumbhaChatCompletions, http.MethodPost, "/v1/kumbha/chat/completions", nil,
		uuid.New(), map[string]any{"model": "teepin/fast", "messages": []any{map[string]string{"role": "user", "content": "hi"}}}, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing X-Teepin-Session)", w.Code)
	}
}

func TestKumbhaChatCompletions_InvalidSessionHeaderIs400(t *testing.T) {
	server := newTestServerWithKumbha(t, allowGate{}, kumbha.NewRouter(nil))
	w := kumbhaRequest(server.KumbhaChatCompletions, http.MethodPost, "/v1/kumbha/chat/completions", nil,
		uuid.New(), map[string]any{"model": "teepin/fast", "messages": []any{map[string]string{"role": "user", "content": "hi"}}},
		map[string]string{"X-Teepin-Session": "not-a-uuid"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (malformed session id)", w.Code)
	}
}

func TestKumbhaChatCompletions_MissingModelIs400(t *testing.T) {
	server := newTestServerWithKumbha(t, allowGate{}, kumbha.NewRouter(nil))
	w := kumbhaRequest(server.KumbhaChatCompletions, http.MethodPost, "/v1/kumbha/chat/completions", nil,
		uuid.New(), map[string]any{"messages": []any{map[string]string{"role": "user", "content": "hi"}}},
		map[string]string{"X-Teepin-Session": uuid.New().String()})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing model)", w.Code)
	}
}

func TestKumbhaChatCompletions_EmptyMessagesIs400(t *testing.T) {
	server := newTestServerWithKumbha(t, allowGate{}, kumbha.NewRouter(nil))
	w := kumbhaRequest(server.KumbhaChatCompletions, http.MethodPost, "/v1/kumbha/chat/completions", nil,
		uuid.New(), map[string]any{"model": "teepin/fast", "messages": []any{}},
		map[string]string{"X-Teepin-Session": uuid.New().String()})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (empty messages)", w.Code)
	}
}

func TestKumbhaChatCompletions_StreamingRefusedExplicitly(t *testing.T) {
	server := newTestServerWithKumbha(t, allowGate{}, kumbha.NewRouter(nil))
	w := kumbhaRequest(server.KumbhaChatCompletions, http.MethodPost, "/v1/kumbha/chat/completions", nil,
		uuid.New(), map[string]any{"model": "teepin/fast", "stream": true,
			"messages": []any{map[string]string{"role": "user", "content": "hi"}}},
		map[string]string{"X-Teepin-Session": uuid.New().String()})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (streaming not yet available)", w.Code)
	}
}

func TestKumbhaChatCompletions_SessionNotFoundIs404(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{in: 1, out: 1}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID := uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnError(sqlNoRows())

	w := kumbhaRequest(server.KumbhaChatCompletions, http.MethodPost, "/v1/kumbha/chat/completions", nil,
		uuid.New(), map[string]any{"model": "teepin/fast", "messages": []any{map[string]string{"role": "user", "content": "hi"}}},
		map[string]string{"X-Teepin-Session": sessionID.String()})
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

func TestKumbhaChatCompletions_Success(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)

	provider := &stubProvider{body: []byte(`{"model":"qwen3-coder-30b","choices":[{"message":{"content":"hi"}}]}`),
		usage: inference.Usage{InputTokens: 100, OutputTokens: 20}}
	router := kumbha.NewRouter(map[string]kumbha.Route{"teepin/fast": {Provider: provider, ProviderName: "vllm"}})
	gw := kumbha.NewGateway(kStore, router, allowGate{}, &fakeKPricing{in: 2, out: 8}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID := uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
			"last_deploy_failed", "last_deploy_error", "last_deploy_at",
		}).AddRow(sessionID, testAccountID, uuid.New(), 5.0, 0.0, "open", nil, nil, nil, false, nowStub(), nil, false, nil, nil))

	wantCost := 100.0/1e6*2.0 + 20.0/1e6*8.0
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, budget, spent FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "budget", "spent"}).AddRow("open", 5.0, 0.0))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET spent`).
		WithArgs(sessionID, wantCost).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO billing\.inference_session_usage`).
		WithArgs(sessionID, "teepin/fast", "vllm", 100, 20).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := kumbhaRequest(server.KumbhaChatCompletions, http.MethodPost, "/v1/kumbha/chat/completions", nil,
		uuid.New(), map[string]any{"model": "teepin/fast", "messages": []any{map[string]string{"role": "user", "content": "hi"}}},
		map[string]string{"X-Teepin-Session": sessionID.String()})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Teepin-Cost") == "" {
		t.Error("X-Teepin-Cost header missing")
	}
	if w.Header().Get("X-Teepin-Session-Spent") == "" {
		t.Error("X-Teepin-Session-Spent header missing")
	}
	// The backend model name must never reach the response the customer
	// sees to originate from — the body is passed through verbatim (it IS
	// OpenAI-shaped already), but nothing about model/provider is added by
	// the gateway on top of it.
	if !bytes.Contains(w.Body.Bytes(), []byte("qwen3-coder-30b")) {
		t.Error("upstream body was not passed through verbatim")
	}
}

func TestParseCompletionRequest_PreservesUnknownFieldsAsExtra(t *testing.T) {
	body := []byte(`{"model":"teepin/fast","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"tools":[{"type":"function"}]}`)
	req, err := parseCompletionRequest(body)
	if err != nil {
		t.Fatalf("parseCompletionRequest: %v", err)
	}
	if req.Model != "teepin/fast" {
		t.Errorf("Model = %q", req.Model)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(req.Messages))
	}
	if _, ok := req.Extra["temperature"]; !ok {
		t.Error("temperature was dropped instead of preserved in Extra")
	}
	if _, ok := req.Extra["tools"]; !ok {
		t.Error("tools was dropped instead of preserved in Extra")
	}
	if _, ok := req.Extra["model"]; ok {
		t.Error("model leaked into Extra — must be excluded, it is a typed field")
	}
	if _, ok := req.Extra["messages"]; ok {
		t.Error("messages leaked into Extra — must be excluded, it is a typed field")
	}
}

func TestParseCompletionRequest_RejectsInvalidJSON(t *testing.T) {
	_, err := parseCompletionRequest([]byte(`not json`))
	if err == nil {
		t.Error("expected an error for invalid JSON")
	}
}

// --- detectKumbhaPorts ---

// newTestRegistry and pushTestImage mirror pkg/imageinfo's own test
// harness (unexported there, so not directly reusable) — an in-memory
// OCI registry real enough to exercise the actual remote-fetch code
// path, rather than mocking it away.
func newTestRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test registry URL: %v", err)
	}
	return u.Host
}

func pushTestImage(t *testing.T, registryHost, repo string, exposedPorts map[string]struct{}) {
	t.Helper()
	base, err := random.Image(64, 1)
	if err != nil {
		t.Fatalf("build base image: %v", err)
	}
	img, err := mutate.Config(base, v1.Config{ExposedPorts: exposedPorts})
	if err != nil {
		t.Fatalf("set image config: %v", err)
	}
	ref, err := name.ParseReference(registryHost + "/" + repo)
	if err != nil {
		t.Fatalf("parse push reference: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push test image: %v", err)
	}
}

// fakeRegistryProvider satisfies build.RegistryProvider against the
// in-memory test registry above — the test registry has no auth at all,
// so ImageAuth's returned credential is never actually checked, only
// threaded through the same path a real one (Harbor/ECR) would take.
type fakeRegistryProvider struct{ prefix string }

func (f *fakeRegistryProvider) ImagePrefix(context.Context, uuid.UUID, string) (string, error) {
	return f.prefix, nil
}
func (f *fakeRegistryProvider) DockerConfigJSONForBuild(context.Context, uuid.UUID) (string, error) {
	return "", nil
}
func (f *fakeRegistryProvider) ImageAuth(context.Context, uuid.UUID) (string, string, error) {
	return "user", "pass", nil
}

// TestDetectKumbhaPorts_ReadsExposedPortsFromTheBuiltImage is the
// regression test for a real 2026-08-26 finding: DeployKumbhaSession
// required the agent to separately remember and pass `ports` on every
// deploy, when the image it just built already declares this in its own
// manifest — directly asked "why should I tell it to expose a port, it
// should have that as part of the deployment, right?" This confirms the
// full chain end to end: detectKumbhaPorts -> Service.ImageAuth ->
// imageinfo.ResolvePortsWithAuth, against a real (in-memory) registry,
// not a mocked-away shortcut.
func TestDetectKumbhaPorts_ReadsExposedPortsFromTheBuiltImage(t *testing.T) {
	host := newTestRegistry(t)
	pushTestImage(t, host, "myapp:v1", map[string]struct{}{"80/tcp": {}, "443/tcp": {}})

	buildSvc := build.NewService(newFakeCluster(), &fakeRegistryProvider{}, build.DefaultConfig())
	s := &Server{kumbhaBuild: buildSvc}

	ports := s.detectKumbhaPorts(context.Background(), uuid.New(), host+"/myapp:v1")

	if len(ports) != 2 || ports[0].Container != 80 || ports[1].Container != 443 {
		t.Errorf("got %+v, want ports 80 and 443 detected from the image's own ExposedPorts", ports)
	}
}

func TestDetectKumbhaPorts_NoBuildServiceReturnsNil(t *testing.T) {
	s := &Server{}
	if ports := s.detectKumbhaPorts(context.Background(), uuid.New(), "nginx:alpine"); ports != nil {
		t.Errorf("got %+v, want nil when no build service is configured", ports)
	}
}

// --- BuildKumbhaSession ---

func newTestBuildService() *build.Service {
	// newFakeCluster (server_test.go) already implements cluster.Client in
	// full — reused here rather than a second fake, since pkg/build now
	// goes through the same transport-neutral interface every other
	// cluster-touching handler test in this package already exercises.
	return build.NewService(newFakeCluster(), nil, build.DefaultConfig())
}

func TestBuildKumbhaSession_NoBuildServiceIs404(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw) // no WithKumbhaBuild

	sessionID, projectID := uuid.New(), uuid.New()
	w := kumbhaRequest(server.BuildKumbhaSession, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/build",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{}, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

func TestBuildKumbhaSession_SessionNotFoundIs404(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw).WithKumbhaBuild(newTestBuildService())

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnError(sqlNoRows())

	w := kumbhaRequest(server.BuildKumbhaSession, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/build",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{}, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

// --- DeployKumbhaSession ---
//
// DeployKumbhaSession shares buildKumbhaImage with BuildKumbhaSession, so
// its guard-path tests mirror BuildKumbhaSession's own (not-configured/
// not-found/not-approved/no-workspace) — what's new and worth testing
// SEPARATELY is invokeInternally, the mechanism that lets this handler
// reuse CreateInstance/DeleteInstance without a second implementation of
// GPU/home placement, the payment gate, or billing (see its own doc
// comment). The full build-then-create success path needs a real Kaniko
// pod transitioning to Succeeded plus a working Harbor client — the same
// live-infra boundary pkg/build's own tests already stop at (they test
// buildPod/waitForCompletion in isolation, never Build() end-to-end) —
// so it is not re-attempted here.

func TestDeployKumbhaSession_NoBuildServiceIs404(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw) // no WithKumbhaBuild

	sessionID, projectID := uuid.New(), uuid.New()
	w := kumbhaRequest(server.DeployKumbhaSession, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/deploy",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{}, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

func TestDeployKumbhaSession_SessionNotFoundIs404(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw).WithKumbhaBuild(newTestBuildService())

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnError(sqlNoRows())

	w := kumbhaRequest(server.DeployKumbhaSession, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/deploy",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{}, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

func TestDeployKumbhaSession_NotApprovedIs403(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw).WithKumbhaBuild(newTestBuildService())

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sessionRow(sessionID)) // deploy_approved=false

	w := kumbhaRequest(server.DeployKumbhaSession, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/deploy",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{}, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
}

func TestDeployKumbhaSession_NoWorkspaceIs409(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw).WithKumbhaBuild(newTestBuildService())

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(deployApprovedSessionRow(sessionID))
	mock.ExpectQuery(`SELECT current_workspace_version FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{"current_workspace_version"}).AddRow(nil))

	w := kumbhaRequest(server.DeployKumbhaSession, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/deploy",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{}, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", w.Code, w.Body.String())
	}
}

// --- redeployKumbhaInstance ---
//
// Exercised directly (below buildKumbhaImage/DeployKumbhaSession itself)
// for the same reason invokeInternally is tested separately from
// CreateInstance: the full build-then-deploy path needs a real Kaniko pod
// and Harbor client, which this package's tests deliberately stop short
// of (see the comment above the "--- DeployKumbhaSession ---" section).
// What's genuinely new here — reading the existing instance's sizing,
// calling UpdateInstance instead of Create+Delete, reporting the
// UNCHANGED endpoint back — has no such live-infra dependency, so it is
// tested on its own.

// newRedeployTestContext builds a *gin.Context carrying the tenancy this
// package's auth middleware would normally set from a verified JWT —
// redeployKumbhaInstance is called directly (it is not itself a
// gin.HandlerFunc; DeployKumbhaSession supplies these arguments after its
// own request parsing), so there is no request body to route through
// kumbhaRequest here.
func newRedeployTestContext(projectID uuid.UUID) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Set(string(auth.ProjectIDKey), projectID)
	c.Set(string(auth.AccountIDKey), testAccountID)
	return c, w
}

// instanceRecordRow builds a compute.instances SELECT result matching
// pkg/compute's own selectColumns — column order copied from there
// verbatim so a drift between the two would be caught by a real scan
// failure, not silently mismatch.
func instanceRecordRow(id string, accountID, projectID uuid.UUID, name, image string, cpuUnits, memoryGB, storageGB, containerPort int, endpoint string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_id", "project_id", "user_id", "name", "image",
		"instance_type_id", "status", "gpu_vram_gb", "cpu_units", "memory_gb", "endpoint",
		"k8s_pod_name", "k8s_namespace", "provider_id", "dns_name", "public_ip",
		"tls_enabled", "tls_ready", "container_port", "storage_gb",
		"created_at", "updated_at", "started_at", "terminated_at", "kumbha_session_id",
	}).AddRow(id, accountID, projectID, uuid.Nil, name, image,
		"", compute.StatusRunning, 0, cpuUnits, memoryGB, endpoint,
		id+"-pod", "default", "", id+".teepin.com", "",
		true, true, containerPort, storageGB,
		nowStub(), nowStub(), nil, nil, uuid.Nil)
}

func TestRedeployKumbhaInstance_UpdatesExistingInstanceInPlace(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})

	projectID, sessionID := uuid.New(), uuid.New()
	existingID := "inst-existing1"
	fc := newFakeCluster()
	fc.add(existingID, projectID.String(), compute.StatusRunning)
	server := (&Server{store: cStore, cluster: fc}).WithKumbha(gw)

	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WithArgs(existingID).
		WillReturnRows(instanceRecordRow(existingID, testAccountID, projectID, "kumbha-abc123", "old-image:v1", 1, 1, 0, 80, "https://inst-existing1.teepin.com"))
	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs("new-image:v2", existingID+"-pod", 80, compute.StatusPending, existingID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.kumbha_workspace_versions`).
		WithArgs(sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.inference_sessions\s+SET last_deployed_version = current_workspace_version`).
		WithArgs(sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	sess := &kumbha.Session{ID: sessionID, AccountID: testAccountID, ProjectID: projectID, AppInstanceID: existingID}
	c, w := newRedeployTestContext(projectID)
	server.redeployKumbhaInstance(context.Background(), c, sessionID, sess, projectID, testAccountID, "new-image:v2",
		[]models.PortMapping{{Container: 80, Protocol: "tcp"}}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["instance_id"] != existingID {
		t.Errorf("instance_id = %v, want the SAME id the session already had (%s) — a redeploy must not mint a new one", resp["instance_id"], existingID)
	}
	// The whole point: the endpoint the customer already has bookmarked
	// must be reported back unchanged, not derived from `result` (which,
	// being a pod-only replace, never re-provisions or re-reports it).
	if resp["endpoint"] != "https://inst-existing1.teepin.com" {
		t.Errorf("endpoint = %v, want the UNCHANGED original endpoint", resp["endpoint"])
	}
	if fc.lastSpec.Image != "new-image:v2" {
		t.Errorf("UpdateInstance was called with Image = %q, want new-image:v2", fc.lastSpec.Image)
	}
	// Sizing must come from the EXISTING record, not be re-derived or
	// left at zero — a redeploy does not resize the instance.
	if fc.lastSpec.CPUUnits != 1 || fc.lastSpec.MemoryGB != 1 {
		t.Errorf("UpdateInstance spec sizing = %d CPU / %d GB, want the existing instance's own sizing (1/1)", fc.lastSpec.CPUUnits, fc.lastSpec.MemoryGB)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestRedeployKumbhaInstance_EmptyPortsFallsBackToExistingContainerPort
// guards the live incident from 2026-08-31: detectKumbhaPorts is
// best-effort and DeployKumbhaSession re-runs it on every redeploy, not
// just the first — a transient failure (registry auth hiccup, image not
// yet fully propagated) silently returns nil ports. Passed straight
// through to cluster.UpdateInstance, empty Ports skips endpoint synthesis
// on the home-class path (agent.go's createOrReplace) and seeds the
// status cache with an all-empty endpoint, which the reconciler then
// persists over the instance's previously-working endpoint — this is
// exactly what happened to inst-5ed29952, silently breaking the
// screenshot service. A redeploy must fall back to the instance's own
// already-known container port when detection comes back empty, rather
// than ever asking the cluster to redeploy with no ports at all.
func TestRedeployKumbhaInstance_EmptyPortsFallsBackToExistingContainerPort(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})

	projectID, sessionID := uuid.New(), uuid.New()
	existingID := "inst-existing2"
	fc := newFakeCluster()
	fc.add(existingID, projectID.String(), compute.StatusRunning)
	server := (&Server{store: cStore, cluster: fc}).WithKumbha(gw)

	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WithArgs(existingID).
		WillReturnRows(instanceRecordRow(existingID, testAccountID, projectID, "kumbha-abc123", "old-image:v1", 1, 1, 0, 8080, "https://inst-existing2.teepin.com"))
	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs("new-image:v3", existingID+"-pod", 8080, compute.StatusPending, existingID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.kumbha_workspace_versions`).
		WithArgs(sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.inference_sessions\s+SET last_deployed_version = current_workspace_version`).
		WithArgs(sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	sess := &kumbha.Session{ID: sessionID, AccountID: testAccountID, ProjectID: projectID, AppInstanceID: existingID}
	c, w := newRedeployTestContext(projectID)
	// ports == nil: simulates detectKumbhaPorts coming back empty.
	server.redeployKumbhaInstance(context.Background(), c, sessionID, sess, projectID, testAccountID, "new-image:v3", nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if len(fc.lastSpec.Ports) != 1 || fc.lastSpec.Ports[0].Container != 8080 {
		t.Errorf("UpdateInstance spec Ports = %+v, want a fallback to the existing container port (8080) instead of empty", fc.lastSpec.Ports)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestRedeployKumbhaInstance_NoResolvablePortIs422 guards the deeper,
// permanent fix for the same 2026-08-31 incident: the fallback to
// existing.ContainerPort only helps when the instance already has a known
// port. inst-5ed29952 itself did not (its compute.instances row had been
// reconstructed by hand earlier in the same incident, after its code went
// missing, with no port to record) — for exactly that case, a redeploy
// must refuse with a clear, actionable error instead of silently
// proceeding with no endpoint again. This is what actually closes the
// loop: the same text reaches the Kumbha agent verbatim as its own
// "deploy" MCP tool's error result, so it can add an EXPOSE line to the
// Dockerfile and retry, rather than the deploy silently "succeeding" into
// an unreachable instance with nothing anywhere to explain why.
func TestRedeployKumbhaInstance_NoResolvablePortIs422(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})

	projectID, sessionID := uuid.New(), uuid.New()
	existingID := "inst-existing3"
	fc := newFakeCluster()
	fc.add(existingID, projectID.String(), compute.StatusRunning)
	server := (&Server{store: cStore, cluster: fc}).WithKumbha(gw)

	// container_port = 0: no known-good port to fall back to, matching
	// inst-5ed29952's own hand-reconstructed record.
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WithArgs(existingID).
		WillReturnRows(instanceRecordRow(existingID, testAccountID, projectID, "kumbha-abc123", "old-image:v1", 1, 1, 0, 0, "https://inst-existing3.teepin.com"))

	sess := &kumbha.Session{ID: sessionID, AccountID: testAccountID, ProjectID: projectID, AppInstanceID: existingID}
	c, w := newRedeployTestContext(projectID)
	// ports == nil: simulates detectKumbhaPorts coming back empty too.
	server.redeployKumbhaInstance(context.Background(), c, sessionID, sess, projectID, testAccountID, "new-image:v4", nil, nil)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if errMsg, _ := resp["error"].(string); !strings.Contains(errMsg, "EXPOSE") {
		t.Errorf("error message = %q, want actionable guidance mentioning EXPOSE", errMsg)
	}
	// The cluster must never be asked to redeploy with no ports at all.
	if fc.lastSpec.InstanceID != "" {
		t.Errorf("UpdateInstance was called (spec: %+v), want no cluster call when no port could be resolved", fc.lastSpec)
	}
}

// TestRedeployKumbhaInstance_FallsBackToFreshEndpointWhenExistingIsEmpty
// guards the live incident from 2026-08-31, the last piece of it: even
// after container_port and terminated_at were both fixed and the
// instance was genuinely reachable again, this response — and, via the
// exact same value, triggerScreenshotCapture's targetURL — still
// reported an empty endpoint. existing.Endpoint is read from the
// database BEFORE cluster.UpdateInstance runs, so on the FIRST redeploy
// after recovering from a corrupted (empty) endpoint, it cannot possibly
// reflect what THIS SAME call's own createOrReplace just freshly
// synthesized — but result.EndpointURL, returned by that same call,
// already has it. A redeploy must fall back to that fresh value rather
// than reporting a stale empty one it could see was wrong.
func TestRedeployKumbhaInstance_FallsBackToFreshEndpointWhenExistingIsEmpty(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})

	projectID, sessionID := uuid.New(), uuid.New()
	existingID := "inst-existing4"
	fc := newFakeCluster()
	fc.add(existingID, projectID.String(), compute.StatusRunning)
	fc.nextResult = &cluster.InstanceResult{EndpointURL: "https://inst-existing4.dev.teepin.com"}
	server := (&Server{store: cStore, cluster: fc}).WithKumbha(gw)

	// endpoint == "" in the stored record: the corrupted-state case.
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WithArgs(existingID).
		WillReturnRows(instanceRecordRow(existingID, testAccountID, projectID, "kumbha-abc123", "old-image:v1", 1, 1, 0, 80, ""))
	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs("new-image:v5", existingID+"-pod", 80, compute.StatusPending, existingID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.kumbha_workspace_versions`).
		WithArgs(sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.inference_sessions\s+SET last_deployed_version = current_workspace_version`).
		WithArgs(sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	sess := &kumbha.Session{ID: sessionID, AccountID: testAccountID, ProjectID: projectID, AppInstanceID: existingID}
	c, w := newRedeployTestContext(projectID)
	server.redeployKumbhaInstance(context.Background(), c, sessionID, sess, projectID, testAccountID, "new-image:v5",
		[]models.PortMapping{{Container: 80, Protocol: "tcp"}}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["endpoint"] != "https://inst-existing4.dev.teepin.com" {
		t.Errorf("endpoint = %v, want the FRESH endpoint this same redeploy just synthesized, not a stale empty value", resp["endpoint"])
	}
}

func TestRedeployKumbhaInstance_InstanceGoneIs404(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore, cluster: newFakeCluster()}).WithKumbha(gw)

	projectID, sessionID := uuid.New(), uuid.New()
	existingID := "inst-deleted-by-hand"
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WithArgs(existingID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "user_id", "name", "image",
			"instance_type_id", "status", "gpu_vram_gb", "cpu_units", "memory_gb", "endpoint",
			"k8s_pod_name", "k8s_namespace", "provider_id", "dns_name", "public_ip",
			"tls_enabled", "tls_ready", "container_port", "storage_gb",
			"created_at", "updated_at", "started_at", "terminated_at",
		})) // no rows

	sess := &kumbha.Session{ID: sessionID, AccountID: testAccountID, ProjectID: projectID, AppInstanceID: existingID}
	c, w := newRedeployTestContext(projectID)
	server.redeployKumbhaInstance(context.Background(), c, sessionID, sess, projectID, testAccountID, "new-image:v2", nil, nil)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

// TestRedeployKumbhaInstance_WrongAccountIsNotFound is the tenancy
// regression test: even if some future bug let a session's AppInstanceID
// point at another tenant's instance ID, redeployKumbhaInstance must
// treat it as absent rather than updating (or leaking the existence of)
// an instance that is not this account's — the same "another tenant's
// instance is indistinguishable from a nonexistent one" rule GetInstance/
// DeleteInstance already enforce elsewhere in this file.
func TestRedeployKumbhaInstance_WrongAccountIsNotFound(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore, cluster: newFakeCluster()}).WithKumbha(gw)

	projectID, sessionID := uuid.New(), uuid.New()
	existingID := "inst-someone-elses"
	otherAccount := uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WithArgs(existingID).
		WillReturnRows(instanceRecordRow(existingID, otherAccount, projectID, "not-mine", "img:v1", 1, 1, 0, 80, "https://inst-someone-elses.teepin.com"))

	sess := &kumbha.Session{ID: sessionID, AccountID: testAccountID, ProjectID: projectID, AppInstanceID: existingID}
	c, w := newRedeployTestContext(projectID)
	server.redeployKumbhaInstance(context.Background(), c, sessionID, sess, projectID, testAccountID, "new-image:v2", nil, nil)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

func TestRedeployKumbhaInstance_ClusterUnavailableMapsTo503(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})

	projectID, sessionID := uuid.New(), uuid.New()
	existingID := "inst-existing2"
	fc := newFakeCluster()
	fc.failWith = cluster.ErrClusterUnavailable
	server := (&Server{store: cStore, cluster: fc}).WithKumbha(gw)

	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WithArgs(existingID).
		WillReturnRows(instanceRecordRow(existingID, testAccountID, projectID, "kumbha-def456", "old-image:v1", 1, 1, 0, 80, "https://inst-existing2.teepin.com"))

	sess := &kumbha.Session{ID: sessionID, AccountID: testAccountID, ProjectID: projectID, AppInstanceID: existingID}
	c, w := newRedeployTestContext(projectID)
	server.redeployKumbhaInstance(context.Background(), c, sessionID, sess, projectID, testAccountID, "new-image:v2", nil, nil)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", w.Code, w.Body.String())
	}
}

// echoIdentityHandler is a minimal gin.HandlerFunc that reports back
// exactly what invokeInternally put into the synthetic context — used to
// verify the dispatch mechanism itself (identity, params, body) rather
// than any real business logic.
func echoIdentityHandler(c *gin.Context) {
	accountID, _ := auth.GetAccountID(c)
	projectID, _ := auth.GetProjectID(c)
	userID, _ := auth.GetUserID(c)
	sessionID, sessionIDPresent := auth.GetSessionID(c)
	var body map[string]any
	_ = c.ShouldBindJSON(&body)
	c.JSON(http.StatusTeapot, gin.H{
		"account_id":         accountID.String(),
		"project_id":         projectID.String(),
		"user_id":            userID.String(),
		"session_id":         sessionID.String(),
		"session_id_present": sessionIDPresent,
		"param_id":           c.Param("id"),
		"body":               body,
	})
}

func TestInvokeInternally_CarriesIdentityParamsAndBodyThrough(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	accountID, projectID, userID := uuid.New(), uuid.New(), uuid.New()
	status, body := server.invokeInternally(echoIdentityHandler, http.MethodPost, "/v1/internal/echo/abc123",
		gin.Params{{Key: "id", Value: "abc123"}}, accountID, projectID, userID, uuid.Nil, map[string]string{"k": "v"})

	if status != http.StatusTeapot {
		t.Fatalf("status = %d, want %d (body: %s)", status, http.StatusTeapot, body)
	}
	var resp struct {
		AccountID string         `json:"account_id"`
		ProjectID string         `json:"project_id"`
		UserID    string         `json:"user_id"`
		ParamID   string         `json:"param_id"`
		Body      map[string]any `json:"body"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.AccountID != accountID.String() {
		t.Errorf("account_id = %q, want %q", resp.AccountID, accountID.String())
	}
	if resp.ProjectID != projectID.String() {
		t.Errorf("project_id = %q, want %q", resp.ProjectID, projectID.String())
	}
	if resp.UserID != userID.String() {
		t.Errorf("user_id = %q, want %q", resp.UserID, userID.String())
	}
	if resp.ParamID != "abc123" {
		t.Errorf("param_id = %q, want abc123", resp.ParamID)
	}
	if resp.Body["k"] != "v" {
		t.Errorf("body = %v, want {k: v} to have round-tripped", resp.Body)
	}
}

func TestInvokeInternally_OmitsUserIDFromContextWhenNil(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	_, body := server.invokeInternally(echoIdentityHandler, http.MethodPost, "/v1/internal/echo",
		nil, uuid.New(), uuid.New(), uuid.Nil, uuid.Nil, nil)

	var resp struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.UserID != uuid.Nil.String() {
		t.Errorf("user_id = %q, want the zero UUID (no attribution) when userID is uuid.Nil", resp.UserID)
	}
}

// TestInvokeInternally_CarriesKumbhaSessionIDThrough proves DeployKumbhaSession's
// internal CreateInstance call tags the resulting instance with the
// session it came from, via the exact same auth.SessionIDKey mechanism a
// real Kumbha session token uses on the create_instance MCP path — see
// migration 032's own doc comment for the live incident (two untracked
// instances from one build) this closes.
func TestInvokeInternally_CarriesKumbhaSessionIDThrough(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID := uuid.New()
	_, body := server.invokeInternally(echoIdentityHandler, http.MethodPost, "/v1/internal/echo",
		nil, uuid.New(), uuid.New(), uuid.Nil, sessionID, nil)

	var resp struct {
		SessionID        string `json:"session_id"`
		SessionIDPresent bool   `json:"session_id_present"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.SessionIDPresent || resp.SessionID != sessionID.String() {
		t.Errorf("session_id = (%q, present=%v), want (%q, true)", resp.SessionID, resp.SessionIDPresent, sessionID.String())
	}
}

func TestBuildKumbhaSession_NotApprovedIs403(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw).WithKumbhaBuild(newTestBuildService())

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sessionRow(sessionID)) // deploy_approved=false by default

	w := kumbhaRequest(server.BuildKumbhaSession, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/build",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{}, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
}

// deployApprovedSessionRow is sessionRow with deploy_approved flipped —
// TestBuildKumbhaSession_NoWorkspaceIs409 and the agent-not-configured
// test both need a session past the approval gate.
func deployApprovedSessionRow(sessionID uuid.UUID) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_id", "project_id", "budget", "spent", "status", "label",
		"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
		"last_deploy_failed", "last_deploy_error", "last_deploy_at",
	}).AddRow(sessionID, testAccountID, uuid.New(), 5.0, 0.0, "open", nil, nil, nil, true, nowStub(), nil, false, nil, nil)
}

func TestBuildKumbhaSession_NoWorkspaceIs409(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw).WithKumbhaBuild(newTestBuildService())

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(deployApprovedSessionRow(sessionID))
	mock.ExpectQuery(`SELECT current_workspace_version FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{"current_workspace_version"}).AddRow(nil))

	w := kumbhaRequest(server.BuildKumbhaSession, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/build",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{}, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", w.Code, w.Body.String())
	}
}

// TestBuildKumbhaSession_FailurePersistsLastDeployStatus is the
// regression test for the actual point of migration 030: a build failure
// must be PERSISTED on the session (last_deploy_failed/last_deploy_error),
// not just returned once in the HTTP response — what backs the "Failed"
// status on the console's "Previous builds" list, which reads back a
// session it did not itself trigger the build from.
func TestBuildKumbhaSession_FailurePersistsLastDeployStatus(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw).WithKumbhaBuild(newTestBuildService())

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(deployApprovedSessionRow(sessionID))
	mock.ExpectQuery(`SELECT current_workspace_version FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{"current_workspace_version"}).AddRow(nil))
	mock.ExpectExec(`UPDATE billing\.inference_sessions\s+SET last_deploy_failed`).
		WithArgs(sessionID, true, "nothing has been saved for this build yet").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := kumbhaRequest(server.BuildKumbhaSession, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/build",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{}, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// --- SendKumbhaMessage / PollKumbhaMessages ---

func fakeMintKumbhaToken(_, _, _ uuid.UUID, _ time.Duration) (string, error) {
	return "fake-agent-token", nil
}

func TestSendKumbhaMessage_NotAvailableWhenKumbhaNil(t *testing.T) {
	server := &Server{}
	sessionID, projectID := uuid.New(), uuid.New()
	w := kumbhaRequest(server.SendKumbhaMessage, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/messages",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{"content": "hi"}, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

func TestSendKumbhaMessage_SessionNotFoundIs404(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnError(sqlNoRows())

	w := kumbhaRequest(server.SendKumbhaMessage, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/messages",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{"content": "hi"}, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

// TestSendKumbhaMessage_ClosedSessionStillRelaunches is the regression
// test for the actual point of removing the old "Close session" button
// (2026-08-30): a session's stored status column no longer gates whether
// a customer can keep chatting at all. A closed session with no live
// agent pod relaunches exactly like an open one whose pod happened to
// exit on its own idle timeout — same DeliverMessage relaunch path,
// same "the workspace already exists" prompt.
func TestSendKumbhaMessage_ClosedSessionStillRelaunches(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	fc := newFakeCluster()
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{}).
		WithAgent(fc, fakeMintKumbhaToken, kumbha.AgentConfig{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
			"last_deploy_failed", "last_deploy_error", "last_deploy_at",
		}).AddRow(sessionID, testAccountID, projectID, 5.0, 0.0, "closed", nil, nil, nil, false, nowStub(), nowStub(), false, nil, nil))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET agent_instance_id`).
		WithArgs(sessionID, "kumbha-agent-"+sessionID.String()[:8]).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := kumbhaRequest(server.SendKumbhaMessage, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/messages",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{"content": "add a footer"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Delivered  bool `json:"delivered"`
		Relaunched bool `json:"relaunched"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Delivered || !resp.Relaunched {
		t.Errorf("got %+v, want delivered=true relaunched=true", resp)
	}
	if _, ok := fc.instances["kumbha-agent-"+sessionID.String()[:8]]; !ok {
		t.Error("relaunch did not create a fresh agent pod")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The common case: an already-running agent pod, so the message is just
// queued for its own poll loop — mirrors kumbha's own
// TestGateway_DeliverMessage_QueuesWhenAgentRunning, exercised here
// through the actual HTTP handler.
func TestSendKumbhaMessage_QueuesWhenAgentRunning(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	fc := newFakeCluster()
	sessionID, projectID := uuid.New(), uuid.New()
	fc.add("kumbha-agent-abc", projectID.String(), "running")

	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{}).
		WithAgent(fc, fakeMintKumbhaToken, kumbha.AgentConfig{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
			"last_deploy_failed", "last_deploy_error", "last_deploy_at",
		}).AddRow(sessionID, testAccountID, projectID, 5.0, 0.0, "open", nil, "kumbha-agent-abc", nil, false, nowStub(), nil, false, nil, nil))
	mock.ExpectQuery(`INSERT INTO billing\.kumbha_messages`).
		WithArgs(sessionID, "add a footer").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), nowStub()))

	w := kumbhaRequest(server.SendKumbhaMessage, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/messages",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{"content": "add a footer"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Delivered  bool `json:"delivered"`
		Relaunched bool `json:"relaunched"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Delivered || resp.Relaunched {
		t.Errorf("got %+v, want delivered=true relaunched=false", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSendKumbhaMessage_EmptyContentIs400(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	fc := newFakeCluster()
	sessionID, projectID := uuid.New(), uuid.New()
	fc.add("kumbha-agent-abc", projectID.String(), "running")

	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{}).
		WithAgent(fc, fakeMintKumbhaToken, kumbha.AgentConfig{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
			"last_deploy_failed", "last_deploy_error", "last_deploy_at",
		}).AddRow(sessionID, testAccountID, projectID, 5.0, 0.0, "open", nil, "kumbha-agent-abc", nil, false, nowStub(), nil, false, nil, nil))

	w := kumbhaRequest(server.SendKumbhaMessage, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/messages",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{"content": ""}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
}

func TestPollKumbhaMessages_RequiresSessionCredential(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID := uuid.New()
	w := kumbhaRequest(server.PollKumbhaMessages, http.MethodGet, "/v1/kumbha/sessions/"+sessionID.String()+"/messages/poll",
		gin.Params{{Key: "id", Value: sessionID.String()}}, uuid.Nil, nil, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
}

func TestPollKumbhaMessages_RejectsMismatchedSession(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	credentialSession, pathSession := uuid.New(), uuid.New()
	w := agentRequest(server.PollKumbhaMessages, http.MethodGet, "/v1/kumbha/sessions/"+pathSession.String()+"/messages/poll",
		gin.Params{{Key: "id", Value: pathSession.String()}}, credentialSession, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
}

func TestPollKumbhaMessages_Success(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, content, created_at FROM billing\.kumbha_messages`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "created_at"}).
			AddRow(int64(1), "add a footer", nowStub()))
	mock.ExpectExec(`UPDATE billing\.kumbha_messages SET delivered_at = NOW\(\)`).
		WithArgs(sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := agentRequest(server.PollKumbhaMessages, http.MethodGet, "/v1/kumbha/sessions/"+sessionID.String()+"/messages/poll",
		gin.Params{{Key: "id", Value: sessionID.String()}}, sessionID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Messages) != 1 || resp.Messages[0].Content != "add a footer" {
		t.Errorf("got %+v, want one message \"add a footer\"", resp.Messages)
	}
}

// A session with a saved workspace and an approved plan, but the Gateway
// never got WithAgent (no APIBaseURL/token minter configured) — the build
// pipeline itself is present (kumbhaBuild != nil), but nothing can mint the
// fetch-workspace initContainer its credential, so this must degrade to
// the same honest "not available" 404 as no build service at all, not a
// 500.
func TestBuildKumbhaSession_AgentNotConfiguredIs404(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw).WithKumbhaBuild(newTestBuildService())

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(deployApprovedSessionRow(sessionID))
	mock.ExpectQuery(`SELECT current_workspace_version FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{"current_workspace_version"}).AddRow(1))
	mock.ExpectQuery(`FROM billing\.kumbha_workspace_versions v`).
		WithArgs(sessionID, testAccountID, 1).
		WillReturnRows(sqlmock.NewRows([]string{"version", "files", "skipped", "file_count", "byte_size", "created_by", "created_at", "is_checkpoint", "is_deployed"}).
			AddRow(1, `[{"path":"index.html","content":"<h1>hi</h1>"}]`, `[]`, 1, 11, "agent", nowStub(), false, false))

	w := kumbhaRequest(server.BuildKumbhaSession, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/build",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]string{}, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

// TestDeleteKumbhaSessions_ClosesAnOpenOneThenDeletesBoth exercises the
// real end-to-end DeleteSessions behavior (2026-08-26 rework): an open
// session is closed (settling usage, tearing down any agent pod) before
// the delete, not skipped — a customer's explicit, confirmed delete
// request is authorization to stop a still-building session too, not
// just clean up ones that already finished. The other id doesn't belong
// to this account at all, so it never reaches Close and correctly ends
// up in "skipped" — existence-must-not-leak, same as everywhere else.
func TestDeleteKumbhaSessions_ClosesAnOpenOneThenDeletesBoth(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	openID, notOwnedID, projectID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT id, account_id, project_id, budget, spent, status`).
		WithArgs(openID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
			"last_deploy_failed", "last_deploy_error", "last_deploy_at",
		}).AddRow(openID, testAccountID, projectID, 5.0, 0.0, "open", nil, nil, nil, false, nowStub(), nil, false, nil, nil))
	mock.ExpectQuery(`UPDATE billing\.inference_sessions`).
		WithArgs(openID, testAccountID, "closed").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
		}).AddRow(openID, testAccountID, projectID, 5.0, 0.0, "closed", nil, nil, nil, false, nowStub(), nowStub()))

	mock.ExpectQuery(`SELECT id, account_id, project_id, budget, spent, status`).
		WithArgs(notOwnedID, testAccountID).
		WillReturnError(sqlNoRows())

	mock.ExpectQuery(`DELETE FROM billing\.inference_sessions`).
		WithArgs(testAccountID, pq.Array([]uuid.UUID{openID, notOwnedID})).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(openID))

	w := kumbhaRequest(server.DeleteKumbhaSessions, http.MethodPost, "/v1/kumbha/sessions/bulk-delete", nil,
		uuid.New(), map[string]any{"ids": []string{openID.String(), notOwnedID.String()}}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Deleted []string `json:"deleted"`
		Skipped []string `json:"skipped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Deleted) != 1 || resp.Deleted[0] != openID.String() {
		t.Errorf("got deleted %v, want only %v (closed then deleted)", resp.Deleted, openID)
	}
	if len(resp.Skipped) != 1 || resp.Skipped[0] != notOwnedID.String() {
		t.Errorf("got skipped %v, want only %v (not this account's session)", resp.Skipped, notOwnedID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// --- GetKumbhaSession ---
//
// What's worth testing here specifically is the split GetKumbhaSession
// introduces over every other session-returning endpoint (see
// kumbhaSessionResponse's own comment): a LIVE read of agent/app status
// rather than the cheap "was a pod ever launched" proxy.

// liveSessionRow is sessionRow (kumbha_workspace_handlers_test.go) with
// agent_instance_id/app_instance_id actually populated, so a test can
// exercise GetKumbhaSession's live-status enrichment instead of the
// no-agent-yet default every other helper row leaves at NULL.
func liveSessionRow(sessionID uuid.UUID, agentInstanceID, appInstanceID string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_id", "project_id", "budget", "spent", "status", "label",
		"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
		"last_deploy_failed", "last_deploy_error", "last_deploy_at",
	}).AddRow(sessionID, testAccountID, uuid.New(), 5.0, 0.0, "open", nil, agentInstanceID, appInstanceID, true, nowStub(), nil, false, nil, nil)
}

func TestGetKumbhaSession_ExposesAppInstanceID(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw) // no cluster wired

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(liveSessionRow(sessionID, "", "inst-deployed1"))

	w := kumbhaRequest(server.GetKumbhaSession, http.MethodGet, "/v1/kumbha/sessions/"+sessionID.String(),
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["app_instance_id"] != "inst-deployed1" {
		t.Errorf("app_instance_id = %v, want inst-deployed1", resp["app_instance_id"])
	}
	// No cluster wired: app_status must be absent, not guessed.
	if _, ok := resp["app_status"]; ok {
		t.Errorf("app_status = %v present with no cluster configured, want absent", resp["app_status"])
	}
}

func TestGetKumbhaSession_LiveAgentRunningOverridesTheCheapProxy(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	fc := newFakeCluster()
	server := (&Server{store: cStore, cluster: fc}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	agentID := "kumbha-agent-abcd1234"
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(liveSessionRow(sessionID, agentID, ""))

	// The agent pod already EXITED (found live 2026-08-29: this is
	// exactly the case the cheap agent_instance_id != "" proxy gets
	// wrong — it would report true here forever).
	// fakeCluster never had this id added, so GetInstanceStatus reports
	// cluster.ErrNotFound, which isAgentRunning maps to (false, nil).

	w := kumbhaRequest(server.GetKumbhaSession, http.MethodGet, "/v1/kumbha/sessions/"+sessionID.String(),
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["agent_running"] != false {
		t.Errorf("agent_running = %v, want false — the pod has exited even though agent_instance_id is still set", resp["agent_running"])
	}
}

func TestGetKumbhaSession_ReportsLiveAppStatus(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	fc := newFakeCluster()
	appID := "inst-deployed2"

	sessionID, projectID := uuid.New(), uuid.New()
	fc.add(appID, projectID.String(), compute.StatusRunning)
	server := (&Server{store: cStore, cluster: fc}).WithKumbha(gw)

	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(liveSessionRow(sessionID, "", appID))

	w := kumbhaRequest(server.GetKumbhaSession, http.MethodGet, "/v1/kumbha/sessions/"+sessionID.String(),
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["app_status"] != compute.StatusRunning {
		t.Errorf("app_status = %v, want %q", resp["app_status"], compute.StatusRunning)
	}
}

// TestGetKumbhaSession_ReportsLiveAppEndpoint is what actually lets the
// console auto-switch to its Preview tab and link straight to the
// deployed instance the moment a deploy succeeds — whether that deploy
// was the agent's own `deploy` MCP call or the console IDE's Deploy
// button — without parsing a URL out of an activity-feed event summary.
func TestGetKumbhaSession_ReportsLiveAppEndpoint(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	fc := newFakeCluster()
	appID := "inst-deployed3"

	sessionID, projectID := uuid.New(), uuid.New()
	fc.add(appID, projectID.String(), compute.StatusRunning)
	server := (&Server{store: cStore, cluster: fc}).WithKumbha(gw)

	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(liveSessionRow(sessionID, "", appID))
	// The endpoint must come from the STORED record, not
	// cluster.InstanceStatus.EndpointURL directly — that field is only
	// ever populated by DirectClient; AgentClient's cached status (the
	// platform's actual home-node topology) never carries it. See
	// resolveEndpoint's own doc comment (found live 2026-08-29).
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).
		WithArgs(appID).
		WillReturnRows(instanceRecordRow(appID, testAccountID, projectID, "kumbha-deployed3", "img:v1", 1, 1, 0, 80, "https://inst-deployed3.teepin.com"))

	w := kumbhaRequest(server.GetKumbhaSession, http.MethodGet, "/v1/kumbha/sessions/"+sessionID.String(),
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["app_endpoint"] != "https://inst-deployed3.teepin.com" {
		t.Errorf("app_endpoint = %v, want https://inst-deployed3.teepin.com", resp["app_endpoint"])
	}
}

// --- UpdateKumbhaBudget ---

func TestUpdateKumbhaBudget_Success(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sessionRow(sessionID)) // budget=5.0, status=open
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET budget`).
		WithArgs(15.0, sessionID, testAccountID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
			"last_deploy_failed", "last_deploy_error", "last_deploy_at",
		}).AddRow(sessionID, testAccountID, uuid.New(), 15.0, 0.0, "open", nil, nil, nil, false, nowStub(), nil, false, nil, nil))

	w := kumbhaRequest(server.UpdateKumbhaBudget, http.MethodPatch, "/v1/kumbha/sessions/"+sessionID.String()+"/budget",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]float64{"budget": 15.0}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["budget"] != 15.0 {
		t.Errorf("budget = %v, want 15", resp["budget"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

func TestUpdateKumbhaBudget_NotHigherIs400(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sessionRow(sessionID)) // budget=5.0

	w := kumbhaRequest(server.UpdateKumbhaBudget, http.MethodPatch, "/v1/kumbha/sessions/"+sessionID.String()+"/budget",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]float64{"budget": 5.0}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
}

func TestUpdateKumbhaBudget_NotAvailableWhenKumbhaNil(t *testing.T) {
	server := &Server{}
	w := kumbhaRequest(server.UpdateKumbhaBudget, http.MethodPatch, "/v1/kumbha/sessions/"+uuid.New().String()+"/budget",
		nil, uuid.New(), map[string]float64{"budget": 15.0}, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestDeleteKumbhaSessions_EmptyIDsIs400(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	w := kumbhaRequest(server.DeleteKumbhaSessions, http.MethodPost, "/v1/kumbha/sessions/bulk-delete", nil,
		uuid.New(), map[string]any{"ids": []string{}}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
}

// TestListKumbhaSessions_LiveAgentRunningNotStuckTrue is the regression
// test for a real live UX bug (found 2026-08-29): the "Previous builds"
// list showed an animated, ongoing-looking "Building" status for a
// session whose agent had long since finished — ListKumbhaSessions used
// to return only the cheap "was one ever launched" proxy for
// agent_running. It now shares enrichKumbhaAgentRunning with
// GetKumbhaSession, but deliberately NOT enrichKumbhaAppStatus — a
// per-row live cluster read on every list poll is a cost this endpoint
// does not pay (see that method's own doc comment); app_status/
// app_endpoint stay absent here even though the same session's
// GetKumbhaSession response would include them.
func TestListKumbhaSessions_LiveAgentRunningNotStuckTrue(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	fc := newFakeCluster()
	appID := "inst-listed01"
	sessID, projectID := uuid.New(), uuid.New()
	fc.add(appID, projectID.String(), compute.StatusRunning)
	server := (&Server{store: cStore, cluster: fc}).WithKumbha(gw)

	rows := sqlmock.NewRows([]string{
		"id", "account_id", "project_id", "budget", "spent", "status", "label",
		"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
		"last_deploy_failed", "last_deploy_error", "last_deploy_at",
	}).AddRow(sessID, testAccountID, projectID, 5.0, 1.0, "open", "a build",
		"kumbha-agent-abcd1234", appID, true, nowStub(), nil, false, nil, nil)
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(testAccountID, projectID).
		WillReturnRows(rows)

	w := kumbhaRequest(server.ListKumbhaSessions, http.MethodGet, "/v1/kumbha/sessions",
		nil, projectID, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(resp.Sessions))
	}
	// The agent pod (fakeCluster never had "kumbha-agent-abcd1234" added)
	// has exited — agent_running must reflect that live fact, not the
	// cheap agent_instance_id != "" proxy staying stuck true.
	if resp.Sessions[0]["agent_running"] != false {
		t.Errorf("agent_running = %v, want false (live read, agent pod exited)", resp.Sessions[0]["agent_running"])
	}
	if _, ok := resp.Sessions[0]["app_status"]; ok {
		t.Errorf("app_status = %v present on ListKumbhaSessions, want absent — that live read is GetKumbhaSession-only", resp.Sessions[0]["app_status"])
	}
}

// --- StopKumbhaAgent ---

func TestStopKumbhaAgent_KillsRunningPod(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	fc := newFakeCluster()
	sessionID, projectID := uuid.New(), uuid.New()
	agentID := "kumbha-agent-abcd1234"
	fc.add(agentID, projectID.String(), "running")

	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{}).
		WithAgent(fc, fakeMintKumbhaToken, kumbha.AgentConfig{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	// liveSessionRow's own project_id is a random uuid, unrelated to the
	// one this test's fake instance was added under — StopAgent scopes
	// its live status check to sess.ProjectID, so this row must carry the
	// SAME projectID the fake instance is visible under, unlike most
	// other liveSessionRow callers which never exercise a scoped read.
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
			"last_deploy_failed", "last_deploy_error", "last_deploy_at",
		}).AddRow(sessionID, testAccountID, projectID, 5.0, 0.0, "open", nil, agentID, nil, true, nowStub(), nil, false, nil, nil))

	w := kumbhaRequest(server.StopKumbhaAgent, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/stop",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if _, ok := fc.instances[agentID]; ok {
		t.Error("agent pod is still present after Stop, want it torn down")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStopKumbhaAgent_NothingRunningIs409(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	fc := newFakeCluster()
	sessionID, projectID := uuid.New(), uuid.New()

	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{}).
		WithAgent(fc, fakeMintKumbhaToken, kumbha.AgentConfig{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	// No agent_instance_id at all on this session — nothing to stop.
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(liveSessionRow(sessionID, "", ""))

	w := kumbhaRequest(server.StopKumbhaAgent, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/stop",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, nil, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", w.Code, w.Body.String())
	}
}
