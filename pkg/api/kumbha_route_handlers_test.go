// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
)

func kumbhaRouteRouter(h *KumbhaRouteHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/routes", h.List)
	r.PUT("/routes", h.SetEnabled)
	return r
}

func newRouteStoreDB(t *testing.T) (*kumbha.RouteStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	return kumbha.NewRouteStore(db), mock, func() { db.Close() }
}

func TestKumbhaRouteHandler_List(t *testing.T) {
	router := kumbha.NewRouter(map[string]kumbha.Route{
		"teepin/fast": {ProviderName: "vllm"},
		"teepin/deep": {ProviderName: "anthropic"},
	})
	store, mock, done := newRouteStoreDB(t)
	defer done()
	mock.ExpectQuery(`SELECT route_name, enabled FROM billing\.kumbha_routes`).
		WillReturnRows(sqlmock.NewRows([]string{"route_name", "enabled"}).AddRow("teepin/deep", false))

	monitor := kumbha.NewRouteMonitor(map[string]kumbha.Route{"teepin/fast": {}, "teepin/deep": {}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/routes", nil)
	kumbhaRouteRouter(NewKumbhaRouteHandler(router, store, monitor, nil, nil, "dev")).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Routes []kumbhaRouteView `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Routes) != 2 {
		t.Fatalf("routes = %+v, want 2", out.Routes)
	}
	byName := map[string]kumbhaRouteView{}
	for _, r := range out.Routes {
		byName[r.Name] = r
	}
	if !byName["teepin/fast"].Enabled {
		t.Error("teepin/fast (no row) should default to enabled")
	}
	if byName["teepin/deep"].Enabled {
		t.Error("teepin/deep (explicit false row) should be disabled")
	}
	// Neither route has ever been health-checked in this test — must read
	// as "unknown", not a bare zero value / missing field.
	for _, r := range out.Routes {
		if r.Health != string(kumbha.HealthUnknown) {
			t.Errorf("%s health = %q, want unknown", r.Name, r.Health)
		}
	}
}

func TestKumbhaRouteHandler_List_ReflectsLiveHealth(t *testing.T) {
	router := kumbha.NewRouter(map[string]kumbha.Route{"teepin/fast": {ProviderName: "vllm"}})
	store, mock, done := newRouteStoreDB(t)
	defer done()
	mock.ExpectQuery(`SELECT route_name, enabled FROM billing\.kumbha_routes`).
		WillReturnRows(sqlmock.NewRows([]string{"route_name", "enabled"}))

	monitor := kumbha.NewRouteMonitor(map[string]kumbha.Route{
		"teepin/fast": {Provider: unhealthyTestProvider{}},
	})
	monitor.CheckNow(context.Background())

	rec := httptest.NewRecorder()
	kumbhaRouteRouter(NewKumbhaRouteHandler(router, store, monitor, nil, nil, "dev")).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/routes", nil))

	var out struct {
		Routes []kumbhaRouteView `json:"routes"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Routes) != 1 || out.Routes[0].Health != string(kumbha.HealthUnhealthy) || out.Routes[0].HealthErr != "backend down" {
		t.Errorf("routes = %+v", out.Routes)
	}
	if out.Routes[0].CheckedAt == "" {
		t.Error("checked_at was not populated after a real check ran")
	}
}

func TestKumbhaRouteHandler_SetEnabled(t *testing.T) {
	router := kumbha.NewRouter(map[string]kumbha.Route{"teepin/fast": {ProviderName: "vllm"}})
	store, mock, done := newRouteStoreDB(t)
	defer done()
	mock.ExpectExec(`INSERT INTO billing\.kumbha_routes`).
		WithArgs("teepin/fast", false, "admin-api").
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/routes?route=teepin%2Ffast", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	kumbhaRouteRouter(NewKumbhaRouteHandler(router, store, kumbha.NewRouteMonitor(nil), nil, nil, "dev")).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// An unrecognised route name (typo, or a route retired from main.go) is
// refused rather than silently written to a table nothing will ever read —
// it would otherwise sit there forever with no visible effect.
func TestKumbhaRouteHandler_SetEnabled_RejectsUnknownRoute(t *testing.T) {
	router := kumbha.NewRouter(map[string]kumbha.Route{"teepin/fast": {ProviderName: "vllm"}})
	store, _, done := newRouteStoreDB(t)
	defer done()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/routes?route=teepin%2Fnonexistent", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	kumbhaRouteRouter(NewKumbhaRouteHandler(router, store, kumbha.NewRouteMonitor(nil), nil, nil, "dev")).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unconfigured route name", rec.Code)
	}
}

func TestKumbhaRouteHandler_SetEnabled_RejectsMissingBody(t *testing.T) {
	router := kumbha.NewRouter(map[string]kumbha.Route{"teepin/fast": {ProviderName: "vllm"}})
	store, _, done := newRouteStoreDB(t)
	defer done()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/routes?route=teepin%2Ffast", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	kumbhaRouteRouter(NewKumbhaRouteHandler(router, store, kumbha.NewRouteMonitor(nil), nil, nil, "dev")).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a missing enabled field", rec.Code)
	}
}

// unhealthyTestProvider is a minimal inference.Provider + HealthChecker
// used only to prove List reflects the monitor's live cache.
type unhealthyTestProvider struct{}

func (unhealthyTestProvider) Name() string { return "vllm" }
func (unhealthyTestProvider) Complete(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("not implemented")
}
func (unhealthyTestProvider) Stream(context.Context, inference.Request, func(inference.Chunk) error) error {
	return errors.New("not implemented")
}
func (unhealthyTestProvider) Capabilities() inference.Capabilities { return inference.Capabilities{} }
func (unhealthyTestProvider) CheckHealth(context.Context) error    { return errors.New("backend down") }

var _ inference.Provider = unhealthyTestProvider{}
var _ inference.HealthChecker = unhealthyTestProvider{}
