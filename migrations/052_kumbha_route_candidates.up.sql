-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Kumbha's own backend candidates, per route, configurable live from
-- Control Center with no redeploy -- see pkg/kumbha/candidates.go.
--
-- A route name ("teepin/fast", "teepin/deep") can now have SEVERAL ranked
-- candidate backends, not exactly one: priority (lower tried first) lets an
-- operator register a second backend for the same route as a fallback, or
-- swap a candidate's config/model without ever touching main.go or
-- terraform. Gateway.Complete tries candidates in priority order and falls
-- through to the next one only on inference.ErrProviderUnavailable (the
-- backend could not be reached / did not respond) -- never on an
-- application-level error, and never mid-stream (Kumbha only ever calls
-- Provider.Complete, which is atomic: either a full response comes back or
-- nothing does, so there is no partial output to double-send).
--
-- The API key itself is deliberately NOT a column here -- it lives in AWS
-- Secrets Manager, named deterministically from this row's id (see
-- pkg/kumbha/secrets.go's CandidateSecretName), and is fetched live by the
-- running process rather than injected once at container start. has_secret
-- only records whether one has ever been set, so the console can show
-- "configured" without ever being able to read the value back.
--
-- billing.kumbha_routes (migration 051) is untouched and keeps its own
-- meaning: route-name-level enable/disable, one level above this table.
-- Disabling a route there still gates every candidate underneath it.

BEGIN;

CREATE TABLE billing.kumbha_route_candidates (
    id                UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    route_name        TEXT NOT NULL,
    -- Lower tried first. Not unique -- an operator may leave two candidates
    -- at the same priority deliberately (no defined order between them);
    -- the important guarantee is total order across DIFFERENT priorities,
    -- not tie-breaking within one.
    priority          INT NOT NULL DEFAULT 0,
    provider_type     TEXT NOT NULL CHECK (provider_type IN ('vllm', 'anthropic')),
    base_url          TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL,
    context_window    INT NOT NULL DEFAULT 0 CHECK (context_window >= 0),
    supports_tools    BOOLEAN NOT NULL DEFAULT TRUE,
    -- Anthropic-specific; ignored for provider_type='vllm'. Never 0 -- an
    -- Anthropic request with max_tokens=0 is rejected by the API itself.
    max_output_tokens INT NOT NULL DEFAULT 4096 CHECK (max_output_tokens > 0),
    -- A candidate can be registered and configured before it is ready to
    -- take traffic (e.g. while its secret is still being set by hand) --
    -- same "exists but not yet routable" distinction migration 047 makes
    -- for inference.models.enabled.
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    has_secret        BOOLEAN NOT NULL DEFAULT FALSE,
    updated_by        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_kumbha_route_candidates_route ON billing.kumbha_route_candidates(route_name, priority);

COMMIT;
