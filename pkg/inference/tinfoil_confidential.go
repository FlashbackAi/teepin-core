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
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go/v3/option"
	tinfoil "github.com/tinfoilsh/tinfoil-go"
)

// aptosConfidentialInferenceRepo is the attestation repo backing Teepin's one
// confidential-inference partner (see flashback-instructions.md at the repo
// root — "flashback" is only a codename, ignore it). Hardcoded, not an admin
// field: the repo identifies which signed release's measurement to trust,
// which is inherent to Aptos's specific hosted router, not something an
// operator should be able to repoint per model registration without also
// changing which vendor's hardware they are trusting. A second confidential-
// inference partner with a different repo would be a second Provider
// constant, mirroring how ProviderAnthropic/ProviderOpenAICompatible are
// already vendor/shape-specific rather than generic.
const aptosConfidentialInferenceRepo = "aptos-labs/confidential-inference-router"

// tinfoilClientTimeout bounds NewTinfoilConfidential's one-time client
// construction. tinfoil.NewClientWithParams performs a real remote
// attestation handshake against the enclave (not just a TCP dial) and takes
// no context of its own to bound that wait — confirmed by reading its
// signature directly (github.com/tinfoilsh/tinfoil-go@v0.15.7,
// tinfoil_client.go). Without a bound here, a slow or wedged enclave would
// hang whatever request triggered a cache-miss rebuild in
// externalProviderFor indefinitely, rather than surfacing a clear error.
const tinfoilClientTimeout = 30 * time.Second

// TinfoilConfidentialProvider talks to a partner-hosted, hardware-attested
// confidential-inference enclave via the Tinfoil client
// (github.com/tinfoilsh/tinfoil-go) — verified attestation and encryption to
// the attested key (EHBP), not plain TLS. See
// [[confidential-inference-hosting-model]]: the partner hosts the enclave,
// Teepin is the front end (Kumbha, console, billing), the same leased-
// capacity shape Teepin already uses for GPU/CPU compute.
//
// Deliberately built on the SAME raw-HTTP request/response shape as
// VLLMProvider rather than the typed openai-go params tinfoil-go also
// exposes: tinfoil.Client embeds *openai.Client, but also exposes
// HTTPClient() — a plain *http.Client already bound to the verified,
// attested transport, explicitly documented as safe "for secure, direct
// HTTP requests to the enclave." Using it keeps this provider's body
// construction (Extra-first merge, raw JSON passthrough for Response.Body)
// identical to VLLMProvider's already-established, tested pattern instead of
// re-deriving message translation against openai-go's typed union types.
type TinfoilConfidentialProvider struct {
	http    *http.Client
	baseURL string // "https://<enclave>/v1" — no trailing slash
	enclave string
	model   string
	// apiKey is set on every request by post() directly, NOT left to
	// tinfoil-go's own option.WithAPIKey: that option only takes effect
	// through the typed openai.Client's own method-call pipeline
	// (confirmed by reading openai-go/v3's option.WithAPIKey — it sets a
	// client-side "setting" applied at request-construction time, not
	// baked into the transport HTTPClient() returns). Since this provider
	// bypasses that typed pipeline entirely to keep the same raw-HTTP body
	// shape VLLMProvider already uses, it must attach Authorization itself
	// or every completion request would go out unauthenticated.
	apiKey string
	caps   Capabilities
}

var _ Provider = (*TinfoilConfidentialProvider)(nil)

// TinfoilConfidentialConfig configures a confidential-inference backend.
type TinfoilConfidentialConfig struct {
	// Enclave is the bare hostname the client attests against (e.g.
	// "router.inference.aptoslabs.com") — NOT a URL. A leading "https://" or
	// "http://", or a trailing slash, is stripped defensively: the model
	// catalog's Base URL field is shared with ProviderOpenAICompatible,
	// where a full URL is the convention, so an operator pasting one here by
	// habit must not silently produce a broken enclave hostname.
	Enclave string
	// Model is the provider's own model id (e.g. "qwen3-omni", "glm-5.3") —
	// modelcatalog.Model.ProviderModel, the same field VLLMProvider reads.
	Model string
	// APIKey authenticates to the partner's router. Required — unlike
	// VLLMProvider's optional key, there is no local backend to fall back
	// to without one.
	APIKey string
	// ContextWindow in tokens. Zero disables the pre-dispatch fit check,
	// same convention as VLLMConfig.
	ContextWindow int
}

