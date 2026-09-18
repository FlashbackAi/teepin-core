// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"bytes"
	"database/sql"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
)

func newModelCatalogHandlerMock(t *testing.T) (*ModelCatalogHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewModelCatalogHandler(modelcatalog.NewService(db)), mock, func() { db.Close() }
}

// jsonRequest is nodeHandlerRequest's counterpart for handlers that read a
// JSON body — path may include a query string (e.g. "...?model_route=x"),
// which c.Query reads straight off the constructed request's URL.
func jsonRequest(handler gin.HandlerFunc, method, path string, body []byte, params gin.Params) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = params
	handler(c)
	return w
}

func modelRows() []string {
	return []string{
		"model_route", "display_name", "cost_class", "engine", "context_window",
		"supports_tools", "supports_vision", "supports_audio",
		"input_price_per_million", "output_price_per_million",
		"vendor_input_cost_per_million", "vendor_output_cost_per_million",
		"enabled", "updated_by", "created_at", "updated_at",
	}
}

func TestRegisterModel_Success(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()

	mock.ExpectExec(`INSERT INTO inference\.models`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT model_route, display_name, cost_class`).
		WithArgs("teepin/qwen3-omni-7b").
		WillReturnRows(sqlmock.NewRows(modelRows()).AddRow(
			"teepin/qwen3-omni-7b", "Qwen3 Omni 7B", "own", "vllm-omni", 32768,
			true, true, true, 0.0, 0.0, nil, nil, true, "admin-api", time.Now(), time.Now(),
		))

	body := []byte(`{"model_route":"teepin/qwen3-omni-7b","display_name":"Qwen3 Omni 7B","cost_class":"own","engine":"vllm-omni","context_window":32768,"supports_tools":true,"supports_vision":true,"supports_audio":true,"enabled":true}`)
	w := jsonRequest(h.RegisterModel, "POST", "/v1/admin/inference/models", body, nil)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestRegisterModel_InvalidBody(t *testing.T) {
	h, _, done := newModelCatalogHandlerMock(t)
	defer done()

	// Missing required cost_class/engine — must be rejected before any query.
	body := []byte(`{"model_route":"teepin/x","display_name":"X"}`)
	w := jsonRequest(h.RegisterModel, "POST", "/v1/admin/inference/models", body, nil)

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestGetModel_MissingQueryParam(t *testing.T) {
	h, _, done := newModelCatalogHandlerMock(t)
	defer done()

	w := jsonRequest(h.GetModel, "GET", "/v1/admin/inference/models/one", nil, nil)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestGetModel_NotFound(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()

	mock.ExpectQuery(`SELECT model_route, display_name, cost_class`).
		WithArgs("missing/model").
		WillReturnError(sql.ErrNoRows)

	w := jsonRequest(h.GetModel, "GET", "/v1/admin/inference/models/one?model_route=missing/model", nil, nil)
	if w.Code != 404 {
		t.Fatalf("status = %d, want 404, body: %s", w.Code, w.Body.String())
	}
}

func TestSetPricing_MissingQueryParam(t *testing.T) {
	h, _, done := newModelCatalogHandlerMock(t)
	defer done()

	w := jsonRequest(h.SetPricing, "PUT", "/v1/admin/inference/models/pricing", []byte(`{}`), nil)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSetPricing_NotFound(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()

	mock.ExpectExec(`UPDATE inference\.models\s+SET input_price_per_million`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	body := []byte(`{"input_price_per_million":0.1,"output_price_per_million":0.3}`)
	w := jsonRequest(h.SetPricing, "PUT", "/v1/admin/inference/models/pricing?model_route=missing/model", body, nil)
	if w.Code != 404 {
		t.Fatalf("status = %d, want 404, body: %s", w.Code, w.Body.String())
	}
}

func TestDeleteModel_Success(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()

	mock.ExpectExec(`DELETE FROM inference\.models`).
		WithArgs("teepin/retired").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := jsonRequest(h.DeleteModel, "DELETE", "/v1/admin/inference/models?model_route=teepin/retired", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
}
