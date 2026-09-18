-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Teepin Inference's model catalog and per-model pricing.
--
-- Distinct from billing.pricing.llm_price_per_million_input/output (migration
-- 024-era column, built for Kumbha's own internal single-model gateway): that
-- pair is ONE flat rate for the whole platform, which was fine when there was
-- exactly one self-hosted model in play. Teepin Inference serves several
-- models at once -- a self-hosted 7B and a proxied frontier model have wildly
-- different real per-token costs -- so pricing has to be keyed by model, not
-- global. billing.pricing's LLM columns are untouched and keep meaning
-- whatever Kumbha's own internal routing already uses them for.
--
-- model_route is the same "route key" concept pkg/inference.Request.Model
-- already documents ("teepin/fast", not the backend's own model id) --
-- primary key here rather than a surrogate UUID because callers, config, and
-- logs all address a model by this string, never by an internal id.
--
-- Same "ships on, bills nothing until an operator sets it" contract as every
-- other rate in this platform: input/output rates default to 0.

BEGIN;

CREATE SCHEMA IF NOT EXISTS inference;

CREATE TABLE inference.models (
    model_route              TEXT PRIMARY KEY,
    display_name             TEXT NOT NULL,
    -- 'own': self-hosted, marginal cost is amortised GPU-hours not a
    -- per-token invoice (see pkg/inference.go's CostClassOwn doc comment).
    -- 'frontier': a third party bills us per token.
    cost_class               TEXT NOT NULL CHECK (cost_class IN ('own', 'frontier')),
    -- Free text ('vllm', 'vllm-omni', 'mlx', 'anthropic', ...) -- routing
    -- and placement care about node_services.config, not this column; it is
    -- informational, for the admin UI and audit trail.
    engine                   TEXT NOT NULL,
    context_window           INT NOT NULL DEFAULT 0 CHECK (context_window >= 0),
    supports_tools           BOOLEAN NOT NULL DEFAULT FALSE,
    supports_vision          BOOLEAN NOT NULL DEFAULT FALSE,
    supports_audio           BOOLEAN NOT NULL DEFAULT FALSE,
    -- Customer-facing rates, per million tokens. Priced separately because
    -- input (prefill, parallel) and output (decode, one token at a time)
    -- cost very differently on every backend this platform routes to.
    input_price_per_million  DECIMAL(12,4) NOT NULL DEFAULT 0 CHECK (input_price_per_million >= 0),
    output_price_per_million DECIMAL(12,4) NOT NULL DEFAULT 0 CHECK (output_price_per_million >= 0),
    -- What the vendor actually charges US, per million tokens -- only
    -- meaningful for cost_class='frontier', NULL for 'own' (which has no
    -- per-token vendor invoice at all, matching CostClassOwn's own
    -- "cost_basis is unattributed" precedent rather than inventing a zero
    -- that would look like a real, known-zero cost).
    vendor_input_cost_per_million  DECIMAL(12,4) CHECK (vendor_input_cost_per_million IS NULL OR vendor_input_cost_per_million >= 0),
    vendor_output_cost_per_million DECIMAL(12,4) CHECK (vendor_output_cost_per_million IS NULL OR vendor_output_cost_per_million >= 0),
    -- An admin can register a model before it is actually routable (e.g.
    -- while capacity is still being mounted) or retire one without deleting
    -- its pricing/audit history -- the router only ever considers
    -- enabled=true models.
    enabled                  BOOLEAN NOT NULL DEFAULT TRUE,
    updated_by               TEXT,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMIT;
