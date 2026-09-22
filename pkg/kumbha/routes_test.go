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

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

func newRouteStoreMock(t *testing.T) (*RouteStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewRouteStore(db), mock, func() { db.Close() }
}

// A route with no row is enabled — a fleet that never touches this from
// Control Center must behave exactly as it did before the table existed.
func TestRouteStore_IsEnabled_NoRowMeansEnabled(t *testing.T) {
	s, mock, done := newRouteStoreMock(t)
	defer done()

	mock.ExpectQuery(`SELECT enabled FROM billing\.kumbha_routes WHERE route_name = \$1`).
		WithArgs("teepin/fast").
		WillReturnError(sql.ErrNoRows)

	got, err := s.IsEnabled(context.Background(), "teepin/fast")
	if err != nil || !got {
		t.Fatalf("IsEnabled = %v, %v; want true, nil", got, err)
	}
}

func TestRouteStore_IsEnabled_ExplicitRowWins(t *testing.T) {
	s, mock, done := newRouteStoreMock(t)
	defer done()

	mock.ExpectQuery(`SELECT enabled FROM billing\.kumbha_routes`).
		WithArgs("teepin/deep").
		WillReturnRows(sqlmock.NewRows([]string{"enabled"}).AddRow(false))

	got, err := s.IsEnabled(context.Background(), "teepin/deep")
	if err != nil || got {
		t.Fatalf("IsEnabled = %v, %v; want false, nil", got, err)
	}
}

