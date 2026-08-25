// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inference

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// VLLMProvider talks to a vLLM server over its OpenAI-compatible API.
//
// This is the backend Teepin owns, so it is the default route for the bulk
// of traffic — the high-volume, low-ambiguity work where the tokens (and
// therefore the margin) actually are. See KUMBHA-DESIGN.md's routing table
// for what deliberately does NOT come here.
type VLLMProvider struct {
	baseURL string
	model   string
	apiKey  string
	caps    Capabilities
	http    *http.Client
}

// Compile-time check. Cheap, and it fails the build rather than a customer
// request when the interface and implementation drift apart.
var _ Provider = (*VLLMProvider)(nil)

// VLLMConfig configures a vLLM backend.
type VLLMConfig struct {
	// BaseURL is the server root, WITHOUT the /v1 suffix
	// (e.g. "http://vllm.internal:8000").
	BaseURL string
	// Model is the identifier vLLM was launched with. Every request is
	// rewritten to this, because callers address routes ("teepin/fast"),
	// not backend models.
	Model string
	// APIKey is optional — vLLM only requires it when started with
	// --api-key. Sent as a bearer token when set.
	APIKey string
	// ContextWindow in tokens. Zero disables the pre-dispatch fit check
	// (see FitsContext) rather than guessing a limit.
	ContextWindow int
	// SupportsTools reflects whether the served model was launched with a
	// tool-call parser. Wrong-and-true here produces malformed tool calls
	// the agent cannot act on, so it is explicit rather than assumed.
	SupportsTools bool

	// HTTPClient overrides the default. Provided for tests; production
	// should take the tuned default below.
	HTTPClient *http.Client
}

// NewVLLM builds a vLLM-backed provider.
//
// The default HTTP client is tuned for this specific traffic shape rather
// than taken from http.DefaultClient, which has no timeout at all — a
// hung backend would otherwise pin a gateway goroutine (and the customer's
// request) indefinitely.
//
// No client-level Timeout is set on purpose: inference requests are
// legitimately long, and a blanket timeout would kill valid completions.
// The per-request context is the deadline authority, so callers can give a
// short-prompt request a short deadline and a long agentic step a long one.
// The transport-level timeouts below still bound connection setup and the
// wait for response HEADERS, which is what actually catches a dead backend.
//
// ResponseHeaderTimeout specifically was 60s until this was found live
// (2026-08-23): for a NON-streaming completion (Kumbha does not support
// streaming yet — see the "streaming is not yet available" 400 in
// kumbha_handlers.go), vLLM sends nothing at all, headers included, until
// the entire response has finished generating. A high-reasoning-effort
// agentic turn on a single home-hosted GPU legitimately took longer than
// that, so every retry hit the exact same 60s wall and the agent looked
// permanently stuck — this setting was quietly doing precisely what the
// paragraph above says it deliberately does not do. Raised generously
// rather than removed outright, so a genuinely dead backend is still
// caught eventually rather than hanging forever.
func NewVLLM(cfg VLLMConfig) *VLLMProvider {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				// The gateway is one process fronting one vLLM server, so
				// per-host capacity is what matters; Go's default of 2 idle
				// connections per host would force a fresh TCP+TLS handshake
				// on almost every concurrent request.
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: 10 * time.Minute,
				ExpectContinueTimeout: 1 * time.Second,
			},
		}
	}

	return &VLLMProvider{
		baseURL: trimTrailingSlash(cfg.BaseURL),
		model:   cfg.Model,
		apiKey:  cfg.APIKey,
		http:    client,
		caps: Capabilities{
			ContextWindow: cfg.ContextWindow,
			SupportsTools: cfg.SupportsTools,
			SupportsJSON:  true,
			CostClass:     CostClassOwn,
		},
	}
}

func (p *VLLMProvider) Name() string               { return "vllm" }
func (p *VLLMProvider) Capabilities() Capabilities { return p.caps }

