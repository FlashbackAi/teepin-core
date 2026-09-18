-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- The generic, Control-Center-driven mount/unmount primitive for anything
-- Teepin itself (not a customer) runs on a node -- an inference model server
-- today, an agent binary update tomorrow. Deliberately general-purpose from
-- the start, not inference-specific with room left for other kinds: a
-- `kind` discriminator plus a free-form `config` is the whole shape, and
-- both kinds below are first-class, not one primary and one hypothetical.
--
-- Deliberately a NEW table, not a reuse of compute.instances: that table is
-- customer-owned and billed to a project. This is operator-owned, lives at
-- the node level, and bolting nullable customer-billing columns onto it to
-- fit a non-customer workload would be the wrong shape (see the roadmap
-- entry this migration implements for the fuller reasoning).
--
-- desired_state is what Control Center asked for; observed_state is what
-- whatever actually runs the thing last reported -- the same desired-vs-
-- observed split compute.instances already uses, reconciled the same way
-- (see pkg/nodeservices, not yet built at this migration's time, and the
-- reconciliation discipline already hardened for compute.instances).
--
-- Row is kept (desired_state flips to 'unmounted'), never deleted, on an
-- unmount -- history and idempotent re-mounting both want the row to still
-- exist rather than being recreated from nothing.

BEGIN;

CREATE TABLE compute.node_services (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    node_id        UUID NOT NULL REFERENCES compute.nodes(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL CHECK (kind IN ('inference_model', 'agent_binary')),
    -- Kind-specific shape (e.g. {"model_route": "...", "engine": "vllm"} for
    -- inference_model, {"version": "..."} for agent_binary) -- deliberately
    -- opaque to this table so new kinds never need a schema change here.
    config         JSONB NOT NULL DEFAULT '{}'::jsonb,
    desired_state  TEXT NOT NULL DEFAULT 'mounted' CHECK (desired_state IN ('mounted', 'unmounted')),
    observed_state TEXT NOT NULL DEFAULT 'pending' CHECK (observed_state IN ('pending', 'mounted', 'unmounted', 'error')),
    observed_error TEXT,
    observed_at    TIMESTAMPTZ,
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_node_services_node_id ON compute.node_services(node_id);
CREATE INDEX idx_node_services_kind ON compute.node_services(kind);

COMMIT;