func TestRouteStore_SetEnabled_Upserts(t *testing.T) {
	s, mock, done := newRouteStoreMock(t)
	defer done()

	mock.ExpectExec(`INSERT INTO billing\.kumbha_routes .* ON CONFLICT \(route_name\) DO UPDATE`).
		WithArgs("teepin/deep", true, "admin-api").
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := s.SetEnabled(context.Background(), "teepin/deep", true, "admin-api"); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestRouteStore_Enabled_ListsExplicitRowsOnly(t *testing.T) {
	s, mock, done := newRouteStoreMock(t)
	defer done()

	mock.ExpectQuery(`SELECT route_name, enabled FROM billing\.kumbha_routes`).
		WillReturnRows(sqlmock.NewRows([]string{"route_name", "enabled"}).
			AddRow("teepin/deep", false))

	got, err := s.Enabled(context.Background())
	if err != nil || len(got) != 1 || got["teepin/deep"] != false {
		t.Fatalf("Enabled = %v, %v", got, err)
	}
}

// --- RouteMonitor -------------------------------------------------------

// healthyProvider/unhealthyProvider wrap the package's shared fakeProvider
// (router_test.go) with a CheckHealth — RouteMonitor only ever touches the
// inference.HealthChecker type assertion, never Complete/Stream, so the
// embedded fakeProvider's own behaviour is irrelevant here.
type healthyProvider struct{ *fakeProvider }

func (healthyProvider) CheckHealth(context.Context) error { return nil }

type unhealthyProvider struct {
	*fakeProvider
	healthErr error
}

func (p unhealthyProvider) CheckHealth(context.Context) error { return p.healthErr }

var _ inference.HealthChecker = healthyProvider{}
var _ inference.HealthChecker = unhealthyProvider{}

func TestRouteMonitor_StartsUnknownUntilChecked(t *testing.T) {
	m := NewRouteMonitor(map[string]Route{
		"teepin/fast": {Provider: healthyProvider{&fakeProvider{name: "vllm"}}},
	})
	st := m.Statuses()["teepin/fast"]
	if st.Status != HealthUnknown || !st.CheckedAt.IsZero() {
		t.Errorf("initial status = %+v, want unknown/never-checked", st)
	}
}

func TestRouteMonitor_CheckNow_HealthyAndUnhealthy(t *testing.T) {
	m := NewRouteMonitor(map[string]Route{
		"teepin/fast": {Provider: healthyProvider{&fakeProvider{name: "vllm"}}},
		"teepin/deep": {Provider: unhealthyProvider{&fakeProvider{name: "anthropic"}, errors.New("401 unauthorized")}},
	})
	m.CheckNow(context.Background())

	statuses := m.Statuses()
	if statuses["teepin/fast"].Status != HealthHealthy || statuses["teepin/fast"].CheckedAt.IsZero() {
		t.Errorf("teepin/fast = %+v, want healthy with a timestamp", statuses["teepin/fast"])
	}
	if statuses["teepin/deep"].Status != HealthUnhealthy || statuses["teepin/deep"].Error != "401 unauthorized" {
		t.Errorf("teepin/deep = %+v, want unhealthy with the error text", statuses["teepin/deep"])
	}
}

// A Provider that implements no health check must stay "unknown" forever,
// never "unhealthy" — silence is not a negative signal (see
// inference.HealthChecker's own doc comment on why this matters).
func TestRouteMonitor_ProviderWithNoHealthCheckStaysUnknown(t *testing.T) {
	m := NewRouteMonitor(map[string]Route{
		"teepin/fast": {Provider: &fakeProvider{name: "vllm"}},
	})
	m.CheckNow(context.Background())

	st := m.Statuses()["teepin/fast"]
	if st.Status != HealthUnknown {
		t.Errorf("status = %q, want unknown for a Provider with no CheckHealth", st.Status)
	}
	if st.CheckedAt.IsZero() {
		t.Error("CheckedAt was not updated even though a check round ran")
	}
}

func TestRouteMonitor_StatusesReturnsACopy(t *testing.T) {
	m := NewRouteMonitor(map[string]Route{"teepin/fast": {Provider: healthyProvider{&fakeProvider{name: "vllm"}}}})
	got := m.Statuses()
	got["teepin/fast"] = RouteHealth{Status: HealthUnhealthy}
	if m.Statuses()["teepin/fast"].Status == HealthUnhealthy {
		t.Error("mutating the returned map affected the monitor's own state")
	}
}

func TestRouteMonitor_StartStopsOnContextCancel(t *testing.T) {
	m := NewRouteMonitor(map[string]Route{"teepin/fast": {Provider: healthyProvider{&fakeProvider{name: "vllm"}}}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Start(ctx, time.Millisecond); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
}

// --- WithCandidates: per-candidate health ---

type fakeCandidateLister struct {
	all map[string][]RouteCandidate
	err error
}

func (f *fakeCandidateLister) ListAll(context.Context) (map[string][]RouteCandidate, error) {
	return f.all, f.err
}

func TestRouteMonitor_WithCandidates_ChecksEachCandidateIndependently(t *testing.T) {
	healthyID, unhealthyID := uuid.New(), uuid.New()
	lister := &fakeCandidateLister{all: map[string][]RouteCandidate{
		"teepin/fast": {
			{ID: healthyID, RouteName: "teepin/fast", Model: "model-a", Enabled: true},
			{ID: unhealthyID, RouteName: "teepin/fast", Model: "model-b", Enabled: true},
		},
	}}
	builder := &fakeCandidateBuilder{byModel: map[string]inference.Provider{
		"model-a": healthyProvider{&fakeProvider{name: "vllm"}},
		"model-b": unhealthyProvider{&fakeProvider{name: "vllm"}, errors.New("connection refused")},
	}}

	m := NewRouteMonitor(nil).WithCandidates(lister, builder)
	m.CheckNow(context.Background())

	statuses := m.CandidateStatuses()
	if statuses[healthyID].Status != HealthHealthy {
		t.Errorf("healthy candidate = %+v, want healthy", statuses[healthyID])
	}
	if statuses[unhealthyID].Status != HealthUnhealthy || statuses[unhealthyID].Error != "connection refused" {
		t.Errorf("unhealthy candidate = %+v, want unhealthy with the error text", statuses[unhealthyID])
	}
}

// A disabled candidate is reported unknown, never checked at all — an
// operator taking a backend offline for maintenance shouldn't see it
// flip to "unhealthy" just because nothing is polling it.
func TestRouteMonitor_WithCandidates_DisabledCandidateStaysUnknown(t *testing.T) {
	id := uuid.New()
	lister := &fakeCandidateLister{all: map[string][]RouteCandidate{
		"teepin/fast": {{ID: id, RouteName: "teepin/fast", Model: "model-a", Enabled: false}},
	}}
	builder := &fakeCandidateBuilder{byModel: map[string]inference.Provider{
		"model-a": unhealthyProvider{&fakeProvider{name: "vllm"}, errors.New("should never be called")},
	}}

	m := NewRouteMonitor(nil).WithCandidates(lister, builder)
	m.CheckNow(context.Background())

	if got := m.CandidateStatuses()[id].Status; got != HealthUnknown {
		t.Errorf("disabled candidate status = %q, want unknown (never checked)", got)
	}
}

// A candidate the factory cannot currently build (e.g. its secret can't be
// read right now) is reported unhealthy with that reason, not silently
// dropped from the listing.
func TestRouteMonitor_WithCandidates_BuildFailureIsUnhealthy(t *testing.T) {
	id := uuid.New()
	lister := &fakeCandidateLister{all: map[string][]RouteCandidate{
		"teepin/fast": {{ID: id, RouteName: "teepin/fast", Model: "missing-model", Enabled: true}},
	}}
	builder := &fakeCandidateBuilder{byModel: map[string]inference.Provider{}} // Build will error: not registered

	m := NewRouteMonitor(nil).WithCandidates(lister, builder)
	m.CheckNow(context.Background())

	st := m.CandidateStatuses()[id]
	if st.Status != HealthUnhealthy || st.Error == "" {
		t.Errorf("build-failure candidate = %+v, want unhealthy with a non-empty error", st)
	}
}