// Complete performs a non-streaming completion.
func (p *VLLMProvider) Complete(ctx context.Context, req Request) (*Response, error) {
	if err := FitsContext(req, p.caps); err != nil {
		return nil, err
	}

	body, err := p.encode(req, false)
	if err != nil {
		return nil, err
	}

	httpResp, err := p.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: reading response: %v", ErrProviderUnavailable, err)
	}

	if err := classifyStatus(httpResp.StatusCode, raw); err != nil {
		return nil, err
	}

	// Only usage and model are parsed out; the body itself is handed back
	// untouched (see Response.Body).
	var parsed struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: response was not valid JSON: %v", ErrProviderUnavailable, err)
	}

	return &Response{
		Usage: Usage{
			InputTokens:  parsed.Usage.PromptTokens,
			OutputTokens: parsed.Usage.CompletionTokens,
		},
		Model: parsed.Model,
		Body:  raw,
	}, nil
}

// Stream performs a streaming completion over vLLM's OpenAI-compatible SSE
// endpoint, decoding each `data: {...}` frame and handing it to onChunk as
// it arrives.
//
// stream_options.include_usage is forced on by encode() (see its comment) —
// never left to the caller — because a streamed request without it is
// unbillable: vLLM omits the usage object entirely unless asked, and a
// harness that "forgot" to ask would otherwise get free inference. The
// final chunk before [DONE] carries that usage object; every chunk before
// it has Usage == nil.
func (p *VLLMProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk) error) error {
	if err := FitsContext(req, p.caps); err != nil {
		return err
	}

	body, err := p.encode(req, true)
	if err != nil {
		return err
	}

	httpResp, err := p.post(ctx, body)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// An error is still a plain JSON body, not SSE, on this path — read
		// it the same way Complete does rather than trying to scan it as a
		// stream of frames.
		raw, readErr := io.ReadAll(io.LimitReader(httpResp.Body, 8192))
		if readErr != nil {
			return fmt.Errorf("%w: reading error response: %v", ErrProviderUnavailable, readErr)
		}
		return classifyStatus(httpResp.StatusCode, raw)
	}

	scanner := bufio.NewScanner(httpResp.Body)
	// SSE frames carrying tool-call arguments or long completions can
	// exceed bufio's 64KB default token size; grow the buffer rather than
	// truncating a frame mid-JSON.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue // blank lines and SSE comments separate frames; skip
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			// A normal, billed completion — return here, not after the loop:
			// falling through would hit the "no terminal marker" error below.
			return onChunk(Chunk{Done: true})
		}

		var parsed struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			return fmt.Errorf("%w: malformed stream frame: %v", ErrProviderUnavailable, err)
		}

		chunk := Chunk{Data: json.RawMessage(data)}
		if parsed.Usage != nil {
			chunk.Usage = &Usage{
				InputTokens:  parsed.Usage.PromptTokens,
				OutputTokens: parsed.Usage.CompletionTokens,
			}
		}
		if err := onChunk(chunk); err != nil {
			return err // caller's own error (e.g. client disconnected) — not ours to wrap
		}
	}

	if err := scanner.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr // caller cancelled/timed out — not a backend failure
		}
		return fmt.Errorf("%w: reading stream: %v", ErrProviderUnavailable, err)
	}
	// Reached only if the connection closed cleanly without ever seeing
	// [DONE] (the DONE branch above returns directly) — the backend was cut
	// off mid-stream. Silently treating this as success would let a
	// truncated (and possibly unbilled, if usage never arrived) generation
	// look complete.
	return fmt.Errorf("%w: stream ended without a terminal [DONE] frame", ErrProviderUnavailable)
}

