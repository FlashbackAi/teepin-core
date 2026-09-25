// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inferencegateway

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
)

// fakeHealthCheckingProvider is a fakeProvider that additionally implements
// inference.HealthChecker with a canned result — used to drive checkOne
// down its CheckHealth branch without any real network call.
type fakeHealthCheckingProvider struct {
	*fakeProvider
	err error
}

func (f *fakeHealthCheckingProvider) CheckHealth(context.Context) error { return f.err }

var _ inference.HealthChecker = (*fakeHealthCheckingProvider)(nil)

var confidentialModel = modelcatalog.Model{
	ModelRoute: "teepin/confidential-omni", CostClass: modelcatalog.CostClassFrontier, Engine: "tinfoil",
	Enabled: true, Provider: modelcatalog.ProviderTinfoilConfidential, ProviderModel: "qwen3-omni",
}

// expectListModels primes the one ListModels query CheckExternalHealth/
// CheckConfidentialAttestation issue (no WHERE clause, no args) — separate
// from expectGetModel's single-route lookup, even though both match the
// same leading SQL text, since sqlmock consumes expectations in the order
// they were registered.
func expectListModels(mock sqlmock.Sqlmock, models []modelcatalog.Model) {
	rows := sqlmock.NewRows([]string{
		"model_route", "display_name", "cost_class", "engine", "context_window",
		"supports_tools", "supports_vision", "supports_audio",
		"input_price_per_million", "output_price_per_million",
		"vendor_input_cost_per_million", "vendor_output_cost_per_million",
		"enabled", "provider", "provider_model", "base_url", "max_output_tokens",
		"api_key_ref", "offered_to_customers", "kumbha_enabled", "kumbha_priority",
		"updated_by", "created_at", "updated_at",
	})
	for _, m := range models {
		rows = rows.AddRow(
			m.ModelRoute, m.DisplayName, string(m.CostClass), m.Engine, m.ContextWindow,
			m.SupportsTools, m.SupportsVision, m.SupportsAudio,
			m.InputPricePerMillion, m.OutputPricePerMillion,
			m.VendorInputCostPerMillion, m.VendorOutputCostPerMillion,
			m.Enabled, providerOrNode(m.Provider), m.ProviderModel, m.BaseURL, 4096,
			m.APIKeyRef, m.OfferedToCustomers, m.KumbhaEnabled, m.KumbhaPriority,
			m.UpdatedBy, time.Now(), time.Now(),
		)
	}
	mock.ExpectQuery(`SELECT model_route, display_name, cost_class`).WillReturnRows(rows)
}

// captureLog redirects the standard logger for the duration of a test and
// returns a function reporting everything written to it so far.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return buf.String
}

// The split this whole file pins: a confidential-inference model must never
// be touched by the general external-health loop — it gets its own,
// independently-paced check (CheckConfidentialAttestation) — so widening
// the general interval for cost/noise reasons on ordinary vendor APIs can
// never also slow down how fast a broken attestation guarantee is caught.
func TestCheckExternalHealth_ExcludesConfidentialModels(t *testing.T) {
	catalog, nodeSvcs, mock, done := newTestServices(t)
	defer done()
	expectListModels(mock, []modelcatalog.Model{haiku, confidentialModel})

	g := New(catalog, nodeSvcs, nil)
	g.newExternal = func(m modelcatalog.Model, _ string) (inference.Provider, error) {
		return &fakeHealthCheckingProvider{fakeProvider: &fakeProvider{name: m.Engine}}, nil
	}

	g.CheckExternalHealth(context.Background())

	if got := g.Status(context.Background(), haiku).State; got != StateServing {
		t.Errorf("haiku status = %q, want serving", got)
	}
	if got := g.Status(context.Background(), confidentialModel).State; got != StateUnknown {
		t.Errorf("confidential model status = %q, want unknown — CheckExternalHealth must never touch it", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The inverse: CheckConfidentialAttestation only ever checks confidential
// models, leaving ordinary external models exactly as unchecked as before.
func TestCheckConfidentialAttestation_ChecksOnlyConfidentialModels(t *testing.T) {
	catalog, nodeSvcs, mock, done := newTestServices(t)
	defer done()
	expectListModels(mock, []modelcatalog.Model{haiku, confidentialModel})

	g := New(catalog, nodeSvcs, nil)
	g.newExternal = func(m modelcatalog.Model, _ string) (inference.Provider, error) {
		return &fakeHealthCheckingProvider{fakeProvider: &fakeProvider{name: m.Engine}}, nil
	}

	g.CheckConfidentialAttestation(context.Background())

	if got := g.Status(context.Background(), confidentialModel).State; got != StateServing {
		t.Errorf("confidential model status = %q, want serving", got)
	}
	if got := g.Status(context.Background(), haiku).State; got != StateUnknown {
		t.Errorf("haiku status = %q, want unknown — CheckConfidentialAttestation must never touch ordinary external models", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The alerting fix itself: a confidential model's attestation failing
// produces a distinct, grep-able ERROR line — this is what an operator's
// log-based alerting is meant to watch for.
func TestCheckConfidentialAttestation_LogsErrorOnAttestationFailure(t *testing.T) {
	catalog, nodeSvcs, mock, done := newTestServices(t)
	defer done()
	expectListModels(mock, []modelcatalog.Model{confidentialModel})
	logs := captureLog(t)

	g := New(catalog, nodeSvcs, nil)
	g.newExternal = func(m modelcatalog.Model, _ string) (inference.Provider, error) {
		return &fakeHealthCheckingProvider{
			fakeProvider: &fakeProvider{name: m.Engine},
			err:          errors.New("enclave is not currently attestation-verified"),
		}, nil
	}

	g.CheckConfidentialAttestation(context.Background())

	out := logs()
	if !strings.Contains(out, "ERROR") || !strings.Contains(out, confidentialModel.ModelRoute) {
		t.Errorf("log output = %q, want an ERROR line naming %q", out, confidentialModel.ModelRoute)
	}
	if got := g.Status(context.Background(), confidentialModel).State; got != StateUnhealthy {
		t.Errorf("confidential model status = %q, want unhealthy", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// An ordinary external model's health failure must stay exactly as quiet as
// before this change — the loud ERROR line is reserved for confidentiality
// breaking, not every routine vendor-API hiccup.
func TestCheckExternalHealth_DoesNotLogOnOrdinaryProviderFailure(t *testing.T) {
	catalog, nodeSvcs, mock, done := newTestServices(t)
	defer done()
	expectListModels(mock, []modelcatalog.Model{haiku})
	logs := captureLog(t)

	g := New(catalog, nodeSvcs, nil)
	g.newExternal = func(m modelcatalog.Model, _ string) (inference.Provider, error) {
		return &fakeHealthCheckingProvider{
			fakeProvider: &fakeProvider{name: m.Engine},
			err:          errors.New("upstream 503"),
		}, nil
	}

	g.CheckExternalHealth(context.Background())

	if out := logs(); strings.Contains(out, "ERROR") {
		t.Errorf("log output = %q, want no ERROR line for an ordinary external model's health failure", out)
	}
	if got := g.Status(context.Background(), haiku).State; got != StateUnhealthy {
		t.Errorf("haiku status = %q, want unhealthy", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
