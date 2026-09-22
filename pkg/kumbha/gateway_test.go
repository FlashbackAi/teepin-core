// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

type fakeGate struct {
	allowed bool
	reason  string
	err     error
}

func (f *fakeGate) AccountCanProvision(context.Context, uuid.UUID) (bool, string, error) {
	return f.allowed, f.reason, f.err
}

type fakePricing struct{ in, out float64 }

func (f *fakePricing) LLMPriceInputPerMillion(context.Context) float64  { return f.in }
func (f *fakePricing) LLMPriceOutputPerMillion(context.Context) float64 { return f.out }

type fakeUsageRecorder struct {
	records    []*billing.UsageRecord
	consumed   []float64
	recordErr  error
	consumeErr error
}

func (f *fakeUsageRecorder) RecordUsage(_ context.Context, r *billing.UsageRecord) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	r.ID = uuid.New()
	f.records = append(f.records, r)
	return nil
}

func (f *fakeUsageRecorder) ConsumeCredit(_ context.Context, _, _ uuid.UUID, cost float64) (float64, error) {
	if f.consumeErr != nil {
		return 0, f.consumeErr
	}
	f.consumed = append(f.consumed, cost)
	return cost, nil
}

func TestGateway_CreateSession_GateDeniesIsPaymentRequired(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, NewRouter(nil), &fakeGate{allowed: false, reason: "no card on file"}, &fakePricing{}, &fakeUsageRecorder{})

	_, err := gw.CreateSession(context.Background(), uuid.New(), uuid.New(), 5.0, "test")
	if !errors.Is(err, ErrPaymentRequired) {
		t.Errorf("got %v, want ErrPaymentRequired", err)
	}
}

func TestGateway_CreateSession_GateErrorIsGateUnavailable(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, NewRouter(nil), &fakeGate{err: errors.New("db down")}, &fakePricing{}, &fakeUsageRecorder{})

	_, err := gw.CreateSession(context.Background(), uuid.New(), uuid.New(), 5.0, "test")
	if !errors.Is(err, ErrGateUnavailable) {
		t.Errorf("got %v, want ErrGateUnavailable", err)
	}
}

func TestGateway_CreateSession_GateAllowsDelegatesToStore(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID := uuid.New(), uuid.New()

	mock.ExpectQuery(`INSERT INTO billing\.inference_sessions`).
		WithArgs(accountID, projectID, 5.0, "test").
		WillReturnRows(sqlmock.NewRows([]string{"id", "spent", "status", "started_at"}).
			AddRow(uuid.New(), 0.0, "open", time.Now()))

	gw := NewGateway(store, NewRouter(nil), &fakeGate{allowed: true}, &fakePricing{}, &fakeUsageRecorder{})
	sess, err := gw.CreateSession(context.Background(), accountID, projectID, 5.0, "test")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.Status != "open" {
		t.Errorf("session status = %q, want open", sess.Status)
	}
}

func TestGateway_Complete_RefusesClosedSession(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "closed", Budget: 5.0}
	_, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if !errors.Is(err, ErrSessionClosed) {
		t.Errorf("got %v, want ErrSessionClosed", err)
	}
}

func TestGateway_Complete_RefusesAlreadyExhaustedBudget(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "open", Budget: 5.0, Spent: 5.0}
	_, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("got %v, want ErrBudgetExhausted (pre-flight check)", err)
	}
}

func TestGateway_Complete_UnknownRoutePropagates(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "open", Budget: 5.0}
	_, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/nonexistent"})
	if !errors.Is(err, inference.ErrUnknownModel) {
		t.Errorf("got %v, want inference.ErrUnknownModel", err)
	}
}

