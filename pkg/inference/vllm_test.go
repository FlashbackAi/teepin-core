// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestVLLM(t *testing.T, handler http.HandlerFunc) (*VLLMProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := NewVLLM(VLLMConfig{
		BaseURL:       srv.URL,
		Model:         "qwen3-coder-30b",
		ContextWindow: 32768,
		SupportsTools: true,
	})
	return p, srv
}

func TestVLLM_Complete_ParsesUsageAndPassesBodyThrough(t *testing.T) {
	const upstreamBody = `{
		"id": "chatcmpl-abc",
		"model": "qwen3-coder-30b",
		"choices": [{"message": {"role": "assistant", "content": "hi"}}],
		"usage": {"prompt_tokens": 42, "completion_tokens": 17}
	}`

	p, _ := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upstreamBody))
	})

	resp, err := p.Complete(context.Background(), Request{
		Messages: rawMessages(`{"role":"user","content":"hi"}`),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if resp.Usage.InputTokens != 42 || resp.Usage.OutputTokens != 17 {
		t.Errorf("Usage = %+v, want {42 17}", resp.Usage)
	}
	if resp.Model != "qwen3-coder-30b" {
		t.Errorf("Model = %q, want qwen3-coder-30b", resp.Model)
	}
	// The response the gateway forwards to the caller must be the
	// upstream body VERBATIM — re-encoding it from a struct would drop
	// any field the gateway doesn't model (tool_calls, logprobs, ...) and
	// silently break tool calling for the agent consuming it.
	var gotBody, wantBody map[string]any
	if err := json.Unmarshal(resp.Body, &gotBody); err != nil {
		t.Fatalf("resp.Body is not valid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(upstreamBody), &wantBody); err != nil {
		t.Fatal(err)
	}
	gotChoices, _ := json.Marshal(gotBody["choices"])
	wantChoices, _ := json.Marshal(wantBody["choices"])
	if string(gotChoices) != string(wantChoices) {
		t.Errorf("choices were not passed through verbatim: got %s, want %s", gotChoices, wantChoices)
	}
}

func TestVLLM_Complete_RejectsBeforeDispatchWhenOverContext(t *testing.T) {
	called := false
	p, _ := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	big := strings.Repeat("a", 200_000)
	_, err := p.Complete(context.Background(), Request{
		Messages:  rawMessages(`{"role":"user","content":"` + big + `"}`),
		MaxTokens: 100,
	})
	if !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("got %v, want ErrContextTooLarge", err)
	}
	// The whole point of the pre-dispatch check is to never spend a round
	// trip (or, on a real backend, tokens) on a request that cannot fit.
	if called {
		t.Error("backend was called despite the request exceeding the context window")
	}
}

func TestVLLM_Complete_5xxIsUnavailable(t *testing.T) {
	p, _ := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"out of memory"}`, http.StatusInternalServerError)
	})
	_, err := p.Complete(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("got %v, want ErrProviderUnavailable for a 500", err)
	}
}

func TestVLLM_Complete_429IsUnavailableNotRejected(t *testing.T) {
	// A full queue is retryable; a malformed request is not. Conflating
	// them would make a gateway retry loop hammer a request that will
	// never succeed, or give up on one that would have on a second try.
	p, _ := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"queue full"}`, http.StatusTooManyRequests)
	})
	_, err := p.Complete(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("got %v, want ErrProviderUnavailable for 429", err)
	}
}

func TestVLLM_Complete_4xxIsRejected(t *testing.T) {
	p, _ := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid request: bad role"}`, http.StatusBadRequest)
	})
	_, err := p.Complete(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)})
	if !errors.Is(err, ErrProviderRejected) {
		t.Errorf("got %v, want ErrProviderRejected for a 400", err)
	}
	if !strings.Contains(err.Error(), "bad role") {
		t.Errorf("error %q does not carry the upstream detail — undiagnosable from a log", err)
	}
}

func TestVLLM_Complete_CancelledContextIsNotProviderUnavailable(t *testing.T) {
	// A client giving up must not look like a backend outage — the two
	// have opposite operational responses (do nothing vs page someone).
	block := make(chan struct{})
	p, srv := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		<-block
	})
	defer close(block)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := p.Complete(ctx, Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)})
	if err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	if errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("cancelled-context error was classified as ErrProviderUnavailable: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want context.DeadlineExceeded", err)
	}
	_ = srv // keep reference; httptest server closed via t.Cleanup
}

