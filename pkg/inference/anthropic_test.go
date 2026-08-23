// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inference

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestAnthropic(t *testing.T, handler http.HandlerFunc) (*AnthropicProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := NewAnthropic(AnthropicConfig{
		Model:         "claude-opus-5",
		APIKey:        "test-key",
		BaseURL:       srv.URL,
		ContextWindow: 200000,
	})
	return p, srv
}

func TestAnthropic_Complete_TranslatesResponseToOpenAIShape(t *testing.T) {
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"id": "msg_abc", "type": "message", "role": "assistant",
			"content": [{"type": "text", "text": "hello there"}],
			"model": "claude-opus-5-20260101",
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 12, "output_tokens": 4}
		}`))
	})

	resp, err := p.Complete(context.Background(), Request{
		Messages: rawMessages(`{"role":"user","content":"hi"}`),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 4 {
		t.Errorf("Usage = %+v, want {12 4}", resp.Usage)
	}
	// The backend model string IS captured (for admin/margin attribution
	// — see Response.Model's doc comment) even though it never reaches
	// the customer-facing body's own semantics beyond being informational.
	if resp.Model != "claude-opus-5-20260101" {
		t.Errorf("Model = %q", resp.Model)
	}

	// Response.Body must be OpenAI-chat-completion-shaped — this is the
	// entire point of the adapter: a harness reading resp.Body never
	// needs to know Anthropic served this request.
	var body struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("resp.Body is not valid OpenAI-shaped JSON: %v", err)
	}
	if len(body.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(body.Choices))
	}
	if body.Choices[0].Message.Content != "hello there" {
		t.Errorf("message content = %q, want %q", body.Choices[0].Message.Content, "hello there")
	}
	if body.Choices[0].Message.Role != "assistant" {
		t.Errorf("message role = %q, want assistant", body.Choices[0].Message.Role)
	}
	if body.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop (mapped from end_turn)", body.Choices[0].FinishReason)
	}
	if body.Usage.PromptTokens != 12 || body.Usage.CompletionTokens != 4 {
		t.Errorf("body usage = %+v, want {12 4}", body.Usage)
	}
}

func TestAnthropic_Complete_SplitsSystemRoleFromMessages(t *testing.T) {
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		system, _ := got["system"].([]any)
		if len(system) != 1 {
			t.Fatalf("got %d system blocks, want 1 (system role must not appear in messages)", len(system))
		}
		block := system[0].(map[string]any)
		if block["text"] != "be concise" {
			t.Errorf("system text = %v, want %q", block["text"], "be concise")
		}
		messages, _ := got["messages"].([]any)
		if len(messages) != 1 {
			t.Fatalf("got %d messages, want 1 (the system-role message must not be one of them)", len(messages))
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	})

	_, err := p.Complete(context.Background(), Request{
		Messages: rawMessages(
			`{"role":"system","content":"be concise"}`,
			`{"role":"user","content":"hi"}`,
		),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestAnthropic_Complete_UsesDefaultMaxTokensWhenUnset(t *testing.T) {
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got["max_tokens"] != float64(defaultAnthropicMaxTokens) {
			t.Errorf("max_tokens = %v, want the configured default %d", got["max_tokens"], defaultAnthropicMaxTokens)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	})

	_, err := p.Complete(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestAnthropic_Complete_RejectsBeforeDispatchWhenOverContext(t *testing.T) {
	called := false
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	big := make([]byte, 900000) // ~225k estimated tokens, over the 200k window
	for i := range big {
		big[i] = 'a'
	}
	_, err := p.Complete(context.Background(), Request{
		Messages: rawMessages(`{"role":"user","content":"` + string(big) + `"}`),
	})
	if !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("got %v, want ErrContextTooLarge", err)
	}
	if called {
		t.Error("backend was called despite the request exceeding the context window")
	}
}

func TestAnthropic_Complete_5xxIsUnavailable(t *testing.T) {
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"internal"}}`))
	})
	_, err := p.Complete(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("got %v, want ErrProviderUnavailable for a 500", err)
	}
}

func TestAnthropic_Complete_4xxIsRejected(t *testing.T) {
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`))
	})
	_, err := p.Complete(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)})
	if !errors.Is(err, ErrProviderRejected) {
		t.Errorf("got %v, want ErrProviderRejected for a 400", err)
	}
}

func TestAnthropic_Stream_NotImplementedIsExplicit(t *testing.T) {
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("Stream must not reach the backend before it is implemented")
	})
	err := p.Stream(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)},
		func(Chunk) error { return nil })
	if !errors.Is(err, ErrProviderRejected) {
		t.Errorf("got %v, want a typed error", err)
	}
}

func TestAnthropic_Name_IsStableForBillingAttribution(t *testing.T) {
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {})
	if p.Name() != "anthropic" {
		t.Errorf("Name() = %q, want %q", p.Name(), "anthropic")
	}
}

func TestAnthropic_Capabilities_ReportsFrontierCostClass(t *testing.T) {
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {})
	if got := p.Capabilities().CostClass; got != CostClassFrontier {
		t.Errorf("CostClass = %q, want %q — routing/admin margin views key off this", got, CostClassFrontier)
	}
}

func TestExtractText_HandlesBareStringAndMultimodalArray(t *testing.T) {
	got, err := extractText(json.RawMessage(`"hello"`))
	if err != nil || got != "hello" {
		t.Errorf("bare string: got %q, %v", got, err)
	}

	got, err = extractText(json.RawMessage(`[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"b"}]`))
	if err != nil {
		t.Fatalf("array content: %v", err)
	}
	if got != "ab" {
		t.Errorf("array content: got %q, want %q (non-text parts skipped)", got, "ab")
	}
}
