// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package teepinbuild

import (
	"context"
	"fmt"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// Teepin Build has no model registry of its own. Which models the build agent may
// use is a property of each model in the platform's one catalog
// (pkg/modelcatalog: build_enabled), and every completion is served
// through Teepin Inference's gateway — the same path a customer's own API
// call takes. ModelBackend is that seam.
//
// A customer picks one of these models DIRECTLY, by its real route — there
// is no alias/tier bucket sitting in front of them. An earlier design
// (migration 055's build_alias — "teepin/fast" / "teepin/deep" /
// "teepin/confidential") modeled "which bucket" as one mutually-exclusive
// category per model, which does not match reality: a model can be BOTH
// tool-capable and confidential, or neither, and forcing it into exactly
// one of three buckets either hid a real gap (a bucket with no
// tool-capable model in it, silently unusable by the build agent) or
// created a real security regression — a bucket with more than one
// backing model could fail over from a customer's deliberately-chosen
// confidential model to a DIFFERENT, non-confidential one, silently.
// Direct selection with per-model badges (Confidential, Self-hosted vs
// third-party — both computed from Provider, never stored separately)
// avoids both problems: a customer sees exactly what they are choosing,
// and a failure surfaces as an error on THAT model, never a silent switch
// to one with different guarantees.

// Model is one model the build agent may use, listed to a customer so they
// can choose it directly by name.
type Model struct {
	// Route is the model's catalog model_route — what a customer's choice,
	// and every completion request for that session, names directly.
	Route       string
	DisplayName string
	// Engine identifies the serving backend in usage records' provider
	// column, the one place backend identity is recorded (for the admin
	// margin view) — never shown to a customer.
	Engine string
	// Confidential mirrors modelcatalog.ProviderTinfoilConfidential — a
	// hardware-attested enclave, not a claim Teepin or a plain third party
	// makes about itself.
	Confidential bool
	// SelfHosted is modelcatalog.Provider.IsThirdParty()'s negation, not a
	// plain ProviderNode check — a partner-hosted confidential enclave
	// (ProviderTinfoilConfidential) is presented as Teepin's own
	// infrastructure to the customer, same posture as leased GPU/CPU
	// compute. False means a genuine vendor API Teepin merely calls
	// (Anthropic, an arbitrary OpenAI-compatible endpoint).
	SelfHosted    bool
	SupportsTools bool
	// ToolMode is how the build agent should talk to this model about tools:
	// "native" (structured tool calls) or "text" (the harness's prompt-based
	// protocol, for a model whose native calls do not work). Empty means the
	// agent's default, native.
	ToolMode       string
	SupportsVision bool
	SupportsAudio  bool
	// ContextWindow is the model's input window in tokens, 0 when the
	// catalog does not know it. The agent pod uses it to decide when to
	// condense its conversation history (see LaunchAgent's
	// TEEPIN_CONTEXT_WINDOW).
	ContextWindow int
	// MaxOutputTokens is the most the model can write in one answer, 0 when the
	// catalog does not know it. The agent tells its harness so a single answer
	// is never asked to be longer than the model can give.
	MaxOutputTokens int
	// ReasoningEffort is how hard the model thinks per answer ("low", "medium",
	// "high", "max"), empty to leave the model's own default. The agent sends it
	// with every request (see LaunchAgent's TEEPIN_REASONING_EFFORT).
	ReasoningEffort       string
	InputPricePerMillion  float64
	OutputPricePerMillion float64
	// Unavailable is why the model cannot be used right now ("Not running
	// right now"), empty when it can. A build cannot be started on it; the
	// picker shows it disabled with this reason.
	Unavailable string
}

// ModelBackend lists Teepin Build's models and serves completions against them.
// Implemented in production by an adapter over modelcatalog and
// inferencegateway (cmd/api-server/adapters.go), which keeps this package
// free of both.
type ModelBackend interface {
	// BuildModels returns every model the build agent may currently be
	// asked for — for a customer-facing picker. Order is catalog
	// build_priority (display/default order), never a failover sequence:
	// a customer's choice is exact and is never silently substituted for a
	// different model.
	BuildModels(ctx context.Context) ([]Model, error)
	// Complete serves req against the catalog model named by req.Model.
	Complete(ctx context.Context, accountID string, req inference.Request) (*inference.Response, error)
}

// StaticModel is one entry of a StaticModels backend.
type StaticModel struct {
	Route           string
	Engine          string
	ContextWindow   int
	MaxOutputTokens int
	ReasoningEffort string
	SupportsVision  bool
	// ToolMode is "native" (the default when empty) or "text".
	ToolMode string
	Provider inference.Provider
}

// StaticModels is a fixed, in-memory ModelBackend: its models are listed in
// slice order, each served by its own Provider. For tests and for running
// Teepin Build without a model catalog.
type StaticModels []StaticModel

func (s StaticModels) BuildModels(context.Context) ([]Model, error) {
	out := make([]Model, 0, len(s))
	for _, m := range s {
		out = append(out, Model{Route: m.Route, Engine: m.Engine, ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
			ReasoningEffort: m.ReasoningEffort, SupportsVision: m.SupportsVision, ToolMode: m.ToolMode})
	}
	return out, nil
}

func (s StaticModels) Complete(ctx context.Context, _ string, req inference.Request) (*inference.Response, error) {
	for _, m := range s {
		if m.Route == req.Model && m.Provider != nil {
			return m.Provider.Complete(ctx, req)
		}
	}
	return nil, fmt.Errorf("%w: %q", inference.ErrUnknownModel, req.Model)
}
