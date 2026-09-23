// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	gw := NewGateway(store, nil, &fakeGate{allowed: false, reason: "no card on file"}, &fakePricing{}, &fakeUsageRecorder{})

	_, err := gw.CreateSession(context.Background(), uuid.New(), uuid.New(), 5.0, "test")
	if !errors.Is(err, ErrPaymentRequired) {
		t.Errorf("got %v, want ErrPaymentRequired", err)
	}
}

func TestGateway_CreateSession_GateErrorIsGateUnavailable(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, nil, &fakeGate{err: errors.New("db down")}, &fakePricing{}, &fakeUsageRecorder{})

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

	gw := NewGateway(store, nil, &fakeGate{allowed: true}, &fakePricing{}, &fakeUsageRecorder{})
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
	gw := NewGateway(store, nil, nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "closed", Budget: 5.0}
	_, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if !errors.Is(err, ErrSessionClosed) {
		t.Errorf("got %v, want ErrSessionClosed", err)
	}
}

func TestGateway_Complete_RefusesAlreadyExhaustedBudget(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, nil, nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "open", Budget: 5.0, Spent: 5.0}
	_, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("got %v, want ErrBudgetExhausted (pre-flight check)", err)
	}
}

func TestGateway_Complete_UnknownRoutePropagates(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, nil, nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "open", Budget: 5.0}
	_, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/nonexistent"})
	if !errors.Is(err, inference.ErrUnknownModel) {
		t.Errorf("got %v, want inference.ErrUnknownModel", err)
	}
}

func TestGateway_Complete_ProviderErrorPropagatesUnaccrued(t *testing.T) {
	store, mock := newMockStore(t)
	failing := &fakeProvider{name: "vllm", err: errors.New("upstream 500")}
	models := StaticModels{{Route: "teepin/fast", Engine: "vllm", Provider: failing}}
	gw := NewGateway(store, models, nil, &fakePricing{in: 1, out: 1}, &fakeUsageRecorder{})

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
	models := StaticModels{{Route: "teepin/fast", Engine: "vllm", Provider: provider}}
	usage := &fakeUsageRecorder{}
	gw := NewGateway(store, models, nil, &fakePricing{in: 2.0, out: 8.0}, usage)

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

	gw := NewGateway(store, nil, nil, &fakePricing{}, &fakeUsageRecorder{})
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

	gw := NewGateway(store, nil, nil, &fakePricing{}, &fakeUsageRecorder{})
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

	gw := NewGateway(store, nil, nil, &fakePricing{}, &fakeUsageRecorder{})
	err := gw.IncreaseBudget(context.Background(), sessID, accountID, 15.0)
	if !errors.Is(err, ErrSessionClosed) {
		t.Errorf("got %v, want ErrSessionClosed", err)
	}
}

func TestGateway_IncreaseBudget_GateDeniesIsPaymentRequired(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, nil, &fakeGate{allowed: false, reason: "no card on file"}, &fakePricing{}, &fakeUsageRecorder{})

	err := gw.IncreaseBudget(context.Background(), uuid.New(), uuid.New(), 15.0)
	if !errors.Is(err, ErrPaymentRequired) {
		t.Errorf("got %v, want ErrPaymentRequired", err)
	}
}

// With no model enabled for Kumbha, a completion is refused as a
// (retryable) outage rather than dispatched anywhere.
func TestGateway_Complete_NoKumbhaModelsIsUnavailable(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, StaticModels{}, nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "open", Budget: 5.0}
	_, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if !errors.Is(err, inference.ErrProviderUnavailable) {
		t.Errorf("got %v, want ErrProviderUnavailable when no model is enabled for Kumbha", err)
	}
}

