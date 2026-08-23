// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicProvider talks to Claude over the official Anthropic Messages
// API — a genuinely different wire shape from vLLM's OpenAI-compatible
// endpoint, which is exactly why this provider exists: it is the proof
// that "adding a model costs one adapter" (KUMBHA-DESIGN.md) is a real
// property of pkg/inference, not just a claim. Everything provider-
// specific is translated at the edges: Request's OpenAI-shaped messages
// go in, an OpenAI-chat-completion-shaped Response.Body comes out — a
// harness talking to the gateway never needs to know Anthropic was
// involved.
type AnthropicProvider struct {
	client          anthropic.Client
	model           string
	maxOutputTokens int
	caps            Capabilities
}

var _ Provider = (*AnthropicProvider)(nil)

// AnthropicConfig configures a Claude backend.
type AnthropicConfig struct {
	// Model is the Claude model id (e.g. "claude-opus-5"). Required.
	Model string
	// APIKey is optional. Empty leaves the SDK's own credential
	// resolution in charge (ANTHROPIC_API_KEY, then other sources) —
	// see the claude-api skill's Authentication reference.
	APIKey string
	// ContextWindow in tokens. Zero disables the pre-dispatch fit check
	// (see FitsContext) rather than guessing a limit.
	ContextWindow int
	// MaxOutputTokens is used whenever a request leaves MaxTokens unset —
	// unlike vLLM's OpenAI-compatible endpoint, Anthropic's API requires
	// max_tokens on every request; there is no "let the backend decide"
	// default to fall back on.
	MaxOutputTokens int
	// BaseURL overrides the SDK's default endpoint. Provided for tests
	// (point at an httptest.Server); production should leave it unset and
	// take the SDK's own default.
	BaseURL string
}

const defaultAnthropicMaxTokens = 4096

// NewAnthropic builds a Claude-backed provider.
func NewAnthropic(cfg AnthropicConfig) *AnthropicProvider {
	var opts []option.RequestOption
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}

	maxOut := cfg.MaxOutputTokens
	if maxOut <= 0 {
		maxOut = defaultAnthropicMaxTokens
	}

	return &AnthropicProvider{
		client:          anthropic.NewClient(opts...),
		model:           cfg.Model,
		maxOutputTokens: maxOut,
		caps: Capabilities{
			ContextWindow: cfg.ContextWindow,
			SupportsTools: true,
			SupportsJSON:  true,
			CostClass:     CostClassFrontier,
		},
	}
}

func (p *AnthropicProvider) Name() string               { return "anthropic" }
func (p *AnthropicProvider) Capabilities() Capabilities { return p.caps }

// openAIMessage is the minimal shape Complete reads out of each raw
// message in Request.Messages — just enough to translate into Anthropic's
// message/system split, not a full re-modelling of the OpenAI schema (see
// pkg/inference's doc comment on why Messages stays raw JSON).
type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// extractText pulls plain text out of an OpenAI-shaped content field,
// which is either a bare string or an array of typed parts
// ([{"type":"text","text":"..."}, ...], OpenAI's multimodal shape).
// Non-text parts (images, etc.) are skipped rather than rejected — a
// harness sending a mixed-content message still gets a response instead
// of a hard failure, at the cost of the image being silently invisible to
// Claude on this path. Full multimodal support is not needed to prove the
// adapter property this provider exists for.
func extractText(content json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return "", fmt.Errorf("unsupported message content shape: %w", err)
	}
	var b strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			b.WriteString(part.Text)
		}
	}
	return b.String(), nil
}