func TestGateway_Complete_ProviderErrorPropagatesUnaccrued(t *testing.T) {
	store, mock := newMockStore(t)
	failing := &fakeProvider{name: "vllm", err: errors.New("upstream 500")}
	router := NewRouter(map[string]Route{"teepin/fast": {Provider: failing, ProviderName: "vllm"}})
	gw := NewGateway(store, router, nil, &fakePricing{in: 1, out: 1}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "open", Budget: 5.0}
	_, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if err == nil {
		t.Fatal("expected the provider's error to propagate")
	}
	// A failed completion must never reach Accrue — nothing was actually
	// spent, so no DB call should have been made at all.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB interaction on a failed completion: %v", err)
	}
}

func TestGateway_Complete_SuccessPricesAndAccrues(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID := uuid.New(), uuid.New()

	// $2/M input, $8/M output; 1000 input + 500 output tokens.
	wantCost := 1000.0/1e6*2.0 + 500.0/1e6*8.0

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, budget, spent FROM billing\.inference_sessions`).
		WithArgs(sessID, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "budget", "spent"}).AddRow("open", 5.0, 0.0))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET spent`).
		WithArgs(sessID, wantCost).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO billing\.inference_session_usage`).
		WithArgs(sessID, "teepin/fast", "vllm", 1000, 500).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	provider := &fakeProvider{name: "vllm", usage: inference.Usage{InputTokens: 1000, OutputTokens: 500}}
	router := NewRouter(map[string]Route{"teepin/fast": {Provider: provider, ProviderName: "vllm"}})
	usage := &fakeUsageRecorder{}
	gw := NewGateway(store, router, nil, &fakePricing{in: 2.0, out: 8.0}, usage)

	sess := &Session{ID: sessID, AccountID: accountID, Status: "open", Budget: 5.0, Spent: 0}
	result, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Cost != wantCost {
		t.Errorf("Cost = %v, want %v", result.Cost, wantCost)
	}
	if result.Spent != wantCost {
		t.Errorf("Spent = %v, want %v", result.Spent, wantCost)
	}
	// Settled IMMEDIATELY, not batched until Close (which no longer
	// settles anything at all — see Gateway.CloseSession's own doc
	// comment). This is what lets a session's spend become an
	// invoice-visible, credit-consuming fact even if it is simply
	// abandoned and never explicitly stopped.
	if len(usage.records) != 2 {
		t.Fatalf("got %d usage_records lines, want 2 (input + output)", len(usage.records))
	}
	if usage.records[0].ResourceType != "kumbha/teepin/fast:input" {
		t.Errorf("first line resource_type = %q", usage.records[0].ResourceType)
	}
	if usage.records[1].ResourceType != "kumbha/teepin/fast:output" {
		t.Errorf("second line resource_type = %q", usage.records[1].ResourceType)
	}
	if len(usage.consumed) != 2 {
		t.Fatalf("got %d ConsumeCredit calls, want 2", len(usage.consumed))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// --- IncreaseBudget ---

// getSessionRow mocks the plain SELECT Store.Get issues — same column
// shape as CloseSession's own RETURNING row above, different query text.
func getSessionRow(sessID, accountID, projectID uuid.UUID, budget float64, status string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_id", "project_id", "budget", "spent", "status", "label",
		"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
		"last_deploy_failed", "last_deploy_error", "last_deploy_at",
	}).AddRow(sessID, accountID, projectID, budget, 0.0, status, nil, nil, nil, false, time.Now(), nil, false, nil, nil)
}

func TestGateway_IncreaseBudget_Success(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessID, accountID).
		WillReturnRows(getSessionRow(sessID, accountID, projectID, 5.0, "open"))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET budget`).
		WithArgs(15.0, sessID, accountID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{})
	if err := gw.IncreaseBudget(context.Background(), sessID, accountID, 15.0); err != nil {
		t.Fatalf("IncreaseBudget: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestGateway_IncreaseBudget_NotHigherIsErrBudgetNotIncreased(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessID, accountID).
		WillReturnRows(getSessionRow(sessID, accountID, projectID, 5.0, "open"))

	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{})
	err := gw.IncreaseBudget(context.Background(), sessID, accountID, 5.0)
	if !errors.Is(err, ErrBudgetNotIncreased) {
		t.Errorf("got %v, want ErrBudgetNotIncreased for a value equal to the current budget", err)
	}
	// The SQL UPDATE must never even be attempted — the check happens
	// entirely off the row already loaded, no wasted round trip on a
	// request that was always going to be rejected.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestGateway_IncreaseBudget_ClosedSessionIsErrSessionClosed(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessID, accountID).
		WillReturnRows(getSessionRow(sessID, accountID, projectID, 5.0, "closed"))

	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{})
	err := gw.IncreaseBudget(context.Background(), sessID, accountID, 15.0)
	if !errors.Is(err, ErrSessionClosed) {
		t.Errorf("got %v, want ErrSessionClosed", err)
	}
}

