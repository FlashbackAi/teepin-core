// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"errors"
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
