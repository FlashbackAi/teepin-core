// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package modelcatalog is Teepin Inference's model registry and per-model
// pricing — distinct from pkg/inference (the stateless runtime Provider
// seam) and from billing.pricing's own llm_price_per_million_input/output
// columns (a single flat rate built for Kumbha's internal single-model
// gateway). Teepin Inference serves several models with very different real
// per-token costs at once, so pricing has to be keyed by model, not global.
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

	// Enabled gates routing, not existence — a model can be catalogued
	// (pricing set, capabilities recorded) before it's actually mountable,
	// or retired without losing its pricing/audit history.
	Enabled bool `json:"enabled"`

	UpdatedBy *string   `json:"updated_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