func TestVLLM_Encode_ModelAndMessagesCannotBeOverriddenByExtra(t *testing.T) {
	// A caller who smuggles their own "model" or "messages" into a
	// generic field map must not be able to redirect the request to a
	// different backend model — that would let a customer route around
	// pricing or capability checks tied to the model they were assigned.
	p, srv := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		if got["model"] != "qwen3-coder-30b" {
			t.Errorf("upstream model = %v, want the configured model (caller-supplied Extra must not override it)", got["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen3-coder-30b","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	_ = srv

	_, err := p.Complete(context.Background(), Request{
		Messages: rawMessages(`{"role":"user","content":"hi"}`),
		Extra: map[string]json.RawMessage{
			"model":       json.RawMessage(`"some-other-model-i-was-not-assigned"`),
			"temperature": json.RawMessage(`0.7`),
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestVLLM_Encode_PreservesUnknownExtraFields(t *testing.T) {
	// Tool calling, response_format, seed etc. are fields this package has
	// never heard of. Dropping any of them silently breaks a harness in a
	// way that looks like "the agent just isn't working" — among the
	// worst bugs to diagnose from outside the request/response boundary.
	p, srv := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		if _, ok := got["tools"]; !ok {
			t.Error("the 'tools' field the harness sent was dropped before reaching the backend")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen3-coder-30b","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	_ = srv

	_, err := p.Complete(context.Background(), Request{
		Messages: rawMessages(`{"role":"user","content":"hi"}`),
		Extra: map[string]json.RawMessage{
			"tools": json.RawMessage(`[{"type":"function","function":{"name":"read_file"}}]`),
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestVLLM_Stream_ForcesIncludeUsageAndDeliversChunks(t *testing.T) {
	p, _ := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		opts, _ := got["stream_options"].(map[string]any)
		if opts["include_usage"] != true {
			// Without this vLLM omits usage from the stream entirely and the
			// request becomes unbillable — the gateway must force it, never
			// trust the caller to have asked.
			t.Errorf("stream_options.include_usage = %v, want true (set by the gateway, not the caller)", opts["include_usage"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})

	var chunks []Chunk
	err := p.Stream(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)},
		func(c Chunk) error {
			chunks = append(chunks, c)
			return nil
		})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3 (content delta, usage delta, done)", len(chunks))
	}
	if chunks[0].Usage != nil {
		t.Errorf("first chunk carried usage early: %+v", chunks[0].Usage)
	}
	if chunks[1].Usage == nil || chunks[1].Usage.InputTokens != 5 || chunks[1].Usage.OutputTokens != 2 {
		t.Errorf("second chunk usage = %+v, want {5 2}", chunks[1].Usage)
	}
	if !chunks[2].Done {
		t.Error("final chunk was not marked Done")
	}
}

func TestVLLM_Stream_MissingTerminalMarkerIsUnavailable(t *testing.T) {
	// The connection closing without [DONE] means the backend was cut off
	// mid-generation — treating that as a clean success would let a
	// truncated, possibly-unbilled completion look complete.
	p, _ := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	})

	err := p.Stream(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)},
		func(Chunk) error { return nil })
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("got %v, want ErrProviderUnavailable", err)
	}
}

func TestVLLM_Stream_UpstreamErrorStatusIsClassified(t *testing.T) {
	// An error response on the streaming endpoint is a plain JSON body, not
	// SSE — must not be scanned as a stream of frames.
	p, _ := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid request: bad role"}`, http.StatusBadRequest)
	})

	err := p.Stream(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)},
		func(Chunk) error { return nil })
	if !errors.Is(err, ErrProviderRejected) {
		t.Errorf("got %v, want ErrProviderRejected", err)
	}
}

func TestVLLM_Stream_CallbackErrorPropagatesUnwrapped(t *testing.T) {
	// A caller aborting mid-stream (e.g. the client disconnected) must see
	// its own error back, not have Stream swallow or rewrap it as a
	// provider failure.
	sentinel := errors.New("client went away")
	p, _ := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	})

	err := p.Stream(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)},
		func(Chunk) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("got %v, want the callback's own sentinel error", err)
	}
}

func TestVLLM_Name_IsStableForBillingAttribution(t *testing.T) {
	p, _ := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {})
	if p.Name() != "vllm" {
		t.Errorf("Name() = %q, want %q — this value is written to billing.usage_records.provider", p.Name(), "vllm")
	}
}

func TestVLLM_Capabilities_ReportsOwnCostClass(t *testing.T) {
	p, _ := newTestVLLM(t, func(w http.ResponseWriter, r *http.Request) {})
	if got := p.Capabilities().CostClass; got != CostClassOwn {
		t.Errorf("CostClass = %q, want %q — the admin margin view keys off this", got, CostClassOwn)
	}
}

func TestDiscoverContextWindow_ReturnsMaxModelLenForTheConfiguredModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("requested path %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"Qwen/Qwen3.8-27B","object":"model","max_model_len":1010000}]}`))
	}))
	t.Cleanup(srv.Close)

	got, err := DiscoverContextWindow(context.Background(), srv.URL, "Qwen/Qwen3.8-27B", "")
	if err != nil {
		t.Fatalf("DiscoverContextWindow: %v", err)
	}
	if got != 1010000 {
		t.Errorf("got %d, want 1010000", got)
	}
}

func TestDiscoverContextWindow_SendsBearerTokenWhenAPIKeySet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
			t.Errorf("Authorization header = %q, want a bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m","max_model_len":8192}]}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := DiscoverContextWindow(context.Background(), srv.URL, "m", "secret-key"); err != nil {
		t.Fatalf("DiscoverContextWindow: %v", err)
	}
}

func TestDiscoverContextWindow_ErrorsWhenModelNotListed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"a-different-model","max_model_len":8192}]}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := DiscoverContextWindow(context.Background(), srv.URL, "the-configured-model", ""); err == nil {
		t.Error("expected an error when the configured model is not in the /v1/models response")
	}
}

func TestDiscoverContextWindow_ErrorsOnServerFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	// Callers must fall back to a configured/default value on error, not
	// treat this as fatal — a vLLM server that is briefly unreachable must
	// not stop the control plane from starting.
	if _, err := DiscoverContextWindow(context.Background(), srv.URL, "m", ""); err == nil {
		t.Error("expected an error on a 500 response")
	}
}