// Complete performs a non-streaming completion.
func (p *AnthropicProvider) Complete(ctx context.Context, req Request) (*Response, error) {
	if err := FitsContext(req, p.caps); err != nil {
		return nil, err
	}

	var system []anthropic.TextBlockParam
	var messages []anthropic.MessageParam
	for _, raw := range req.Messages {
		var m openAIMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("%w: invalid message: %v", ErrProviderRejected, err)
		}
		text, err := extractText(m.Content)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrProviderRejected, err)
		}

		switch m.Role {
		case "system":
			// Anthropic has no "system" role inside the message list — it
			// is a separate top-level field. Multiple system messages
			// concatenate in order, matching how most harnesses treat a
			// system-role sequence.
			system = append(system, anthropic.TextBlockParam{Text: text})
		case "assistant":
			messages = append(messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(text)))
		default:
			// "user" and anything unrecognised default to user — the safe
			// direction, since treating an unknown role as assistant could
			// let untrusted content masquerade as the model's own prior
			// turn.
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(text)))
		}
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.maxOutputTokens
	}

	resp, err := p.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: int64(maxTokens),
		System:    system,
		Messages:  messages,
	})
	if err != nil {
		return nil, classifyAnthropicError(ctx, err)
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(tb.Text)
		}
	}

	usage := Usage{
		InputTokens:  int(resp.Usage.InputTokens),
		OutputTokens: int(resp.Usage.OutputTokens),
	}

	// The response the gateway forwards to the caller is built fresh in
	// OpenAI's chat-completion shape — never Anthropic's own response
	// passed through — because that shape is the one contract every
	// harness on the other side of the gateway already speaks (see the
	// package doc comment: "OpenAI-compatible on both sides").
	body, err := json.Marshal(openAICompletionBody{
		Model: string(resp.Model),
		Choices: []openAIChoice{{
			Message:      openAIResponseMessage{Role: "assistant", Content: text.String()},
			FinishReason: mapAnthropicStopReason(resp.StopReason),
		}},
		Usage: openAIUsageBody{
			PromptTokens:     usage.InputTokens,
			CompletionTokens: usage.OutputTokens,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encoding response: %v", ErrProviderUnavailable, err)
	}

	return &Response{Usage: usage, Model: string(resp.Model), Body: body}, nil
}

// Stream is not implemented yet. Anthropic's own build order lands after
// vLLM's (KUMBHA-DESIGN.md's Gateway build order sequences streaming
// against the backend already proven end to end); an explicit error beats
// a silent fallback to Complete, same precedent as vLLM's Stream before it
// was implemented.
func (p *AnthropicProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk) error) error {
	return fmt.Errorf("%w: streaming is not implemented for anthropic yet", ErrProviderRejected)
}

// classifyAnthropicError turns an SDK error into one of the package's
// typed errors — same retryable-vs-not split as vLLM's classifyStatus:
// 5xx and 429 mean "try again", other 4xx mean the request itself is
// unacceptable.
func classifyAnthropicError(ctx context.Context, err error) error {
	// A cancelled context is the caller giving up, not the backend
	// failing — same reasoning as vLLM's post(): reporting it as "provider
	// unavailable" would fire spurious alerts and make a disconnected
	// client look like an outage.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode >= 500 || apiErr.StatusCode == 429 {
			return fmt.Errorf("%w: upstream status %d: %v", ErrProviderUnavailable, apiErr.StatusCode, err)
		}
		return fmt.Errorf("%w: upstream status %d: %v", ErrProviderRejected, apiErr.StatusCode, err)
	}
	// Not a structured API error (e.g. a connection failure) — treat as
	// retryable, the same direction network errors take in vLLM's post().
	return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
}

// openAICompletionBody / openAIChoice / openAIResponseMessage /
// openAIUsageBody are the minimal OpenAI chat-completion shape Complete
// constructs for Response.Body — only the fields a harness actually reads
// (message content, finish reason, usage), not a full re-implementation
// of every optional OpenAI field.
type openAICompletionBody struct {
	Model   string          `json:"model"`
	Choices []openAIChoice  `json:"choices"`
	Usage   openAIUsageBody `json:"usage"`
}

type openAIChoice struct {
	Index        int                   `json:"index"`
	Message      openAIResponseMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

type openAIResponseMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIUsageBody struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// mapAnthropicStopReason translates Anthropic's stop_reason vocabulary
// into OpenAI's finish_reason vocabulary, so a harness written against
// OpenAI's contract does not need to learn a second one for this route.
func mapAnthropicStopReason(reason anthropic.StopReason) string {
	switch reason {
	case anthropic.StopReasonMaxTokens:
		return "length"
	case anthropic.StopReasonToolUse:
		return "tool_calls"
	case anthropic.StopReasonRefusal:
		return "content_filter"
	default: // end_turn, stop_sequence, pause_turn, and anything future
		return "stop"
	}
}