func TestGateway_IncreaseBudget_GateDeniesIsPaymentRequired(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, NewRouter(nil), &fakeGate{allowed: false, reason: "no card on file"}, &fakePricing{}, &fakeUsageRecorder{})

	err := gw.IncreaseBudget(context.Background(), uuid.New(), uuid.New(), 15.0)
	if !errors.Is(err, ErrPaymentRequired) {
		t.Errorf("got %v, want ErrPaymentRequired", err)
	}
}

// A disabled route is presented identically to an unconfigured one — a
// customer never learns whether a route exists but was turned off, vs
// never existed at all (see KUMBHA-DESIGN.md's "no console page of its
// own": route/backend identity is operator-only).
func TestGateway_Complete_DisabledRouteIsUnknownModel(t *testing.T) {
	store, _ := newMockStore(t)
	fast := &fakeProvider{name: "vllm"}
	router := NewRouter(map[string]Route{"teepin/fast": {Provider: fast, ProviderName: "vllm"}})
	gw := NewGateway(store, router, nil, &fakePricing{}, &fakeUsageRecorder{})

	routeDB, routeMock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer routeDB.Close()
	routeMock.ExpectQuery(`SELECT enabled FROM billing\.kumbha_routes`).
		WithArgs("teepin/fast").
		WillReturnRows(sqlmock.NewRows([]string{"enabled"}).AddRow(false))
	gw = gw.WithRouteControl(NewRouteStore(routeDB))

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "open", Budget: 5.0}
	_, err = gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if !errors.Is(err, inference.ErrUnknownModel) {
		t.Errorf("got %v, want inference.ErrUnknownModel for a disabled route", err)
	}
}

// A route with no explicit setting stays usable — the "ships on" default —
// and an enabled route completes normally with route control wired in.
func TestGateway_Complete_EnabledRouteStillWorksWithRouteControlWired(t *testing.T) {
	store, mock := newMockStore(t)
	fast := &fakeProvider{name: "vllm"}
	router := NewRouter(map[string]Route{"teepin/fast": {Provider: fast, ProviderName: "vllm"}})
	gw := NewGateway(store, router, nil, &fakePricing{}, &fakeUsageRecorder{})

	routeDB, routeMock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer routeDB.Close()
	routeMock.ExpectQuery(`SELECT enabled FROM billing\.kumbha_routes`).
		WithArgs("teepin/fast").
		WillReturnError(sql.ErrNoRows)
	gw = gw.WithRouteControl(NewRouteStore(routeDB))

	sessID, accountID := uuid.New(), uuid.New()
	sess := &Session{ID: sessID, AccountID: accountID, Status: "open", Budget: 5.0}
	expectZeroCostAccrual(mock, sessID, accountID)

	if _, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// A failure to READ the enabled flag (a DB blip) must not take down every
// Kumbha completion — it fails open, unlike a payment gate.
func TestGateway_Complete_RouteCheckFailureFailsOpen(t *testing.T) {
	store, mock := newMockStore(t)
	fast := &fakeProvider{name: "vllm"}
	router := NewRouter(map[string]Route{"teepin/fast": {Provider: fast, ProviderName: "vllm"}})
	gw := NewGateway(store, router, nil, &fakePricing{}, &fakeUsageRecorder{})

	routeDB, routeMock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer routeDB.Close()
	routeMock.ExpectQuery(`SELECT enabled FROM billing\.kumbha_routes`).
		WithArgs("teepin/fast").
		WillReturnError(errors.New("connection reset"))
	gw = gw.WithRouteControl(NewRouteStore(routeDB))

	sessID, accountID := uuid.New(), uuid.New()
	sess := &Session{ID: sessID, AccountID: accountID, Status: "open", Budget: 5.0}
	expectZeroCostAccrual(mock, sessID, accountID)

	if _, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"}); err != nil {
		t.Fatalf("Complete should fail open on a route-check error, got: %v", err)
	}
}

