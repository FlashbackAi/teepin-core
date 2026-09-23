-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Restores the two Kumbha tables empty -- the candidates migrated into
-- inference.models are not moved back, so Kumbha falls back to its
-- env-var-configured routes until candidates are re-added by hand. The
-- third-party models themselves are removed from the catalog, since the
-- columns that describe how to reach them are dropped.

BEGIN;

DELETE FROM inference.models WHERE provider <> 'node';

DROP INDEX IF EXISTS inference.idx_inference_models_kumbha;

ALTER TABLE inference.models
    DROP COLUMN provider,
    DROP COLUMN provider_model,
    DROP COLUMN base_url,
    DROP COLUMN max_output_tokens,
    DROP COLUMN api_key_ref,
    DROP COLUMN offered_to_customers,
    DROP COLUMN kumbha_enabled,
    DROP COLUMN kumbha_priority;

CREATE TABLE IF NOT EXISTS billing.kumbha_routes (
    route_name TEXT PRIMARY KEY,
    enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    updated_by TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS billing.kumbha_route_candidates (
    id                UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    route_name        TEXT NOT NULL,
    priority          INT NOT NULL DEFAULT 0,
    provider_type     TEXT NOT NULL CHECK (provider_type IN ('vllm', 'anthropic')),
    base_url          TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL,
    context_window    INT NOT NULL DEFAULT 0 CHECK (context_window >= 0),
    supports_tools    BOOLEAN NOT NULL DEFAULT TRUE,
    max_output_tokens INT NOT NULL DEFAULT 4096 CHECK (max_output_tokens > 0),
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    has_secret        BOOLEAN NOT NULL DEFAULT FALSE,
    updated_by        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_kumbha_route_candidates_route ON billing.kumbha_route_candidates(route_name, priority);

COMMIT;
