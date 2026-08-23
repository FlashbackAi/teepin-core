// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package inference is the model-backend seam for the Kumbha gateway: one
// Provider interface with an implementation per backend (vLLM on our own
// GPUs today; Anthropic/OpenAI and a customer's local model later).
//
// The shape deliberately mirrors pkg/cluster's Client seam, where
// DirectClient and AgentClient both satisfy one interface and callers never
// learn which they hold. Same reasoning here: the gateway's auth, budget,
// metering and audit logic must not branch on which model served a request.
//
// THE RULE THIS PACKAGE EXISTS TO ENFORCE: no agent harness ever talks to a
// model backend directly. Everything goes through the gateway, because a
// call we cannot see is one we cannot meter, cap, audit or route — and
// un-meterable spend is the single most common complaint levelled at this
// entire product category (see KUMBHA-DESIGN.md).
//
// Fidelity over modelling. A request is passed to the backend as close to
// verbatim as possible, and only the fields the gateway must reason about
// (model, message bulk, token counts) are parsed out. Harnesses send fields
// this package has never heard of — tools, tool_choice, response_format,
// seed, logprobs — and silently dropping any of them would break tool
// calling in ways that surface as an agent "just not working", which is
// among the worst possible bugs to diagnose from the outside.
package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Sentinel errors, classified so the HTTP layer maps them to a status
// without string-matching. Anything not listed here is an internal fault
// and becomes a 500.
var (
	// ErrContextTooLarge means the request cannot fit the provider's
	// context window. Returned BEFORE dispatch: catching it here costs
	// nothing, whereas letting the backend reject it burns a round trip
	// and, on some providers, real tokens.
	ErrContextTooLarge = errors.New("request exceeds the model's context window")

	// ErrUnknownModel is an unroutable model name.
	ErrUnknownModel = errors.New("unknown model")

	// ErrProviderUnavailable means the backend could not be reached or
	// returned 5xx — retryable, and distinct from the caller having sent
	// something invalid.
	ErrProviderUnavailable = errors.New("model provider unavailable")

	// ErrProviderRejected means the backend returned 4xx: the request was
	// malformed or violated a backend constraint. Not retryable.
	ErrProviderRejected = errors.New("model provider rejected the request")
)

// Request is one inference call, decoded only as far as the gateway needs.
//
// Messages stay as raw JSON on purpose. The gateway counts and sizes them
// but has no reason to interpret their contents, and a message can carry
// tool_calls, multimodal content parts, or provider-specific extensions
// that a struct would quietly discard. The Anthropic adapter will parse
// them properly when it lands, because it genuinely must translate.
type Request struct {
	// Model is the ROUTE key the caller asked for ("teepin/fast"), not the
	// backend's own model identifier. Providers substitute their own.
	Model     string
	Messages  []json.RawMessage
	MaxTokens int
	Stream    bool

	// Extra carries every top-level field this package does not model, so
	// the backend receives them untouched. Populated by the HTTP layer from
	// the original body; merged back in by each provider on encode.
	Extra map[string]json.RawMessage
}

// Usage is the token accounting for one call — the input to metering, and
// the only reason the gateway parses provider responses at all.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Response carries the backend's reply verbatim plus the accounting parsed
// out of it.
//
// Body is returned to the caller as-is rather than re-encoded from a struct:
// the gateway promises an OpenAI-compatible response, and the surest way to
// keep that promise is to not rewrite what an already-OpenAI-compatible
// backend produced.
type Response struct {
	Usage Usage
	// Model is what actually served the request (the backend's own id), for
	// the audit log and the admin margin view — never the route alias.
	Model string
	Body  json.RawMessage
}

// Chunk is one streamed fragment. Defined now so the Provider interface is
// stable, though streaming lands later in the build order (KUMBHA-DESIGN.md).
//
// Usage is set only on the final chunk, and ONLY if the request asked the
// backend for it (stream_options.include_usage on OpenAI-shaped backends).
// Without that flag a streamed request is unbillable — the gateway must set
// it rather than trusting the caller to.
type Chunk struct {
	Data  json.RawMessage
	Usage *Usage
	Done  bool
}

// Capabilities describes what a backend can actually do. It drives routing
// and the pre-dispatch fit check, so the gateway can refuse a request a
// backend cannot serve instead of discovering it after spending tokens.
type Capabilities struct {
	// ContextWindow in tokens. Configured rather than probed: backends do
	// not reliably advertise it, and a wrong value here makes the fit check
	// either useless (too high) or a source of spurious refusals (too low).
	ContextWindow int
	SupportsTools bool
	SupportsJSON  bool

	// CostClass separates what we own from what we resell. Routing policy
	// and the admin margin view both key off it, so it is deliberately a
	// small closed set rather than free text.
	CostClass CostClass
}

// CostClass distinguishes inference we run from inference we buy — the
// distinction the whole margin thesis rests on (KUMBHA-DESIGN.md).
type CostClass string

const (
	// CostClassOwn is our own GPUs. Marginal cost is GPU-hours, not a
	// per-token invoice, so cost_basis for these records is unattributed
	// rather than zero until a GPU-hour model exists.
	CostClassOwn CostClass = "own"
	// CostClassFrontier is a third party billing us per token. Every one of
	// these has a real cost_basis and counts against our own spend ceiling.
	CostClassFrontier CostClass = "frontier"
)

// Provider is one model backend.
//
// Implementations must be safe for concurrent use: the gateway serves many
// sessions from one process and will call a single Provider from many
// goroutines.
type Provider interface {
	// Name identifies the backend in usage records and the audit log
	// ("vllm", "anthropic"). Stable — it is written to the billing ledger,
	// so changing it orphans historical rows from their provider.
	Name() string

	Complete(ctx context.Context, req Request) (*Response, error)

	// Stream delivers chunks in order until the final one (Done) or an
	// error. onChunk returning an error aborts the stream; the provider
	// must still surface whatever usage it observed, because tokens the
	// backend already generated cost real money whether or not the caller
	// stayed to read them.
	Stream(ctx context.Context, req Request, onChunk func(Chunk) error) error

	Capabilities() Capabilities
}

// EstimateTokens approximates the token count of a request for the
// pre-dispatch fit check.
//
// Deliberately an estimate, and deliberately conservative. Counting exactly
// needs the backend's own tokenizer, which the gateway does not have and
// should not carry (it would have to track every model's vocabulary). Four
// bytes per token is the widely used rule of thumb for English text; it
// under-counts CJK and over-counts whitespace-heavy code.
//
// So this is a cheap guard against the obviously-too-large request, NOT an
// authority. The backend remains the real enforcement point, and a request
// that passes here can still be rejected there — which is correct, and why
// ErrProviderRejected exists as a distinct outcome.
func EstimateTokens(req Request) int {
	bytes := 0
	for _, m := range req.Messages {
		bytes += len(m)
	}
	// A small per-message allowance for the role/formatting scaffolding
	// every chat format wraps around content.
	return bytes/4 + len(req.Messages)*4
}

// FitsContext reports whether a request plausibly fits, leaving room for the
// completion. Requests reserve MaxTokens of the window for output, so the
// prompt must fit in what remains.
func FitsContext(req Request, caps Capabilities) error {
	if caps.ContextWindow <= 0 {
		// Unconfigured window: do not invent a limit and reject valid work.
		// The backend will enforce its own.
		return nil
	}
	need := EstimateTokens(req) + req.MaxTokens
	if need > caps.ContextWindow {
		return fmt.Errorf("%w: approximately %d tokens needed, window is %d",
			ErrContextTooLarge, need, caps.ContextWindow)
	}
	return nil
}
