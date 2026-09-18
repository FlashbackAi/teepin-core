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

import (
	"encoding/json"
	"net/url"
	"strings"
)

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
	// ModelSource is where the reconciler gets the model weights from —
	// operator-provided, either a huggingface.co URL (parsed for its repo
	// ID, which is then handed to the engine's own native downloader) or
	// any other direct download URL (fetched by a real init container;
	// see pkg/inferencereconciler). NOT where the running instance is
	// reachable — that is ObservedEndpoint on the node_services row
	// itself, resolved by the reconciler after the instance actually
	// starts, never operator-typed.
	ModelSource string `json:"model_source"`
	// BackendModel overrides the identifier passed to the engine's
	// --served-model-name (or equivalent) — every request gets rewritten
	// to this, mirroring VLLMConfig.Model's own doc comment. Optional:
	// for a HuggingFace ModelSource, the reconciler defaults this to the
	// parsed repo ID when left blank.
	BackendModel string `json:"backend_model,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	// MaxConcurrency bounds how many requests this specific mounted
	// instance serves at once. Zero/absent uses defaultMaxConcurrency —
	// a Mac Mini running MLX (no internal batching, effectively
	// serial) should set this to 1; a vLLM box can go higher.
	MaxConcurrency int `json:"max_concurrency,omitempty"`
	// StorageGB provisions the persistent volume the reconciler mounts
	// at /data — used for the downloaded model (direct-URL sources) or
	// the HuggingFace cache directory (HF sources), either way surviving
	// a pod restart so the model is never re-fetched from scratch.
	// Required (> 0) for any inference_model mount; the reconciler
	// rejects a mount that omits it rather than guessing a size.
	StorageGB int `json:"storage_gb,omitempty"`
	// CPUUnits/MemoryGB size the serving container itself — required
	// (> 0), same reasoning as StorageGB: model sizes vary too much for
	// the reconciler to guess a resource shape safely.
	CPUUnits int `json:"cpu_units,omitempty"`
	MemoryGB int `json:"memory_gb,omitempty"`
	// GPUCount requests that many whole, unsliced GPUs on the target
	// node — 0 means CPU-only serving, a real (if unusual) option for a
	// small enough model. A consumer GPU cannot be MIG-partitioned, so
	// this is always a whole-card request, never a fraction — see the
	// roadmap's "whole-GPU home-node instance" entry for why that's a
	// real, safe primitive to use directly here even though the
	// customer-facing product built around it doesn't exist yet.
	GPUCount int `json:"gpu_count,omitempty"`
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

// backendModelName is the model identifier the engine was launched with, and
// therefore what every request must be rewritten to. It matches what the
// reconciler passed to the engine: BackendModel when the operator set one,
// otherwise the HuggingFace repo id parsed from ModelSource. Without this an
// unset BackendModel would leave the catalog route ("teepin/k2") in the
// request, which vLLM rejects as an unknown model and mlx-lm would try to
// download as a repo.
func backendModelName(cfg ModelServiceConfig) string {
	if cfg.BackendModel != "" {
		return cfg.BackendModel
	}
	u, err := url.Parse(strings.TrimSpace(cfg.ModelSource))
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Host)
	if host != "huggingface.co" && host != "www.huggingface.co" {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}