// expectZeroCostAccrual primes the full Accrue transaction for a completion
// with zero usage/cost (the default fakeProvider) — status/budget/spent
// locked, spend written unchanged, and a zero/zero usage row upserted;
// Complete's own settleLine calls are skipped entirely for zero tokens, so
// nothing beyond Accrue touches the DB.
func expectZeroCostAccrual(mock sqlmock.Sqlmock, sessID, accountID uuid.UUID) {
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, budget, spent FROM billing\.inference_sessions`).
		WithArgs(sessID, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "budget", "spent"}).AddRow("open", 5.0, 0.0))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET spent`).
		WithArgs(sessID, 0.0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO billing\.inference_session_usage`).
		WithArgs(sessID, "teepin/fast", "vllm", 0, 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

// --- Candidate priority-fallback dispatch (WithCandidates) ---

type fakeCandidateSource struct {
	byRoute map[string][]RouteCandidate
	err     error
}

func (f *fakeCandidateSource) ListByRoute(_ context.Context, route string) ([]RouteCandidate, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byRoute[route], nil
}

// fakeCandidateBuilder maps a candidate's Model field to a pre-built
// Provider, so a test controls exactly what each candidate does without a
// real HTTP client or a real Secrets Manager call.
type fakeCandidateBuilder struct {
	byModel map[string]inference.Provider
}

func (f *fakeCandidateBuilder) Build(_ context.Context, c RouteCandidate) (inference.Provider, error) {
	p, ok := f.byModel[c.Model]
	if !ok {
		return nil, fmt.Errorf("no fake provider registered for model %q", c.Model)
	}
	return p, nil
}

// countingProvider is like fakeProvider but records how many times
// Complete was called, so a test can assert a candidate was never tried.
type countingProvider struct {
	name  string
	err   error
	usage inference.Usage
	calls int
}

func (c *countingProvider) Name() string { return c.name }
func (c *countingProvider) Complete(context.Context, inference.Request) (*inference.Response, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return &inference.Response{Model: c.name, Usage: c.usage}, nil
}
func (c *countingProvider) Stream(context.Context, inference.Request, func(inference.Chunk) error) error {
	return nil
}
func (c *countingProvider) Capabilities() inference.Capabilities { return inference.Capabilities{} }

func candidateRow(route, model string, priority int, enabled bool) RouteCandidate {
	return RouteCandidate{ID: uuid.New(), RouteName: route, Priority: priority, ProviderType: "vllm", Model: model, Enabled: enabled}
}

func TestGateway_Complete_FallsThroughToNextCandidateOnProviderUnavailable(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID := uuid.New(), uuid.New()

	failing := &countingProvider{name: "vllm-a", err: fmt.Errorf("connect: %w", inference.ErrProviderUnavailable)}
	healthy := &countingProvider{name: "vllm"} // matches expectZeroCostAccrual's expected provider name

	candidates := &fakeCandidateSource{byRoute: map[string][]RouteCandidate{
		"teepin/fast": {candidateRow("teepin/fast", "model-a", 0, true), candidateRow("teepin/fast", "model-b", 1, true)},
	}}
	builder := &fakeCandidateBuilder{byModel: map[string]inference.Provider{"model-a": failing, "model-b": healthy}}

	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).WithCandidates(candidates, builder)

	sess := &Session{ID: sessID, AccountID: accountID, Status: "open", Budget: 5.0}
	expectZeroCostAccrual(mock, sessID, accountID)

	if _, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if failing.calls != 1 {
		t.Errorf("first candidate called %d times, want 1", failing.calls)
	}
	if healthy.calls != 1 {
		t.Errorf("second candidate called %d times, want 1", healthy.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A non-ErrProviderUnavailable error (a real rejection: bad request,
// content policy, etc.) must surface immediately — it is not a reason to
// silently try a different, unrelated backend.
func TestGateway_Complete_DoesNotFallThroughOnNonProviderUnavailableError(t *testing.T) {
	store, mock := newMockStore(t)
	rejected := &countingProvider{name: "vllm-a", err: errors.New("400: prompt violates content policy")}
	neverCalled := &countingProvider{name: "vllm-b"}

	candidates := &fakeCandidateSource{byRoute: map[string][]RouteCandidate{
		"teepin/fast": {candidateRow("teepin/fast", "model-a", 0, true), candidateRow("teepin/fast", "model-b", 1, true)},
	}}
	builder := &fakeCandidateBuilder{byModel: map[string]inference.Provider{"model-a": rejected, "model-b": neverCalled}}

	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).WithCandidates(candidates, builder)

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "open", Budget: 5.0}
	_, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if err == nil || err.Error() != rejected.err.Error() {
		t.Errorf("got %v, want the first candidate's own error surfaced directly", err)
	}
	if neverCalled.calls != 0 {
		t.Errorf("second candidate was called %d times, want 0", neverCalled.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB interaction on a failed completion: %v", err)
	}
}

// A disabled candidate is skipped, same as a disabled route — an operator
// taking one backend offline for maintenance without deleting its config.
func TestGateway_Complete_SkipsDisabledCandidates(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID := uuid.New(), uuid.New()
	disabled := &countingProvider{name: "vllm-a"}
	enabled := &countingProvider{name: "vllm"}

	candidates := &fakeCandidateSource{byRoute: map[string][]RouteCandidate{
		"teepin/fast": {candidateRow("teepin/fast", "model-a", 0, false), candidateRow("teepin/fast", "model-b", 1, true)},
	}}
	builder := &fakeCandidateBuilder{byModel: map[string]inference.Provider{"model-a": disabled, "model-b": enabled}}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).WithCandidates(candidates, builder)

	sess := &Session{ID: sessID, AccountID: accountID, Status: "open", Budget: 5.0}
	expectZeroCostAccrual(mock, sessID, accountID)

	if _, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if disabled.calls != 0 {
		t.Errorf("disabled candidate was called %d times, want 0", disabled.calls)
	}
}

// Every candidate disabled reads identically to a route with none at all —
// same "operator-only, never distinguishable to a customer" posture as a
// disabled route.
func TestGateway_Complete_AllCandidatesDisabledIsUnknownModel(t *testing.T) {
	store, _ := newMockStore(t)
	candidates := &fakeCandidateSource{byRoute: map[string][]RouteCandidate{
		"teepin/fast": {candidateRow("teepin/fast", "model-a", 0, false)},
	}}
	builder := &fakeCandidateBuilder{byModel: map[string]inference.Provider{}}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).WithCandidates(candidates, builder)

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "open", Budget: 5.0}
	_, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if !errors.Is(err, inference.ErrUnknownModel) {
		t.Errorf("got %v, want inference.ErrUnknownModel", err)
	}
}

// A route with no candidate rows at all falls back to the static,
// env-var-configured router unchanged — WithCandidates must not break a
// deployment that has never registered any candidates.
func TestGateway_Complete_NoCandidatesFallsBackToStaticRouter(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID := uuid.New(), uuid.New()
	static := &fakeProvider{name: "vllm"}
	router := NewRouter(map[string]Route{"teepin/fast": {Provider: static, ProviderName: "vllm"}})
	candidates := &fakeCandidateSource{byRoute: map[string][]RouteCandidate{}} // nothing registered
	builder := &fakeCandidateBuilder{byModel: map[string]inference.Provider{}}
	gw := NewGateway(store, router, nil, &fakePricing{}, &fakeUsageRecorder{}).WithCandidates(candidates, builder)

	sess := &Session{ID: sessID, AccountID: accountID, Status: "open", Budget: 5.0}
	expectZeroCostAccrual(mock, sessID, accountID)

	if _, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// --- Per-route pricing (WithModelPricing) ---

type fakeModelPricing struct {
	byRoute map[string][2]float64
}

func (f *fakeModelPricing) ModelPricing(_ context.Context, route string) (float64, float64, bool) {
	r, ok := f.byRoute[route]
	return r[0], r[1], ok
}

// A route registered in the model catalog is priced off ITS OWN rate, not
// the flat platform-wide one — this is the fix for the real bug found live
// 2026-09-22: teepin/deep routing to a paid Anthropic model while still
// priced identically to the free self-hosted teepin/fast.
func TestGateway_Complete_PrefersModelCatalogPricingOverFlatRate(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID := uuid.New(), uuid.New()

	// Catalog says $10/M in, $50/M out; flat rate says $1/M in, $1/M out —
	// if the flat rate won, wantCost below would be wrong and the test
	// would fail.
	wantCost := 1000.0/1e6*10.0 + 500.0/1e6*50.0

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, budget, spent FROM billing\.inference_sessions`).
		WithArgs(sessID, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "budget", "spent"}).AddRow("open", 5.0, 0.0))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET spent`).
		WithArgs(sessID, wantCost).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO billing\.inference_session_usage`).
		WithArgs(sessID, "teepin/deep", "anthropic", 1000, 500).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	provider := &fakeProvider{name: "anthropic", usage: inference.Usage{InputTokens: 1000, OutputTokens: 500}}
	router := NewRouter(map[string]Route{"teepin/deep": {Provider: provider, ProviderName: "anthropic"}})
	modelPricing := &fakeModelPricing{byRoute: map[string][2]float64{"teepin/deep": {10.0, 50.0}}}
	gw := NewGateway(store, router, nil, &fakePricing{in: 1.0, out: 1.0}, &fakeUsageRecorder{}).WithModelPricing(modelPricing)

	sess := &Session{ID: sessID, AccountID: accountID, Status: "open", Budget: 5.0}
	result, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/deep"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Cost != wantCost {
		t.Errorf("Cost = %v, want %v (catalog rate)", result.Cost, wantCost)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A route with NO catalog entry falls back to the flat platform-wide rate
