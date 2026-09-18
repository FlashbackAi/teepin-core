// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

type fakeCompleter struct {
	resp *inference.Response
	err  error
	got  inference.Request
	acct string
}

func (f *fakeCompleter) Complete(_ context.Context, accountID string, req inference.Request) (*inference.Response, error) {
	f.got, f.acct = req, accountID
	return f.resp, f.err
}

func playgroundPost(t *testing.T, h *InferencePlaygroundHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/chat", h.Chat)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/chat", bytes.NewBufferString(body)))
	return rec
}

func TestPlaygroundChat_ReturnsAssistantText(t *testing.T) {
	f := &fakeCompleter{resp: &inference.Response{
		Model: "org/model",
		Usage: inference.Usage{InputTokens: 5, OutputTokens: 7},
		Body:  json.RawMessage(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`),
	}}
	rec := playgroundPost(t, NewInferencePlaygroundHandler(f), `{"model_route":"teepin/k2","prompt":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var out playgroundResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Content != "hello" || out.InputTokens != 5 || out.OutputTokens != 7 || out.Model != "org/model" {
		t.Errorf("response = %+v", out)
	}
	if f.acct != playgroundAccount || f.got.Model != "teepin/k2" || f.got.MaxTokens != 256 {
		t.Errorf("dispatched as acct=%q req=%+v", f.acct, f.got)
	}
}

func TestPlaygroundChat_ErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("%w: nothing mounted", inference.ErrProviderUnavailable), http.StatusServiceUnavailable},
		{fmt.Errorf("boom"), http.StatusBadGateway},
	}
	for _, c := range cases {
		rec := playgroundPost(t, NewInferencePlaygroundHandler(&fakeCompleter{err: c.err}), `{"model_route":"m","prompt":"p"}`)
		if rec.Code != c.want {
			t.Errorf("err %v -> %d, want %d", c.err, rec.Code, c.want)
		}
	}
}

func TestPlaygroundChat_RejectsMissingFields(t *testing.T) {
	h := NewInferencePlaygroundHandler(&fakeCompleter{})
	if rec := playgroundPost(t, h, `{"model_route":"m"}`); rec.Code != 400 {
		t.Errorf("missing prompt -> %d, want 400", rec.Code)
	}
	if rec := playgroundPost(t, h, `{"model_route":"m","prompt":"   "}`); rec.Code != 400 {
		t.Errorf("blank prompt -> %d, want 400", rec.Code)
	}
}

func TestAssistantText_FallsBackToRawBody(t *testing.T) {
	if got := assistantText(json.RawMessage(`{"unexpected":1}`)); got != `{"unexpected":1}` {
		t.Errorf("got %q", got)
	}
}
