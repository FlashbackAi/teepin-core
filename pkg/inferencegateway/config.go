// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package inferencegateway is Teepin Inference's router: it resolves an
// incoming request's model route against the live catalog
// (pkg/modelcatalog) and, for self-hosted models, the live mounted
// backends (pkg/nodeservices), load-balances across whichever backends are
// actually mounted right now, and bounds concurrency per backend and per
// account before dispatching to a pkg/inference.Provider.
//
// Deliberately does not do its own HTTP serving, auth, or billing — those
// are separate seams (a future public HTTP layer, and pkg/billing) that
// call into this package with an already-resolved account id, the same way
// pkg/inference.Provider itself stays ignorant of who's calling.
package inferencegateway

import "encoding/json"

// ModelServiceConfig is the JSON shape stored in a
// compute.node_services.config column when kind = KindInferenceModel
// (see pkg/nodeservices). Opaque to that package by design; this is the
// one place that actually interprets it.
type ModelServiceConfig struct {
	// ModelRoute must match the catalog entry this backend serves —
	// how the gateway finds "which mounted backends serve this model."
	ModelRoute string `json:"model_route"`
	// Engine selects which pkg/inference.Provider constructor to use.
	// Only "vllm" and "vllm-omni" are wired today (both speak the same
	// OpenAI-compatible HTTP surface as pkg/inference.VLLMProvider) —
	// see the roadmap entry on engine choice per hardware class for why
	// MLX/llama.cpp backends are a separate, not-yet-built Provider.
	Engine string `json:"engine"`
	// BaseURL is where this specific mounted instance is reachable.
	// Direct HTTP today (matches how the existing single-model gateway
	// already reaches its one vLLM box); a home node behind NAT needs a
	// tunnel-backed Provider instead — not yet built, see the roadmap
	// entry on home-node reachability.
	BaseURL string `json:"base_url"`
	// BackendModel is the identifier the engine was actually launched
	// with — every request gets rewritten to this, mirroring
	// VLLMConfig.Model's own doc comment.
	BackendModel string `json:"backend_model"`
	APIKey       string `json:"api_key,omitempty"`
	// MaxConcurrency bounds how many requests this specific mounted
	// instance serves at once. Zero/absent uses defaultMaxConcurrency —
	// a Mac Mini running MLX (no internal batching, effectively
	// serial) should set this to 1; a vLLM box can go higher.
	MaxConcurrency int `json:"max_concurrency,omitempty"`
}

// ParseModelServiceConfig decodes a node_services.config column. Exported
// so callers building/inspecting node_services rows for this kind (an
// eventual admin handler, a test) share one definition of the shape rather
// than each re-declaring it.
func ParseModelServiceConfig(raw json.RawMessage) (ModelServiceConfig, error) {
	var cfg ModelServiceConfig
	err := json.Unmarshal(raw, &cfg)
	return cfg, err
}