// NewTinfoilConfidential builds a confidential-inference provider, performing
// the real remote-attestation handshake against the enclave once at
// construction time (see tinfoilClientTimeout). externalProviderFor caches
// the result by config hash, so this runs once per config version, not once
// per request — the same lifecycle VLLMProvider's and AnthropicProvider's
// constructors already have.
//
// A construction failure (attestation rejected, enclave unreachable, key
// invalid) is returned as an error rather than falling back to an unverified
// connection — matching flashback-instructions.md's own instruction
// verbatim: "If verification fails, the client raises an error and sends
// nothing. Treat that as a hard failure... do not work around it."
func NewTinfoilConfidential(cfg TinfoilConfidentialConfig) (*TinfoilConfidentialProvider, error) {
	enclave := normalizeEnclave(cfg.Enclave)
	if enclave == "" {
		return nil, fmt.Errorf("tinfoil confidential provider: enclave is required")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("tinfoil confidential provider: api key is required")
	}

	type result struct {
		client *tinfoil.Client
		err    error
	}
	done := make(chan result, 1)
	go func() {
		c, err := tinfoil.NewClientWithParams(enclave, aptosConfidentialInferenceRepo, option.WithAPIKey(cfg.APIKey))
		done <- result{client: c, err: err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return nil, fmt.Errorf("%w: enclave attestation failed for %q: %v", ErrProviderUnavailable, enclave, r.err)
		}
		return &TinfoilConfidentialProvider{
			http:    r.client.HTTPClient(),
			baseURL: fmt.Sprintf("https://%s/v1", enclave),
			enclave: enclave,
			model:   cfg.Model,
			apiKey:  cfg.APIKey,
			caps: Capabilities{
				ContextWindow: cfg.ContextWindow,
				SupportsTools: true,
				SupportsJSON:  true,
				CostClass:     CostClassFrontier,
			},
		}, nil
	case <-time.After(tinfoilClientTimeout):
		return nil, fmt.Errorf("%w: enclave attestation for %q did not complete within %s", ErrProviderUnavailable, enclave, tinfoilClientTimeout)
	}
}

// normalizeEnclave strips a scheme prefix and trailing slash a caller may
// have pasted out of habit — see TinfoilConfidentialConfig.Enclave's own
// comment for why. A pure function so it's testable without the network
// call NewTinfoilConfidential otherwise makes.
func normalizeEnclave(s string) string {
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	return s
}

func (p *TinfoilConfidentialProvider) Name() string               { return "tinfoil_confidential" }
func (p *TinfoilConfidentialProvider) Capabilities() Capabilities { return p.caps }

// Complete performs a non-streaming completion. Identical in shape to
// VLLMProvider.Complete — same Extra-first encode, same raw-body passthrough
// — because both backends promise an OpenAI-compatible response and the
// surest way to keep that promise is to not rewrite what the backend
// produced.
func (p *TinfoilConfidentialProvider) Complete(ctx context.Context, req Request) (*Response, error) {
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

// Stream performs a streaming completion over the enclave's OpenAI-compatible
// SSE endpoint. Identical logic to VLLMProvider.Stream, including forcing
// stream_options.include_usage — see that method's own doc comment for why
// a streamed request without it is unbillable.
func (p *TinfoilConfidentialProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk) error) error {
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
		raw, readErr := io.ReadAll(io.LimitReader(httpResp.Body, 8192))
		if readErr != nil {
			return fmt.Errorf("%w: reading error response: %v", ErrProviderUnavailable, readErr)
		}
		return classifyStatus(httpResp.StatusCode, raw)
	}

	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
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
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w: reading stream: %v", ErrProviderUnavailable, err)
	}
	return fmt.Errorf("%w: stream ended without a terminal [DONE] frame", ErrProviderUnavailable)
}

// encode mirrors VLLMProvider.encode exactly — see its own comment for why
// Extra is written first.
func (p *TinfoilConfidentialProvider) encode(req Request, stream bool) ([]byte, error) {
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
		out["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	}

	return json.Marshal(out)
}

func (p *TinfoilConfidentialProvider) post(ctx context.Context, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.http.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	return resp, nil
}