// — the pre-existing behaviour, unaffected by WithModelPricing being wired
// in for routes that do have one.
func TestGateway_Complete_FallsBackToFlatRateWithNoCatalogEntry(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID := uuid.New(), uuid.New()
	wantCost := 1000.0/1e6*2.0 + 500.0/1e6*8.0

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, budget, spent FROM billing\.inference_sessions`).
		WithArgs(sessID, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "budget", "spent"}).AddRow("open", 5.0, 0.0))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET spent`).
		WithArgs(sessID, wantCost).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO billing\.inference_session_usage`).
		WithArgs(sessID, "teepin/fast", "vllm", 1000, 500).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	provider := &fakeProvider{name: "vllm", usage: inference.Usage{InputTokens: 1000, OutputTokens: 500}}
	router := NewRouter(map[string]Route{"teepin/fast": {Provider: provider, ProviderName: "vllm"}})
	modelPricing := &fakeModelPricing{byRoute: map[string][2]float64{}} // teepin/fast not registered
	gw := NewGateway(store, router, nil, &fakePricing{in: 2.0, out: 8.0}, &fakeUsageRecorder{}).WithModelPricing(modelPricing)

	sess := &Session{ID: sessID, AccountID: accountID, Status: "open", Budget: 5.0}
	result, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Cost != wantCost {
		t.Errorf("Cost = %v, want %v (flat fallback rate)", result.Cost, wantCost)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
