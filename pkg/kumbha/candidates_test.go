// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newCandidateStoreDB(t *testing.T) (*CandidateStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	return NewCandidateStore(db), mock, func() { db.Close() }
}

func candidateCols() []string {
	return []string{"id", "route_name", "priority", "provider_type", "base_url", "model",
		"context_window", "supports_tools", "max_output_tokens", "enabled",
		"has_secret", "updated_by", "created_at", "updated_at"}
}

func TestCandidateInput_Validate(t *testing.T) {
	cases := []struct {
		name    string
		in      CandidateInput
		wantErr bool
	}{
		{"valid vllm", CandidateInput{RouteName: "teepin/fast", ProviderType: "vllm", BaseURL: "http://x", Model: "m"}, false},
		{"valid anthropic, no base_url needed", CandidateInput{RouteName: "teepin/deep", ProviderType: "anthropic", Model: "claude-haiku-4-5-20251001"}, false},
		{"missing route_name", CandidateInput{ProviderType: "vllm", BaseURL: "http://x", Model: "m"}, true},
		{"bad provider_type", CandidateInput{RouteName: "teepin/fast", ProviderType: "openai", Model: "m"}, true},
		{"missing model", CandidateInput{RouteName: "teepin/fast", ProviderType: "vllm", BaseURL: "http://x"}, true},
		{"vllm missing base_url", CandidateInput{RouteName: "teepin/fast", ProviderType: "vllm", Model: "m"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.in.validate()
			if (err != nil) != c.wantErr {
				t.Errorf("validate() = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

func TestCandidateStore_ListByRoute_OrdersByPriority(t *testing.T) {
	store, mock, done := newCandidateStoreDB(t)
	defer done()
	now := time.Now()
	id1, id2 := uuid.New(), uuid.New()
	mock.ExpectQuery(`(?s)SELECT id, route_name, priority, provider_type, base_url, model.*FROM billing\.kumbha_route_candidates WHERE route_name = \$1 ORDER BY priority ASC, created_at ASC`).
		WithArgs("teepin/fast").
		WillReturnRows(sqlmock.NewRows(candidateCols()).
			AddRow(id1, "teepin/fast", 0, "vllm", "http://a", "model-a", 8000, true, 4096, true, false, "", now, now).
			AddRow(id2, "teepin/fast", 1, "vllm", "http://b", "model-b", 8000, true, 4096, true, true, "op", now, now))

	got, err := store.ListByRoute(context.Background(), "teepin/fast")
	if err != nil {
		t.Fatalf("ListByRoute: %v", err)
	}
	if len(got) != 2 || got[0].ID != id1 || got[1].ID != id2 {
		t.Fatalf("got %+v, want id1 then id2 in priority order", got)
	}
	if !got[1].HasSecret {
		t.Error("second row should report has_secret=true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestCandidateStore_Get_NotFound(t *testing.T) {
	store, mock, done := newCandidateStoreDB(t)
	defer done()
	id := uuid.New()
	mock.ExpectQuery(`FROM billing\.kumbha_route_candidates WHERE id = \$1`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows(candidateCols()))

	_, err := store.Get(context.Background(), id)
	if !errors.Is(err, ErrCandidateNotFound) {
		t.Errorf("got %v, want ErrCandidateNotFound", err)
	}
}

func TestCandidateStore_Create_RejectsInvalidInput(t *testing.T) {
	store, _, done := newCandidateStoreDB(t)
	defer done()
	_, err := store.Create(context.Background(), CandidateInput{ProviderType: "vllm"}, "op")
	if err == nil {
		t.Error("expected validation error for missing route_name/model, got nil")
	}
}

func TestCandidateStore_Create_Success(t *testing.T) {
	store, mock, done := newCandidateStoreDB(t)
	defer done()
	now := time.Now()
	id := uuid.New()
	in := CandidateInput{RouteName: "teepin/fast", Priority: 0, ProviderType: "vllm", BaseURL: "http://x", Model: "m", ContextWindow: 8000, SupportsTools: true, MaxOutputTokens: 4096, Enabled: true}
	mock.ExpectQuery(`INSERT INTO billing\.kumbha_route_candidates`).
		WithArgs(in.RouteName, in.Priority, in.ProviderType, in.BaseURL, in.Model, in.ContextWindow, in.SupportsTools, in.MaxOutputTokens, in.Enabled, "op").
		WillReturnRows(sqlmock.NewRows(candidateCols()).
			AddRow(id, in.RouteName, in.Priority, in.ProviderType, in.BaseURL, in.Model, in.ContextWindow, in.SupportsTools, in.MaxOutputTokens, in.Enabled, false, "op", now, now))

	got, err := store.Create(context.Background(), in, "op")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.ID != id {
		t.Errorf("got id %v, want %v", got.ID, id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestCandidateStore_Update_NotFound(t *testing.T) {
	store, mock, done := newCandidateStoreDB(t)
	defer done()
	id := uuid.New()
	in := CandidateInput{RouteName: "teepin/fast", ProviderType: "vllm", BaseURL: "http://x", Model: "m"}
	mock.ExpectQuery(`UPDATE billing\.kumbha_route_candidates`).
		WillReturnError(sql.ErrNoRows)
	_, err := store.Update(context.Background(), id, in, "op")
	if !errors.Is(err, ErrCandidateNotFound) {
		t.Errorf("got %v, want ErrCandidateNotFound", err)
	}
}

func TestCandidateStore_Delete_NotFound(t *testing.T) {
	store, mock, done := newCandidateStoreDB(t)
	defer done()
	id := uuid.New()
	mock.ExpectExec(`DELETE FROM billing\.kumbha_route_candidates`).
		WithArgs(id).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := store.Delete(context.Background(), id)
	if !errors.Is(err, ErrCandidateNotFound) {
		t.Errorf("got %v, want ErrCandidateNotFound", err)
	}
}

func TestCandidateStore_SetHasSecret(t *testing.T) {
	store, mock, done := newCandidateStoreDB(t)
	defer done()
	id := uuid.New()
	mock.ExpectExec(`(?s)UPDATE billing\.kumbha_route_candidates.*SET has_secret = \$1, updated_by = \$2, updated_at = NOW\(\).*WHERE id = \$3`).
		WithArgs(true, "op", id).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.SetHasSecret(context.Background(), id, true, "op"); err != nil {
		t.Fatalf("SetHasSecret: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