// The alias is served by the catalog model: the backend is asked for the
// model's own route, while the session records the alias the customer sees.
func TestGateway_Complete_ServesAliasWithCatalogModel(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID := uuid.New(), uuid.New()
	var askedFor string
	provider := &recordingProvider{onComplete: func(req inference.Request) { askedFor = req.Model }}
	models := StaticModels{{Route: "anthropic/claude-haiku-4-5", Engine: "vllm", Provider: provider}}
	gw := NewGateway(store, models, nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: sessID, AccountID: accountID, Status: "open", Budget: 5.0}
	expectZeroCostAccrual(mock, sessID, accountID)

	if _, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if askedFor != "anthropic/claude-haiku-4-5" {
		t.Errorf("backend asked for %q, want the catalog model's route", askedFor)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The legacy frontier alias still works (an agent image or harness
// configured with it must not break), served by the same Kumbha models.
func TestGateway_Complete_LegacyDeepAliasIsServed(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID := uuid.New(), uuid.New()
	models := StaticModels{{Route: "teepin/qwen3-30b-a3b", Engine: "vllm", Provider: &fakeProvider{name: "vllm"}}}
	gw := NewGateway(store, models, nil, &fakePricing{}, &fakeUsageRecorder{})

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, budget, spent FROM billing\.inference_sessions`).
		WithArgs(sessID, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "budget", "spent"}).AddRow("open", 5.0, 0.0))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET spent`).
		WithArgs(sessID, 0.0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO billing\.inference_session_usage`).
		WithArgs(sessID, "teepin/deep", "vllm", 0, 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	sess := &Session{ID: sessID, AccountID: accountID, Status: "open", Budget: 5.0}
	if _, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/deep"}); err != nil {
		t.Fatalf("Complete: %v", err)
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

// --- Priority fallthrough across Kumbha's models ---

// countingProvider is like fakeProvider but records how many times
// Complete was called, so a test can assert a model was never tried.
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

// recordingProvider reports each request it receives.
type recordingProvider struct {
	onComplete func(inference.Request)
}

func (r *recordingProvider) Name() string { return "vllm" }
func (r *recordingProvider) Complete(_ context.Context, req inference.Request) (*inference.Response, error) {
	r.onComplete(req)
	return &inference.Response{}, nil
}
func (r *recordingProvider) Stream(context.Context, inference.Request, func(inference.Chunk) error) error {
	return nil
}
func (r *recordingProvider) Capabilities() inference.Capabilities { return inference.Capabilities{} }

func TestGateway_Complete_FallsThroughToNextModelOnProviderUnavailable(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID := uuid.New(), uuid.New()

	failing := &countingProvider{name: "vllm-a", err: fmt.Errorf("connect: %w", inference.ErrProviderUnavailable)}
	healthy := &countingProvider{name: "vllm"}
	models := StaticModels{
		{Route: "model-a", Engine: "vllm-a", Provider: failing},
		{Route: "model-b", Engine: "vllm", Provider: healthy}, // matches expectZeroCostAccrual's engine
	}
	gw := NewGateway(store, models, nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: sessID, AccountID: accountID, Status: "open", Budget: 5.0}
	expectZeroCostAccrual(mock, sessID, accountID)

	if _, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if failing.calls != 1 || healthy.calls != 1 {
		t.Errorf("calls = %d then %d, want 1 and 1", failing.calls, healthy.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A model disabled between listing and dispatch reads as unknown to the
// backend — that is a reason to try the next model, not to fail the turn.
func TestGateway_Complete_FallsThroughWhenModelDisappearedSinceListing(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID := uuid.New(), uuid.New()
	gone := &countingProvider{name: "vllm-a", err: fmt.Errorf("%w: %q", inference.ErrUnknownModel, "model-a")}
	models := StaticModels{
		{Route: "model-a", Engine: "vllm-a", Provider: gone},
		{Route: "model-b", Engine: "vllm", Provider: &countingProvider{name: "vllm"}},
	}
	gw := NewGateway(store, models, nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: sessID, AccountID: accountID, Status: "open", Budget: 5.0}
	expectZeroCostAccrual(mock, sessID, accountID)
	if _, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// A real rejection (bad request, content policy, ...) must surface
// immediately — it is not a reason to silently try a different model.
func TestGateway_Complete_DoesNotFallThroughOnRejection(t *testing.T) {
	store, mock := newMockStore(t)
	rejected := &countingProvider{name: "vllm-a", err: errors.New("400: prompt violates content policy")}
	neverCalled := &countingProvider{name: "vllm-b"}
	models := StaticModels{
		{Route: "model-a", Engine: "vllm-a", Provider: rejected},
		{Route: "model-b", Engine: "vllm-b", Provider: neverCalled},
	}
	gw := NewGateway(store, models, nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "open", Budget: 5.0}
	_, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if err == nil || err.Error() != rejected.err.Error() {
		t.Errorf("got %v, want the first model's own error surfaced directly", err)
	}
	if neverCalled.calls != 0 {
		t.Errorf("second model was called %d times, want 0", neverCalled.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB interaction on a failed completion: %v", err)
	}
}

// When every model is unavailable the caller learns only that — never
// which models back Kumbha, which is operator-only.
func TestGateway_Complete_AllModelsUnavailableHidesModelIdentity(t *testing.T) {
	store, _ := newMockStore(t)
	down := fmt.Errorf("connect anthropic/claude-haiku-4-5: %w", inference.ErrProviderUnavailable)
	models := StaticModels{{Route: "anthropic/claude-haiku-4-5", Engine: "anthropic", Provider: &countingProvider{err: down}}}
	gw := NewGateway(store, models, nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "open", Budget: 5.0}
	_, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if !errors.Is(err, inference.ErrProviderUnavailable) {
		t.Fatalf("got %v, want ErrProviderUnavailable", err)
	}
	if strings.Contains(err.Error(), "claude") {
		t.Errorf("error %q names the backing model", err)
	}
}

// --- Per-model pricing (WithModelPricing) ---

type fakeModelPricing struct {
	byRoute map[string][2]float64
}

func (f *fakeModelPricing) ModelPricing(_ context.Context, route string) (float64, float64, bool) {
	r, ok := f.byRoute[route]
	return r[0], r[1], ok
}

// A completion is priced at the rate of the model that actually served it,
// not the flat platform-wide one — the fix for the real bug found live
// 2026-09-22: build traffic on a paid Anthropic model priced identically to
// the free self-hosted one. The session still records the alias.
func TestGateway_Complete_PricesAtTheServingModelsCatalogRate(t *testing.T) {
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
		WithArgs(sessID, "teepin/fast", "anthropic", 1000, 500).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	provider := &fakeProvider{name: "anthropic", usage: inference.Usage{InputTokens: 1000, OutputTokens: 500}}
	models := StaticModels{{Route: "anthropic/claude-haiku-4-5", Engine: "anthropic", Provider: provider}}
	modelPricing := &fakeModelPricing{byRoute: map[string][2]float64{"anthropic/claude-haiku-4-5": {10.0, 50.0}}}
	usage := &fakeUsageRecorder{}
	gw := NewGateway(store, models, nil, &fakePricing{in: 1.0, out: 1.0}, usage).WithModelPricing(modelPricing)

	sess := &Session{ID: sessID, AccountID: accountID, Status: "open", Budget: 5.0}
	result, err := gw.Complete(context.Background(), sess, inference.Request{Model: "teepin/fast"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Cost != wantCost {
		t.Errorf("Cost = %v, want %v (catalog rate)", result.Cost, wantCost)
	}
	if len(usage.records) != 2 || usage.records[0].ResourceType != "kumbha/teepin/fast:input" {
		t.Errorf("usage records must name the alias, got %+v", usage.records)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A model with NO catalog entry falls back to the flat platform-wide rate
// rather than charging nothing.
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
	models := StaticModels{{Route: "teepin/qwen3-30b-a3b", Engine: "vllm", Provider: provider}}
	modelPricing := &fakeModelPricing{byRoute: map[string][2]float64{}} // not registered
	gw := NewGateway(store, models, nil, &fakePricing{in: 2.0, out: 8.0}, &fakeUsageRecorder{}).WithModelPricing(modelPricing)

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
