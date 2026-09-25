// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package modelcatalog is the platform's single model registry and per-model
// pricing — every model Teepin can serve, self-hosted or third-party, and
// where each may be used: Teepin Inference's customer API, the Kumbha build
// agent, or both. Distinct from pkg/inference (the stateless runtime
// Provider seam). Teepin serves several models with very different real
// per-token costs at once, so pricing is keyed by model, not global.
package modelcatalog

import "time"

// CostClass distinguishes inference Teepin runs from inference it buys —
// mirrors pkg/inference.CostClass exactly, redeclared here rather than
// imported so this package stays free of a dependency on the runtime seam
// (a catalog entry can exist before any Provider ever serves it).
type CostClass string

const (
	CostClassOwn      CostClass = "own"
	CostClassFrontier CostClass = "frontier"
)

// Provider is how a model is served.
type Provider string

const (
	// ProviderNode is self-hosted: mounted on Teepin's own nodes through
	// compute.node_services, load-balanced across every live mount.
	ProviderNode Provider = "node"
	// ProviderAnthropic is Anthropic's Messages API.
	ProviderAnthropic Provider = "anthropic"
	// ProviderOpenAICompatible is any third-party endpoint speaking the
	// OpenAI chat-completions API, at BaseURL.
	ProviderOpenAICompatible Provider = "openai_compatible"
	// ProviderTinfoilConfidential is a hardware-attested confidential-
	// inference enclave reached via the Tinfoil client (see
	// pkg/inference.TinfoilConfidentialProvider) rather than plain TLS — a
	// distinct Provider from ProviderOpenAICompatible even though both speak
	// the OpenAI chat-completions shape on the wire, because this one's
	// BaseURL is a bare enclave hostname attested against a fixed,
	// hardcoded repo, not an arbitrary endpoint an operator can point
	// anywhere.
	ProviderTinfoilConfidential Provider = "tinfoil_confidential"
)

// IsExternal reports whether the model is served by a third-party API
// rather than Teepin's own nodes.
func (p Provider) IsExternal() bool { return p != ProviderNode }

// Model is one entry in Teepin Inference's catalog: a routable model name,
// what it can do, and what it costs on both sides of the ledger.
type Model struct {
	// ModelRoute is the key callers address ("teepin/qwen3-omni-7b",
	// "anthropic/claude-sonnet-5") — the same "route key, not the backend's
	// own model id" convention pkg/inference.Request.Model already
	// documents.
	ModelRoute  string    `json:"model_route"`
	DisplayName string    `json:"display_name"`
	CostClass   CostClass `json:"cost_class"`
	Engine      string    `json:"engine"`

	ContextWindow  int  `json:"context_window"`
	SupportsTools  bool `json:"supports_tools"`
	SupportsVision bool `json:"supports_vision"`
	SupportsAudio  bool `json:"supports_audio"`

	// Customer-facing rates, per million tokens. Zero until an admin sets
	// them — same "ships on, bills nothing until configured" contract as
	// every other rate on this platform.
	InputPricePerMillion  float64 `json:"input_price_per_million"`
	OutputPricePerMillion float64 `json:"output_price_per_million"`

	// What the vendor actually charges Teepin, per million tokens. Nil for
	// CostClassOwn (no per-token vendor invoice exists — see CostClassOwn's
	// own doc comment in pkg/inference.go) and for a frontier model whose
	// vendor cost hasn't been recorded yet. Never a fabricated zero.
	VendorInputCostPerMillion  *float64 `json:"vendor_input_cost_per_million,omitempty"`
	VendorOutputCostPerMillion *float64 `json:"vendor_output_cost_per_million,omitempty"`

	// Provider/ProviderModel/BaseURL/MaxOutputTokens describe how to reach
	// an external model; a ProviderNode model ignores all but Provider.
	Provider        Provider `json:"provider"`
	ProviderModel   string   `json:"provider_model"`
	BaseURL         string   `json:"base_url"`
	MaxOutputTokens int      `json:"max_output_tokens"`
	// APIKeyRef names the Secrets Manager secret holding this model's API
	// key, without the "teepin/<environment>/" prefix. Empty means none is
	// stored. Never serialized: only HasAPIKey reaches an API response.
	APIKeyRef string `json:"-"`
	HasAPIKey bool   `json:"has_api_key"`

	// Enabled is the master switch: a disabled model is served nowhere. A
	// model can be catalogued (pricing set, capabilities recorded) before
	// it's actually servable, or retired without losing its history.
	Enabled bool `json:"enabled"`
	// OfferedToCustomers exposes the model on Teepin Inference's public API.
	OfferedToCustomers bool `json:"offered_to_customers"`
	// KumbhaEnabled lets the Kumbha build agent use the model; enabled
	// models are tried in ascending KumbhaPriority order.
	KumbhaEnabled  bool `json:"kumbha_enabled"`
	KumbhaPriority int  `json:"kumbha_priority"`

	UpdatedBy *string   `json:"updated_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
