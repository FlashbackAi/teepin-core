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

// newTestTinfoilConfidential builds a provider against an httptest server
// via a struct literal, bypassing NewTinfoilConfidential entirely — that
// constructor performs a real remote-attestation network call with no fake
// available (tinfoil.NewClientWithParams is a free function, not an
// interface), so the HTTP-request-shape behavior below is tested the same
// way it actually runs in production: against the *http.Client and baseURL
// a successful construction would have produced.
func newTestTinfoilConfidential(t *testing.T, handler http.HandlerFunc) (*TinfoilConfidentialProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := &TinfoilConfidentialProvider{
		http:    srv.Client(),
		baseURL: srv.URL,
		enclave: "test.enclave.invalid",
		model:   "qwen3-omni",
		apiKey:  "test-api-key",
		caps: Capabilities{
			ContextWindow: 32768,
			SupportsTools: true,
			SupportsJSON:  true,
			CostClass:     CostClassFrontier,
		},
	}
	return p, srv
}

func TestNormalizeEnclave_StripsSchemeAndTrailingSlash(t *testing.T) {
	cases := map[string]string{
		"router.inference.aptoslabs.com":          "router.inference.aptoslabs.com",
		"https://router.inference.aptoslabs.com":  "router.inference.aptoslabs.com",
		"http://router.inference.aptoslabs.com":   "router.inference.aptoslabs.com",
		"https://router.inference.aptoslabs.com/": "router.inference.aptoslabs.com",
	}
	for in, want := range cases {
		if got := normalizeEnclave(in); got != want {
			t.Errorf("normalizeEnclave(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewTinfoilConfidential_RejectsMissingEnclaveOrKeyWithoutNetworkCall(t *testing.T) {
	if _, err := NewTinfoilConfidential(TinfoilConfidentialConfig{APIKey: "k"}); err == nil {
		t.Error("expected an error for a missing enclave")
	}
	if _, err := NewTinfoilConfidential(TinfoilConfidentialConfig{Enclave: "e"}); err == nil {
		t.Error("expected an error for a missing API key")
	}
}

func TestTinfoilConfidential_Complete_ParsesUsageAndPassesBodyThrough(t *testing.T) {
	const upstreamBody = `{
		"id": "chatcmpl-abc",
		"model": "qwen3-omni",
		"choices": [{"message": {"role": "assistant", "content": "hi"}}],
		"usage": {"prompt_tokens": 42, "completion_tokens": 17}
	}`

	p, _ := newTestTinfoilConfidential(t, func(w http.ResponseWriter, r *http.Request) {
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
	if resp.Model != "qwen3-omni" {
		t.Errorf("Model = %q, want qwen3-omni", resp.Model)
	}

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

// Regression test for the bug caught before this provider ever shipped:
// tinfoil-go's option.WithAPIKey only takes effect through the typed
// openai.Client's own method-call pipeline, NOT on the raw *http.Client
// HTTPClient() exposes — since this provider bypasses that typed pipeline
// entirely, post() must attach Authorization itself or every request would
// go out unauthenticated against the real enclave.
func TestTinfoilConfidential_Complete_SendsBearerAuthorizationHeader(t *testing.T) {
	var gotAuth string
	p, _ := newTestTinfoilConfidential(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen3-omni","choices":[],"usage":{}}`))
	})

	if _, err := p.Complete(context.Background(), Request{
		Messages: rawMessages(`{"role":"user","content":"hi"}`),
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAuth != "Bearer test-api-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-api-key")
	}
}

func TestTinfoilConfidential_Complete_5xxIsUnavailable(t *testing.T) {
	p, _ := newTestTinfoilConfidential(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"enclave overloaded"}`))
	})

	_, err := p.Complete(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("err = %v, want ErrProviderUnavailable", err)
	}
}

func TestTinfoilConfidential_Complete_4xxIsRejected(t *testing.T) {
	p, _ := newTestTinfoilConfidential(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	})

	_, err := p.Complete(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)})
	if !errors.Is(err, ErrProviderRejected) {
		t.Errorf("err = %v, want ErrProviderRejected", err)
	}
}

func TestTinfoilConfidential_Encode_ModelAndMessagesCannotBeOverriddenByExtra(t *testing.T) {
	p := &TinfoilConfidentialProvider{model: "qwen3-omni"}
	body, err := p.encode(Request{
		Messages: rawMessages(`{"role":"user","content":"hi"}`),
		Extra: map[string]json.RawMessage{
			"model":    json.RawMessage(`"attacker-model"`),
			"messages": json.RawMessage(`[{"role":"user","content":"overridden"}]`),
			"top_p":    json.RawMessage(`0.9`),
		},
	}, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if string(out["model"]) != `"qwen3-omni"` {
		t.Errorf("model = %s, want the provider's own model, not Extra's", out["model"])
	}
	if string(out["top_p"]) != "0.9" {
		t.Errorf("top_p = %s, want preserved from Extra", out["top_p"])
	}
}

func TestTinfoilConfidential_Stream_ForcesIncludeUsageAndDeliversChunks(t *testing.T) {
	p, _ := newTestTinfoilConfidential(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if string(body["stream_options"]) != `{"include_usage":true}` {
			t.Errorf("stream_options = %s, want include_usage forced on", body["stream_options"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	var chunks []Chunk
	err := p.Stream(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)}, func(c Chunk) error {
		chunks = append(chunks, c)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	if !chunks[2].Done {
		t.Error("final chunk should be Done")
	}
	if chunks[1].Usage == nil || chunks[1].Usage.InputTokens != 5 {
		t.Errorf("chunks[1].Usage = %+v, want InputTokens 5", chunks[1].Usage)
	}
}

func TestTinfoilConfidential_Name_IsStableForBillingAttribution(t *testing.T) {
	p := &TinfoilConfidentialProvider{}
	if p.Name() != "tinfoil_confidential" {
		t.Errorf("Name() = %q, want a stable billing-ledger identifier", p.Name())
	}
}

func TestTinfoilConfidential_Capabilities_ReportsFrontierCostClass(t *testing.T) {
	p := &TinfoilConfidentialProvider{caps: Capabilities{CostClass: CostClassFrontier}}
	if p.Capabilities().CostClass != CostClassFrontier {
		t.Errorf("CostClass = %q, want frontier (a partner bills Teepin per token, not Teepin's own capacity)", p.Capabilities().CostClass)
	}
}

// A *tinfoil.Client can only be constructed via a real network attestation
// handshake (no fake available — see newTestTinfoilConfidential's own
// comment), so these exercise the nil-client guard rather than a real
// VerificationDocument. What matters here is the fail-safe shape: a missing
// client must error cleanly (ErrProviderUnavailable), never panic — a nil
// *tinfoil.Client dereferences on VerificationDocument() (confirmed by
// reading tinfoil-go's own source), so this guard is load-bearing, not
// defensive theater.
func TestTinfoilConfidential_CheckHealth_NilClientErrorsInsteadOfPanicking(t *testing.T) {
	p := &TinfoilConfidentialProvider{enclave: "test.enclave.invalid"}
	if err := p.CheckHealth(context.Background()); !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("err = %v, want ErrProviderUnavailable", err)
	}
}

func TestTinfoilConfidential_Attestation_NilClientErrorsInsteadOfPanicking(t *testing.T) {
	p := &TinfoilConfidentialProvider{enclave: "test.enclave.invalid"}
	if _, err := p.Attestation(); !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("err = %v, want ErrProviderUnavailable", err)
	}
}

func TestTinfoilConfidential_ImplementsOptionalCapabilityInterfaces(t *testing.T) {
	var p any = &TinfoilConfidentialProvider{}
	if _, ok := p.(HealthChecker); !ok {
		t.Error("TinfoilConfidentialProvider should implement HealthChecker")
	}
	if _, ok := p.(AttestationReporter); !ok {
		t.Error("TinfoilConfidentialProvider should implement AttestationReporter")
	}
}
