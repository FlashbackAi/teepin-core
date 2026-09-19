// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/inferencegateway"
	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
)

type fakeGW struct {
	completeResp *inference.Response
	completeErr  error
	streamChunks []inference.Chunk
	streamErr    error
	gotReq       inference.Request
	gotAccount   string
}

func (f *fakeGW) Complete(_ context.Context, acct string, req inference.Request) (*inference.Response, error) {
	f.gotReq, f.gotAccount = req, acct
	return f.completeResp, f.completeErr
}

func (f *fakeGW) Stream(_ context.Context, acct string, req inference.Request, on func(inference.Chunk) error) error {
	f.gotReq, f.gotAccount = req, acct
	for _, ch := range f.streamChunks {
		if err := on(ch); err != nil {
			return err
		}
	}
	return f.streamErr
}

type fakeCatalog struct{ models map[string]modelcatalog.Model }

func (f fakeCatalog) GetModel(_ context.Context, route string) (*modelcatalog.Model, error) {
	m, ok := f.models[route]
	if !ok {
		return nil, modelcatalog.ErrNotFound
	}
	return &m, nil
}
func (f fakeCatalog) ListModels(context.Context) ([]modelcatalog.Model, error) {
	out := []modelcatalog.Model{}
	for _, m := range f.models {
		out = append(out, m)
	}
	return out, nil
}

type fakeUsage struct {
	records  []billing.UsageRecord
	consumed []float64
}

func (f *fakeUsage) RecordUsage(_ context.Context, r *billing.UsageRecord) error {
	r.ID = uuid.New()
	f.records = append(f.records, *r)
	return nil
}
func (f *fakeUsage) ConsumeCredit(_ context.Context, _, _ uuid.UUID, cost float64) (float64, error) {
	f.consumed = append(f.consumed, cost)
	return cost, nil
}

func testCatalog() fakeCatalog {
	return fakeCatalog{models: map[string]modelcatalog.Model{
		"teepin/a":        {ModelRoute: "teepin/a", DisplayName: "A", Engine: "mlx", Enabled: true, InputPricePerMillion: 2, OutputPricePerMillion: 10},
		"teepin/b":        {ModelRoute: "teepin/b", DisplayName: "B", Engine: "mlx", Enabled: true},
		"teepin/disabled": {ModelRoute: "teepin/disabled", Enabled: false},
	}}
}

type caller struct {
	viaKey  bool
	scopes  []string
	session bool
}

// serve runs one request through the handler with the given credential, the
// way the auth middleware would have published it.
func serve(t *testing.T, h *InferenceHandler, who caller, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(auth.AccountIDKey), uuid.MustParse("11111111-1111-1111-1111-111111111111"))
		c.Set(string(auth.ProjectIDKey), uuid.MustParse("22222222-2222-2222-2222-222222222222"))
		if who.viaKey {
			c.Set(string(auth.ViaAPIKeyKey), true)
			c.Set(string(auth.ScopesKey), who.scopes)
		}
		if who.session {
			c.Set(string(auth.SessionIDKey), uuid.New())
		}
		c.Next()
	})
	r.POST("/v1/chat/completions", h.ChatCompletions)
	r.GET("/v1/models", h.ListModels)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

const okBody = `{"model":"teepin/a","messages":[{"role":"user","content":"hi"}]}`

