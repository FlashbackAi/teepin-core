// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/inferencegateway"
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
		"enabled", "provider", "provider_model", "base_url", "max_output_tokens",
		"api_key_ref", "offered_to_customers", "kumbha_enabled", "kumbha_priority", "kumbha_alias",
		"updated_by", "created_at", "updated_at",
	}
}

// expectModelRow primes one GetModel lookup of route, stored with the given
// provider and API key reference.
func expectModelRow(mock sqlmock.Sqlmock, route, provider, apiKeyRef string) {
	mock.ExpectQuery(`SELECT model_route, display_name, cost_class`).
		WithArgs(route).
		WillReturnRows(sqlmock.NewRows(modelRows()).AddRow(
			route, "Model", "frontier", "anthropic", 200000,
			true, false, false, 0.0, 0.0, nil, nil,
			true, provider, "claude-haiku-4-5-20251001", "", 4096,
			apiKeyRef, false, true, 0, "teepin/fast",
			"admin-api", time.Now(), time.Now(),
		))
}

// fakeModelSecrets records Secrets Manager writes and deletes.
type fakeModelSecrets struct {
	puts    map[string]string
	deleted []string
}

func (f *fakeModelSecrets) Get(context.Context, string) (string, bool, error) { return "", false, nil }
func (f *fakeModelSecrets) Put(_ context.Context, id, value string) error {
	if f.puts == nil {
		f.puts = map[string]string{}
	}
	f.puts[id] = value
	return nil
}
func (f *fakeModelSecrets) Delete(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func TestRegisterModel_Success(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()

	mock.ExpectExec(`INSERT INTO inference\.models`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectModelRow(mock, "teepin/qwen3-omni-7b", "node", "")

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

// Registering an external model with its key and availability in one call:
// the key goes to Secrets Manager under a fresh inference-model-key- name
// (never into the database), and the model is placed on Kumbha without
// being offered to customers.
func TestRegisterModel_ExternalModelStoresKeyAndAvailability(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()
	secrets := &fakeModelSecrets{}
	h.WithAPIKeys(secrets, "dev")

	mock.ExpectExec(`INSERT INTO inference\.models`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	off, on := false, true
	mock.ExpectExec(`SET offered_to_customers = COALESCE`).
		WithArgs(&off, &on, nil, nil, "admin-api", "anthropic/claude-haiku-4-5").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectModelRow(mock, "anthropic/claude-haiku-4-5", "anthropic", "") // storeAPIKey's lookup: no key yet
	mock.ExpectExec(`SET api_key_ref = NULLIF`).
		WithArgs(sqlmock.AnyArg(), "admin-api", "anthropic/claude-haiku-4-5").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectModelRow(mock, "anthropic/claude-haiku-4-5", "anthropic", "inference-model-key-x")

	body := []byte(`{"model_route":"anthropic/claude-haiku-4-5","display_name":"Claude Haiku 4.5","cost_class":"frontier","engine":"anthropic",` +
		`"provider":"anthropic","provider_model":"claude-haiku-4-5-20251001","api_key":"sk-ant-test","enabled":true,` +
		`"offered_to_customers":false,"kumbha_enabled":true}`)
	w := jsonRequest(h.RegisterModel, "POST", "/v1/admin/inference/models", body, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if len(secrets.puts) != 1 {
		t.Fatalf("got %d secret writes, want 1", len(secrets.puts))
	}
	for id, v := range secrets.puts {
		if !strings.HasPrefix(id, "teepin/dev/inference-model-key-") || v != "sk-ant-test" {
			t.Errorf("secret %q = %q", id, v)
		}
	}
	if strings.Contains(w.Body.String(), "sk-ant-test") {
		t.Error("the API key was echoed back in the response")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// Rotating a key rewrites the model's existing secret rather than minting a
// second one.
func TestRegisterModel_KeyRotationReusesExistingSecret(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()
	secrets := &fakeModelSecrets{}
	h.WithAPIKeys(secrets, "dev")

	mock.ExpectExec(`INSERT INTO inference\.models`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectModelRow(mock, "anthropic/claude-haiku-4-5", "anthropic", "kumbha-candidate-abc-api-key")
	mock.ExpectExec(`SET api_key_ref = NULLIF`).
		WithArgs("kumbha-candidate-abc-api-key", "admin-api", "anthropic/claude-haiku-4-5").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectModelRow(mock, "anthropic/claude-haiku-4-5", "anthropic", "kumbha-candidate-abc-api-key")

	body := []byte(`{"model_route":"anthropic/claude-haiku-4-5","display_name":"Haiku","cost_class":"frontier","engine":"anthropic",` +
		`"provider":"anthropic","provider_model":"claude-haiku-4-5-20251001","api_key":"sk-new","enabled":true}`)
	w := jsonRequest(h.RegisterModel, "POST", "/v1/admin/inference/models", body, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if secrets.puts["teepin/dev/kumbha-candidate-abc-api-key"] != "sk-new" {
		t.Errorf("secret writes = %v, want the existing secret rewritten", secrets.puts)
	}
}

// Without a secrets backend the model still saves; the unsaved key is
// reported rather than silently dropped.
func TestRegisterModel_KeyWithoutSecretsBackendWarns(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()

	mock.ExpectExec(`INSERT INTO inference\.models`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectModelRow(mock, "anthropic/claude-haiku-4-5", "anthropic", "")

	body := []byte(`{"model_route":"anthropic/claude-haiku-4-5","display_name":"Haiku","cost_class":"frontier","engine":"anthropic",` +
		`"provider":"anthropic","provider_model":"claude-haiku-4-5-20251001","api_key":"sk-x","enabled":true}`)
	w := jsonRequest(h.RegisterModel, "POST", "/v1/admin/inference/models", body, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Warning string `json:"warning"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Warning == "" {
		t.Error("an API key that could not be stored must be reported")
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

func TestSetAvailability_RequiresAField(t *testing.T) {
	h, _, done := newModelCatalogHandlerMock(t)
	defer done()

	w := jsonRequest(h.SetAvailability, "PUT", "/v1/admin/inference/models/availability?model_route=teepin/a", []byte(`{}`), nil)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 for an update that changes nothing", w.Code)
	}
}

func TestSetAvailability_UpdatesOnlyWhatWasSent(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()

	priority := 2
	mock.ExpectExec(`SET offered_to_customers = COALESCE`).
		WithArgs(nil, nil, &priority, nil, "admin-api", "teepin/a").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := jsonRequest(h.SetAvailability, "PUT", "/v1/admin/inference/models/availability?model_route=teepin/a", []byte(`{"kumbha_priority":2}`), nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// Deleting a model also removes its stored API key.
func TestDeleteModel_RemovesItsAPIKey(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()
	secrets := &fakeModelSecrets{}
	h.WithAPIKeys(secrets, "dev")

	expectModelRow(mock, "anthropic/claude-haiku-4-5", "anthropic", "inference-model-key-1")
	mock.ExpectExec(`DELETE FROM inference\.models`).
		WithArgs("anthropic/claude-haiku-4-5").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := jsonRequest(h.DeleteModel, "DELETE", "/v1/admin/inference/models?model_route=anthropic/claude-haiku-4-5", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if len(secrets.deleted) != 1 || secrets.deleted[0] != "teepin/dev/inference-model-key-1" {
		t.Errorf("deleted secrets = %v", secrets.deleted)
	}
}

func TestDeleteModel_NotFound(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()

	mock.ExpectQuery(`SELECT model_route, display_name, cost_class`).
		WithArgs("teepin/retired").
		WillReturnError(sql.ErrNoRows)

	w := jsonRequest(h.DeleteModel, "DELETE", "/v1/admin/inference/models?model_route=teepin/retired", nil, nil)
	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

type fixedStatus struct{ st inferencegateway.ModelStatus }

func (f fixedStatus) Status(context.Context, modelcatalog.Model) inferencegateway.ModelStatus {
	return f.st
}

// The listing carries each enabled model's live status, so an operator can
// see at a glance which models can actually be served right now.
func TestListModels_IncludesLiveStatus(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()
	h.WithModelStatus(fixedStatus{inferencegateway.ModelStatus{State: inferencegateway.StateNoBackend}})

	mock.ExpectQuery(`SELECT model_route, display_name, cost_class`).
		WillReturnRows(sqlmock.NewRows(modelRows()).AddRow(
			"teepin/qwen3-30b-a3b", "Qwen", "own", "mlx", 8000,
			false, false, false, 0.0, 0.0, nil, nil,
			true, "node", "", "", 4096, "", true, false, 0, "teepin/fast",
			"op", time.Now(), time.Now(),
		))

	w := jsonRequest(h.ListModels, "GET", "/v1/admin/inference/models", nil, nil)
	var resp struct {
		Models []struct {
			ModelRoute string `json:"model_route"`
			Status     struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(resp.Models) != 1 || resp.Models[0].Status.State != "no_backend" {
		t.Errorf("models = %+v, want one with status no_backend", resp.Models)
	}
}