// encode builds the upstream request body: the caller's own fields, with
// the ones the gateway owns overwritten.
//
// Order matters. Extra is written FIRST so the gateway's own fields cannot
// be overridden by a caller who sends, say, their own "model" — which would
// let a customer route to a backend model they were not authorised for and
// bill it to the wrong rate.
func (p *VLLMProvider) encode(req Request, stream bool) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(req.Extra)+5)
	for k, v := range req.Extra {
		out[k] = v
	}

	model, err := json.Marshal(p.model)
	if err != nil {
		return nil, fmt.Errorf("encode model: %w", err)
	}
	out["model"] = model

	messages, err := json.Marshal(req.Messages)
	if err != nil {
		return nil, fmt.Errorf("encode messages: %w", err)
	}
	out["messages"] = messages

	if req.MaxTokens > 0 {
		v, err := json.Marshal(req.MaxTokens)
		if err != nil {
			return nil, fmt.Errorf("encode max_tokens: %w", err)
		}
		out["max_tokens"] = v
	}

	streamVal, err := json.Marshal(stream)
	if err != nil {
		return nil, fmt.Errorf("encode stream: %w", err)
	}
	out["stream"] = streamVal

	if stream {
		// Without this vLLM omits usage from the stream entirely and the
		// request becomes unbillable. Set by the gateway rather than trusted
		// from the caller — a client that omitted it would otherwise get
		// free inference.
		out["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	}

	return json.Marshal(out)
}

func (p *VLLMProvider) post(ctx context.Context, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.http.Do(httpReq)
	if err != nil {
		// A cancelled context is the caller giving up, not the backend
		// failing — reporting it as "provider unavailable" would fire
		// spurious alerts and, worse, make a disconnected client look like
		// an outage.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	return resp, nil
}

// classifyStatus turns an upstream status into one of the package's typed
// errors, so the HTTP layer maps it without inspecting strings.
//
// The split is retryable vs not: 5xx and 429 mean "try again" (429 because
// the queue is full, not because the request is wrong), while other 4xx mean
// the request itself is unacceptable and retrying changes nothing.
func classifyStatus(code int, body []byte) error {
	if code >= 200 && code < 300 {
		return nil
	}
	// Upstream error bodies are small and already structured; including one
	// verbatim is what makes a backend misconfiguration diagnosable from a
	// gateway log instead of requiring a reproduction.
	detail := string(bytes.TrimSpace(body))
	const maxDetail = 512
	if len(detail) > maxDetail {
		detail = detail[:maxDetail] + "…"
	}

	if code >= 500 || code == http.StatusTooManyRequests {
		return fmt.Errorf("%w: upstream status %d: %s", ErrProviderUnavailable, code, detail)
	}
	return fmt.Errorf("%w: upstream status %d: %s", ErrProviderRejected, code, detail)
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// DiscoverContextWindow queries vLLM's own OpenAI-compatible /v1/models
// endpoint for the served model's real max_model_len, rather than making
// an operator hardcode a guess that silently goes stale the moment the
// underlying model changes. Found live 2026-08-25: TEEPIN_VLLM_CONTEXT_WINDOW
// defaulted to a guessed 32768, which rejected (client-side, via
// FitsContext, before the request ever reached vLLM) a genuinely large but
// entirely servable build request — the actual deployed model reported
// max_model_len: 1010000. Callers should treat a non-nil error as "fall
// back to a configured/default value", not a fatal startup condition: a
// vLLM server that is briefly unreachable, or one whose /v1/models response
// shape differs, must not stop the control plane from starting.
func DiscoverContextWindow(ctx context.Context, baseURL, model, apiKey string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, trimTrailingSlash(baseURL)+"/v1/models", nil)
	if err != nil {
		return 0, fmt.Errorf("build /v1/models request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("query %s/v1/models: %w", baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("query %s/v1/models: status %d", baseURL, resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID          string `json:"id"`
			MaxModelLen int    `json:"max_model_len"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decode %s/v1/models response: %w", baseURL, err)
	}

	for _, m := range body.Data {
		if m.ID != model {
			continue
		}
		if m.MaxModelLen <= 0 {
			return 0, fmt.Errorf("model %q reported max_model_len %d", model, m.MaxModelLen)
		}
		return m.MaxModelLen, nil
	}
	return 0, fmt.Errorf("model %q not present in %s/v1/models response", model, baseURL)
}
