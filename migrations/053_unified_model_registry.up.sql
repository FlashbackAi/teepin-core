-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- One model registry for the whole platform.
--
-- Before this, two registries described the same models: inference.models
-- (Teepin Inference's customer catalog) and billing.kumbha_route_candidates
-- (Kumbha's own per-route backends), plus env-var-configured routes that no
-- admin page showed at all. Kumbha's "teepin/fast" even appeared in the
-- customer catalog as a vllm model while actually being served by a
-- third-party model registered only as a Kumbha candidate. Now every model
-- -- self-hosted on Teepin's nodes or served by a third-party API -- is one
-- inference.models row, and where it may be used is a property of that row:
-- offered_to_customers (Teepin Inference) and kumbha_enabled (the build
-- agent, tried in kumbha_priority order).

BEGIN;

ALTER TABLE inference.models
    -- How the model is served. 'node': self-hosted, mounted on Teepin's own
    -- nodes via compute.node_services (the only mode before this). 'anthropic'
    -- and 'openai_compatible': a third-party API the gateway calls directly.
    ADD COLUMN provider TEXT NOT NULL DEFAULT 'node'
        CHECK (provider IN ('node', 'anthropic', 'openai_compatible')),
    -- The provider's own model id (e.g. "claude-haiku-4-5-20251001"), which
    -- model_route deliberately is not. Unused for 'node' models, whose
    -- backend model name lives on each mount's node_services config.
    ADD COLUMN provider_model TEXT NOT NULL DEFAULT '',
    -- Endpoint root for 'openai_compatible'; unused otherwise.
    ADD COLUMN base_url TEXT NOT NULL DEFAULT '',
    -- Anthropic requires max_tokens on every request; used when a caller
    -- leaves it unset. Never 0 -- the API rejects that.
    ADD COLUMN max_output_tokens INT NOT NULL DEFAULT 4096 CHECK (max_output_tokens > 0),
    -- Name of the Secrets Manager secret holding this model's API key,
    -- WITHOUT the "teepin/<environment>/" prefix (a migration cannot know the
    -- environment; the app adds it). NULL means no key is stored. The key
    -- itself never lives in the database.
    ADD COLUMN api_key_ref TEXT,
    ADD COLUMN offered_to_customers BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN kumbha_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    -- Lower tried first among kumbha_enabled models.
    ADD COLUMN kumbha_priority INT NOT NULL DEFAULT 0;

-- "teepin/fast" / "teepin/deep" were Kumbha's own route aliases, registered
-- into the customer catalog at boot only so Kumbha could look up a price.
-- They are not models; the build agent now draws from the catalog directly.
DELETE FROM inference.models WHERE model_route IN ('teepin/fast', 'teepin/deep');

-- Every Kumbha backend candidate becomes a catalog model. Its existing API
-- key stays exactly where it is in Secrets Manager -- api_key_ref simply
-- points at it. Only candidates on teepin/fast join Kumbha's model list:
-- that is the route the build agent actually used (teepin/deep candidates
-- were never reached by it). None are offered to customers until an
-- operator chooses to; they were Kumbha-only before this.
INSERT INTO inference.models (
    model_route, display_name, cost_class, engine, context_window, supports_tools,
    enabled, provider, provider_model, base_url, max_output_tokens, api_key_ref,
    offered_to_customers, kumbha_enabled, kumbha_priority, updated_by)
SELECT
    CASE c.provider_type WHEN 'anthropic' THEN 'anthropic/' ELSE 'external/' END || c.model,
    c.model,
    CASE c.provider_type WHEN 'anthropic' THEN 'frontier' ELSE 'own' END,
    c.provider_type,
    c.context_window,
    c.supports_tools,
    c.enabled,
    CASE c.provider_type WHEN 'anthropic' THEN 'anthropic' ELSE 'openai_compatible' END,
    c.model,
    c.base_url,
    c.max_output_tokens,
    CASE WHEN c.has_secret THEN 'kumbha-candidate-' || c.id || '-api-key' END,
    FALSE,
    c.route_name = 'teepin/fast' AND COALESCE(r.enabled, TRUE),
    c.priority,
    'migration-053'
FROM billing.kumbha_route_candidates c
LEFT JOIN billing.kumbha_routes r ON r.route_name = c.route_name
ORDER BY (c.route_name = 'teepin/fast') DESC, c.priority, c.created_at
ON CONFLICT (model_route) DO NOTHING;

DROP TABLE billing.kumbha_route_candidates;
DROP TABLE billing.kumbha_routes;

CREATE INDEX idx_inference_models_kumbha ON inference.models (kumbha_priority) WHERE kumbha_enabled;

COMMIT;