func okResponse() *inference.Response {
	return &inference.Response{
		Usage: inference.Usage{InputTokens: 1000, OutputTokens: 2000},
		Body:  json.RawMessage(`{"id":"x","choices":[{"message":{"role":"assistant","content":"hello"}}]}`),
	}
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Error apiError `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Error.Code
}

func TestChat_SuccessReturnsBackendBodyAndMetersTokens(t *testing.T) {
	gw, usage := &fakeGW{completeResp: okResponse()}, &fakeUsage{}
	h := NewInferenceHandler(gw, testCatalog(), usage)

	rec := serve(t, h, caller{}, "POST", "/v1/chat/completions", okBody)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"hello"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if gw.gotAccount != "11111111-1111-1111-1111-111111111111" || gw.gotReq.Model != "teepin/a" {
		t.Errorf("dispatched acct=%q req=%+v", gw.gotAccount, gw.gotReq)
	}

	if len(usage.records) != 2 {
		t.Fatalf("records = %d, want input+output lines", len(usage.records))
	}
	in, out := usage.records[0], usage.records[1]
	if in.ResourceType != "inference/input_tokens" || in.Quantity != 1000 || in.TotalCost != 0.002 {
		t.Errorf("input line = %+v (1000 tok at $2/M = 0.002)", in)
	}
	if out.ResourceType != "inference/output_tokens" || out.Quantity != 2000 || out.TotalCost != 0.02 {
		t.Errorf("output line = %+v (2000 tok at $10/M = 0.02)", out)
	}
	if in.SubjectID != "teepin/a" || in.SubjectType != "inference_model" || in.Unit != "tokens" {
		t.Errorf("subject/unit = %+v", in)
	}
	if len(usage.consumed) != 2 {
		t.Errorf("credit draws = %v, want one per non-zero line", usage.consumed)
	}
}

func TestChat_PassesThroughUnmodelledFieldsAndMaxTokens(t *testing.T) {
	gw := &fakeGW{completeResp: okResponse()}
	h := NewInferenceHandler(gw, testCatalog(), nil)
	body := `{"model":"teepin/a","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":77,"temperature":0.2,"tools":[]}`
	if rec := serve(t, h, caller{}, "POST", "/v1/chat/completions", body); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if gw.gotReq.MaxTokens != 77 {
		t.Errorf("MaxTokens = %d, want 77", gw.gotReq.MaxTokens)
	}
	if string(gw.gotReq.Extra["temperature"]) != "0.2" || gw.gotReq.Extra["tools"] == nil {
		t.Errorf("Extra = %v, want temperature and tools passed through", gw.gotReq.Extra)
	}
	if _, leaked := gw.gotReq.Extra["model"]; leaked {
		t.Error("modelled fields must not also appear in Extra")
	}
}

func TestChat_RequestValidation(t *testing.T) {
	h := NewInferenceHandler(&fakeGW{completeResp: okResponse()}, testCatalog(), nil)
	cases := map[string]string{
		"not json":       `nope`,
		"no model":       `{"messages":[{"role":"user","content":"x"}]}`,
		"no messages":    `{"model":"teepin/a"}`,
		"empty messages": `{"model":"teepin/a","messages":[]}`,
		"bad max_tokens": `{"model":"teepin/a","messages":[{"role":"user","content":"x"}],"max_tokens":-1}`,
		"bad stream":     `{"model":"teepin/a","messages":[{"role":"user","content":"x"}],"stream":"yes"}`,
	}
	for name, body := range cases {
		rec := serve(t, h, caller{}, "POST", "/v1/chat/completions", body)
		if rec.Code != 400 || errCode(t, rec) != "invalid_request" {
			t.Errorf("%s: status=%d code=%q, want 400 invalid_request", name, rec.Code, errCode(t, rec))
		}
	}
}

func TestChat_UnknownDisabledAndRestrictedModelsAreIndistinguishable(t *testing.T) {
	h := NewInferenceHandler(&fakeGW{completeResp: okResponse()}, testCatalog(), nil)
	key := caller{viaKey: true, scopes: []string{ScopeInferenceInvoke, scopeInferenceModel + "teepin/a"}}

	for name, model := range map[string]string{"unknown": "teepin/nope", "disabled": "teepin/disabled", "not permitted to this key": "teepin/b"} {
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"x"}]}`, model)
		rec := serve(t, h, key, "POST", "/v1/chat/completions", body)
		if rec.Code != 404 || errCode(t, rec) != "model_not_found" {
			t.Errorf("%s: status=%d code=%q, want 404 model_not_found", name, rec.Code, errCode(t, rec))
		}
	}
	// The permitted model still works for that key.
	if rec := serve(t, h, key, "POST", "/v1/chat/completions", okBody); rec.Code != 200 {
		t.Errorf("permitted model status = %d", rec.Code)
	}
}

func TestChat_APIKeyNeedsInferenceScope(t *testing.T) {
	h := NewInferenceHandler(&fakeGW{completeResp: okResponse()}, testCatalog(), nil)

	// A legacy key with only instance scopes must be refused, not silently allowed.
	rec := serve(t, h, caller{viaKey: true, scopes: []string{"instances:read", "instances:write"}}, "POST", "/v1/chat/completions", okBody)
	if rec.Code != 403 || errCode(t, rec) != "missing_scope" {
		t.Errorf("status=%d code=%q, want 403 missing_scope", rec.Code, errCode(t, rec))
	}
	// A signed-in user is role-based and unaffected by scopes.
	if rec := serve(t, h, caller{}, "POST", "/v1/chat/completions", okBody); rec.Code != 200 {
		t.Errorf("signed-in user status = %d", rec.Code)
	}
	// An agent session credential must never reach inference.
	if rec := serve(t, h, caller{session: true}, "POST", "/v1/chat/completions", okBody); rec.Code != 403 {
		t.Errorf("session credential status = %d, want 403", rec.Code)
	}
}

func TestChat_ErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
		code string
	}{
		{inferencegateway.ErrThrottled, 429, "rate_limited"},
		{fmt.Errorf("%w: nothing mounted", inference.ErrProviderUnavailable), 503, "model_unavailable"},
		{errors.New("boom"), 502, "upstream_error"},
	}
	for _, c := range cases {
		h := NewInferenceHandler(&fakeGW{completeErr: c.err}, testCatalog(), nil)
		rec := serve(t, h, caller{}, "POST", "/v1/chat/completions", okBody)
		if rec.Code != c.want || errCode(t, rec) != c.code {
			t.Errorf("%v -> %d/%q, want %d/%q", c.err, rec.Code, errCode(t, rec), c.want, c.code)
		}
		if strings.Contains(rec.Body.String(), "boom") {
			t.Error("an internal error message leaked to the client")
		}
	}
}

