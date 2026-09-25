// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package modelcatalog

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMock(t *testing.T) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewService(db), mock, func() { db.Close() }
}

func TestRegisterModel_UpsertsCatalogFieldsOnly(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectExec(`INSERT INTO inference\.models`).
		WithArgs("teepin/qwen3-omni-7b", "Qwen3 Omni 7B", "own", "vllm-omni", 32768,
			true, true, true, true, "node", "", "", 4096, "op").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := s.RegisterModel(context.Background(), Model{
		ModelRoute:     "teepin/qwen3-omni-7b",
		DisplayName:    "Qwen3 Omni 7B",
		CostClass:      CostClassOwn,
		Engine:         "vllm-omni",
		ContextWindow:  32768,
		SupportsTools:  true,
		SupportsVision: true,
		SupportsAudio:  true,
		Enabled:        true,
		UpdatedBy:      strPtr("op"),
	})
	if err != nil {
		t.Fatalf("RegisterModel: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestRegisterModel_Validation(t *testing.T) {
	s := NewService(nil) // rejected before any query

	if err := s.RegisterModel(context.Background(), Model{Engine: "vllm", CostClass: CostClassOwn}); err == nil {
		t.Error("blank model_route accepted")
	}
	if err := s.RegisterModel(context.Background(), Model{ModelRoute: "x", Engine: "vllm", CostClass: "bogus"}); err == nil {
		t.Error("invalid cost_class accepted")
	}
	if err := s.RegisterModel(context.Background(), Model{ModelRoute: "x", CostClass: CostClassOwn}); err == nil {
		t.Error("blank engine accepted")
	}
	if err := s.RegisterModel(context.Background(), Model{ModelRoute: "x", Engine: "vllm", CostClass: CostClassOwn, ContextWindow: -1}); err == nil {
		t.Error("negative context_window accepted")
	}
	if err := s.RegisterModel(context.Background(), Model{ModelRoute: "x", Engine: "anthropic", CostClass: CostClassFrontier, Provider: ProviderAnthropic}); err == nil {
		t.Error("anthropic model without provider_model accepted")
	}
	if err := s.RegisterModel(context.Background(), Model{ModelRoute: "x", Engine: "vllm", CostClass: CostClassOwn, Provider: ProviderOpenAICompatible, ProviderModel: "m"}); err == nil {
		t.Error("openai_compatible model without base_url accepted")
	}
	if err := s.RegisterModel(context.Background(), Model{ModelRoute: "x", Engine: "tinfoil", CostClass: CostClassFrontier, Provider: ProviderTinfoilConfidential, ProviderModel: "qwen3-omni"}); err == nil {
		t.Error("tinfoil_confidential model without base_url accepted")
	}
	if err := s.RegisterModel(context.Background(), Model{ModelRoute: "x", Engine: "vllm", CostClass: CostClassOwn, Provider: "bogus"}); err == nil {
		t.Error("invalid provider accepted")
	}
}

func TestRegisterModel_ExternalModelCarriesProviderFields(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectExec(`INSERT INTO inference\.models`).
		WithArgs("anthropic/claude-haiku-4-5", "Claude Haiku 4.5", "frontier", "anthropic", 200000,
			true, false, false, true, "anthropic", "claude-haiku-4-5-20251001", "", 4096, "op").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := s.RegisterModel(context.Background(), Model{
		ModelRoute:    "anthropic/claude-haiku-4-5",
		DisplayName:   "Claude Haiku 4.5",
		CostClass:     CostClassFrontier,
		Engine:        "anthropic",
		ContextWindow: 200000,
		SupportsTools: true,
		Enabled:       true,
		Provider:      ProviderAnthropic,
		ProviderModel: "claude-haiku-4-5-20251001",
		UpdatedBy:     strPtr("op"),
	})
	if err != nil {
		t.Fatalf("RegisterModel: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A nil field in an availability update must leave that column untouched
// — toggling Kumbha on must never also flip customer exposure.
func TestSetAvailability_PartialUpdateLeavesNilFieldsAlone(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	on := true
	mock.ExpectExec(`SET offered_to_customers = COALESCE\(\$1, offered_to_customers\)`).
		WithArgs(nil, &on, nil, "op", "anthropic/claude-haiku-4-5").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.SetAvailability(context.Background(), "anthropic/claude-haiku-4-5", Availability{KumbhaEnabled: &on}, "op"); err != nil {
		t.Fatalf("SetAvailability: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSetAvailability_NotFound(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectExec(`UPDATE inference\.models`).WillReturnResult(sqlmock.NewResult(0, 0))
	on := true
	if err := s.SetAvailability(context.Background(), "missing", Availability{KumbhaEnabled: &on}, "op"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// Kumbha's list must only contain models that are both enabled and
// Kumbha-enabled, in priority order — the order the build agent tries them.
func TestListKumbhaModels_FiltersAndOrdersByPriority(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`WHERE enabled AND kumbha_enabled ORDER BY kumbha_priority, model_route`).
		WillReturnRows(sqlmock.NewRows(modelRowColumns()))

	if _, err := s.ListKumbhaModels(context.Background()); err != nil {
		t.Fatalf("ListKumbhaModels: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestSetPricing_SeparateFromRegister proves pricing has its own endpoint,
// independent of catalog registration — mirrors billing.pricing's own
// "one endpoint per rate-pair" convention.
func TestSetPricing_SeparateFromRegister(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectExec(`UPDATE inference\.models\s+SET input_price_per_million`).
		WithArgs(0.10, 0.30, "op", "teepin/qwen3-omni-7b").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.SetPricing(context.Background(), "teepin/qwen3-omni-7b", 0.10, 0.30, "op"); err != nil {
		t.Fatalf("SetPricing: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSetPricing_RejectsNegativeRates(t *testing.T) {
	s := NewService(nil)
	if err := s.SetPricing(context.Background(), "x", -1, 0, "op"); err == nil {
		t.Error("negative input rate accepted")
	}
}

func TestSetPricing_NotFound(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectExec(`UPDATE inference\.models`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := s.SetPricing(context.Background(), "missing", 0.1, 0.2, "op"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestSetVendorCost_NilClearsRatherThanZero proves a nil vendor cost is
// stored as NULL ("unknown"), never coerced into a fabricated zero that
// would look like a real, known-zero cost.
func TestSetVendorCost_NilClearsRatherThanZero(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectExec(`UPDATE inference\.models\s+SET vendor_input_cost_per_million`).
		WithArgs(nil, nil, "op", "anthropic/claude-sonnet-5").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.SetVendorCost(context.Background(), "anthropic/claude-sonnet-5", nil, nil, "op"); err != nil {
		t.Fatalf("SetVendorCost: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestGetModel_ReturnsFullRow(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	now := time.Now()
	vendorIn, vendorOut := 3.0, 15.0
	mock.ExpectQuery(`SELECT model_route, display_name, cost_class`).
		WithArgs("anthropic/claude-sonnet-5").
		WillReturnRows(sqlmock.NewRows(modelRowColumns()).AddRow(
			"anthropic/claude-sonnet-5", "Claude Sonnet 5", "frontier", "anthropic", 1000000,
			true, true, false,
			4.5, 18.0,
			&vendorIn, &vendorOut,
			true, "anthropic", "claude-sonnet-5", "", 8192,
			"inference-model-key-abc", false, true, 1,
			"op", now, now,
		))

	m, err := s.GetModel(context.Background(), "anthropic/claude-sonnet-5")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if m.CostClass != CostClassFrontier {
		t.Errorf("CostClass = %q, want frontier", m.CostClass)
	}
	if m.VendorInputCostPerMillion == nil || *m.VendorInputCostPerMillion != 3.0 {
		t.Errorf("VendorInputCostPerMillion = %v, want 3.0", m.VendorInputCostPerMillion)
	}
	if m.Provider != ProviderAnthropic || m.ProviderModel != "claude-sonnet-5" || m.MaxOutputTokens != 8192 {
		t.Errorf("provider fields = %q %q %d", m.Provider, m.ProviderModel, m.MaxOutputTokens)
	}
	if !m.HasAPIKey || m.APIKeyRef != "inference-model-key-abc" {
		t.Errorf("HasAPIKey = %v, APIKeyRef = %q", m.HasAPIKey, m.APIKeyRef)
	}
	if m.OfferedToCustomers || !m.KumbhaEnabled || m.KumbhaPriority != 1 {
		t.Errorf("availability = customers %v, kumbha %v/%d", m.OfferedToCustomers, m.KumbhaEnabled, m.KumbhaPriority)
	}
}

func TestGetModel_NotFound(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT model_route, display_name, cost_class`).
		WillReturnError(sql.ErrNoRows)

	if _, err := s.GetModel(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestListModels_EmptyCatalogReturnsEmptySliceNotNil is the regression test
// for a real bug found live 2026-09-18: an empty result set left `out` as
// a nil slice, which encoding/json marshals to `null` rather than `[]` —
// crashing the console's catalog page (`models.data?.models.length`) the
// moment the catalog was genuinely empty, the exact state a fresh
// deployment starts in.
func TestListModels_EmptyCatalogReturnsEmptySliceNotNil(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`SELECT model_route, display_name, cost_class`).
		WillReturnRows(sqlmock.NewRows(modelRowColumns()))

	models, err := s.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if models == nil {
		t.Fatal("ListModels returned nil for an empty catalog — must be []Model{} so it JSON-marshals to [], not null")
	}
	if len(models) != 0 {
		t.Errorf("got %d models, want 0", len(models))
	}
}

func modelRowColumns() []string {
	return []string{
		"model_route", "display_name", "cost_class", "engine", "context_window",
		"supports_tools", "supports_vision", "supports_audio",
		"input_price_per_million", "output_price_per_million",
		"vendor_input_cost_per_million", "vendor_output_cost_per_million",
		"enabled", "provider", "provider_model", "base_url", "max_output_tokens",
		"api_key_ref", "offered_to_customers", "kumbha_enabled", "kumbha_priority",
		"updated_by", "created_at", "updated_at",
	}
}

func strPtr(s string) *string { return &s }