const streamBody = `{"model":"teepin/a","messages":[{"role":"user","content":"hi"}],"stream":true}`

func TestChat_StreamEmitsSSEFramesEndsWithDoneAndMetersFinalUsage(t *testing.T) {
	gw := &fakeGW{streamChunks: []inference.Chunk{
		{Data: json.RawMessage(`{"choices":[{"delta":{"content":"he"}}]}`)},
		{Data: json.RawMessage(`{"choices":[{"delta":{"content":"llo"}}]}`)},
		{Data: json.RawMessage(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20}}`), Usage: &inference.Usage{InputTokens: 10, OutputTokens: 20}},
		{Done: true},
	}}
	usage := &fakeUsage{}
	h := NewInferenceHandler(gw, testCatalog(), usage)

	rec := serve(t, h, caller{}, "POST", "/v1/chat/completions", streamBody)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data: {"choices":[{"delta":{"content":"he"}}]}`+"\n\n") {
		t.Errorf("first frame missing or malformed: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream must end with [DONE]: %q", body)
	}
	if !gw.gotReq.Stream {
		t.Error("the backend request was not marked as streaming")
	}
	if len(usage.records) != 2 || usage.records[0].Quantity != 10 || usage.records[1].Quantity != 20 {
		t.Errorf("final-chunk usage was not metered: %+v", usage.records)
	}
}

// A backend that fails before sending anything must surface as a normal HTTP
// error, not a broken 200 event stream.
func TestChat_StreamFailingBeforeFirstByteIsAnHTTPError(t *testing.T) {
	gw := &fakeGW{streamErr: fmt.Errorf("%w: down", inference.ErrProviderUnavailable)}
	rec := serve(t, NewInferenceHandler(gw, testCatalog(), nil), caller{}, "POST", "/v1/chat/completions", streamBody)
	if rec.Code != 503 || strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Errorf("status=%d type=%q, want a JSON 503", rec.Code, rec.Header().Get("Content-Type"))
	}
}

// A stream that dies midway still bills what was generated, and tells the
// client with an error event instead of a clean [DONE].
func TestChat_StreamFailingMidwayStillMetersAndSignalsError(t *testing.T) {
	gw := &fakeGW{
		streamChunks: []inference.Chunk{
			{Data: json.RawMessage(`{"choices":[{"delta":{"content":"par"}}]}`)},
			{Data: json.RawMessage(`{"usage":{}}`), Usage: &inference.Usage{InputTokens: 5, OutputTokens: 7}},
		},
		streamErr: errors.New("connection reset"),
	}
	usage := &fakeUsage{}
	rec := serve(t, NewInferenceHandler(gw, testCatalog(), usage), caller{}, "POST", "/v1/chat/completions", streamBody)

	body := rec.Body.String()
	if !strings.Contains(body, `"stream_error"`) || strings.Contains(body, "[DONE]") {
		t.Errorf("body = %q, want an error event and no [DONE]", body)
	}
	if strings.Contains(body, "connection reset") {
		t.Error("internal error text leaked into the stream")
	}
	if len(usage.records) != 2 {
		t.Errorf("tokens already generated were not metered: %+v", usage.records)
	}
}

func TestListModels_FiltersToEnabledAndPermitted(t *testing.T) {
	h := NewInferenceHandler(&fakeGW{}, testCatalog(), nil)

	var out struct {
		Object string      `json:"object"`
		Data   []modelView `json:"data"`
	}
	rec := serve(t, h, caller{}, "GET", "/v1/models", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || out.Object != "list" || len(out.Data) != 2 {
		t.Fatalf("status=%d list=%+v, want the 2 enabled models", rec.Code, out)
	}

	restricted := caller{viaKey: true, scopes: []string{ScopeInferenceInvoke, scopeInferenceModel + "teepin/a"}}
	rec = serve(t, h, restricted, "GET", "/v1/models", "")
	out.Data = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Data) != 1 || out.Data[0].ID != "teepin/a" || out.Data[0].Pricing.OutputPerMillion != 10 {
		t.Errorf("restricted key sees %+v, want only teepin/a with its price", out.Data)
	}

	if rec := serve(t, h, caller{viaKey: true, scopes: []string{"instances:read"}}, "GET", "/v1/models", ""); rec.Code != 403 {
		t.Errorf("key without the inference scope listed models: %d", rec.Code)
	}
}

func TestParseChatRequest_MaxCompletionTokensWins(t *testing.T) {
	req, err := parseChatRequest([]byte(`{"model":"m","messages":[{}],"max_tokens":5,"max_completion_tokens":9}`))
	if err != nil || req.maxTokens != 9 {
		t.Fatalf("maxTokens=%v err=%v, want 9", req, err)
	}
	req, _ = parseChatRequest([]byte(`{"model":"m","messages":[{}],"max_completion_tokens":9,"max_tokens":5}`))
	if req.maxTokens != 9 {
		t.Errorf("order of keys must not change the result: %d", req.maxTokens)
	}
}
