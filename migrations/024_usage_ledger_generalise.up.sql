-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Generalises billing.usage_records from "always a compute instance" to a
-- polymorphic subject, because inference (Kumbha) is the second billable
-- thing that is not a compute instance, and it will not be the last —
-- object storage, managed Postgres, NoSQL and bandwidth are all coming.
-- See KUMBHA-DESIGN.md's "Migration 024" section for the full reasoning,
-- including the ON DELETE CASCADE billing-integrity finding this fixes.

BEGIN;

ALTER TABLE billing.usage_records
    ADD COLUMN IF NOT EXISTS subject_type VARCHAR(40),
    ADD COLUMN IF NOT EXISTS subject_id   VARCHAR(100),
    -- What this usage cost TEEPIN, in the same currency as total_cost. 0
    -- for own-compute/own-inference until GPU-hour attribution exists; the
    -- provider's actual price for a resold third-party model.
    ADD COLUMN IF NOT EXISTS cost_basis   DECIMAL(14,6) NOT NULL DEFAULT 0,
    -- Which backend served it: 'vllm' | 'anthropic' | 'openai' |
    -- 'teepin-compute'.
    ADD COLUMN IF NOT EXISTS provider     VARCHAR(40);

-- Backfill every existing row into the new shape before subject_type/id
-- become NOT NULL below.
UPDATE billing.usage_records
   SET subject_type = 'instance',
       subject_id   = instance_id,
       provider     = 'teepin-compute'
 WHERE subject_type IS NULL;

ALTER TABLE billing.usage_records
    ALTER COLUMN subject_type SET NOT NULL,
    ALTER COLUMN subject_id   SET NOT NULL;

-- instance_id is retired, not dropped in this migration: keeping it one
-- release lets the reconciler, invoice renderer and any operator query
-- move over without a flag day. It also loses its ON DELETE CASCADE via
-- this nullability change staying in place — a hard-delete of an instance
-- row no longer implicitly deletes its billing history, which was a live
-- (if never yet triggered) billing-integrity bug: a financial record must
-- outlive the resource it describes.
ALTER TABLE billing.usage_records
    ALTER COLUMN instance_id DROP NOT NULL;

-- quantity/unit_price/total_cost at DECIMAL(10,4) overflow on raw token
-- counts above ~1M and round a per-1K-token rate to zero. Widened so units
-- become a per-service choice rather than a workaround for column limits.
ALTER TABLE billing.usage_records
    ALTER COLUMN quantity   TYPE DECIMAL(20,8),
    ALTER COLUMN unit_price TYPE DECIMAL(12,8),
    ALTER COLUMN total_cost TYPE DECIMAL(14,6);

CREATE INDEX IF NOT EXISTS idx_usage_subject  ON billing.usage_records(subject_type, subject_id);
CREATE INDEX IF NOT EXISTS idx_usage_provider ON billing.usage_records(provider, created_at);

-- Rates default to 0, matching every other dimension: metering ships on,
-- nothing bills until an operator sets a rate.
ALTER TABLE billing.pricing
    ADD COLUMN IF NOT EXISTS llm_price_per_million_input  DECIMAL(12,8) NOT NULL DEFAULT 0
        CHECK (llm_price_per_million_input  >= 0),
    ADD COLUMN IF NOT EXISTS llm_price_per_million_output DECIMAL(12,8) NOT NULL DEFAULT 0
        CHECK (llm_price_per_million_output >= 0);

-- One row per Kumbha build session: pre-authorised budget, live spend, and
-- (once the agent runtime lands) the underlying agent pod and whether the
-- customer has approved real infrastructure spend — see the Kumbha plan's
-- decisions 2 and 5.
CREATE TABLE billing.inference_sessions (
    id                 UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    account_id         UUID NOT NULL REFERENCES auth.accounts(id) ON DELETE CASCADE,
    project_id         UUID NOT NULL REFERENCES auth.projects(id) ON DELETE CASCADE,
    budget             DECIMAL(14,6) NOT NULL CHECK (budget >= 0),
    spent              DECIMAL(14,6) NOT NULL DEFAULT 0 CHECK (spent >= 0),
    status             VARCHAR(20)   NOT NULL DEFAULT 'open',
    label              TEXT,
    -- The Kumbha agent pod backing this session, if one has been launched.
    -- Deliberately NOT a compute.instances row — see the Kumbha plan's
    -- decision 2: the agent's own workload is not a customer-managed
    -- resource, so it is tracked here instead.
    agent_instance_id  VARCHAR(50),
    -- Gates the MCP tool server's provisioning verbs (create_instance,
    -- deploy, attach_domain): false until the customer explicitly approves
    -- the itemised infrastructure cost estimate. A hard backend gate, not
    -- model-trust — see the Kumbha plan's decision 5.
    deploy_approved    BOOLEAN       NOT NULL DEFAULT false,
    started_at         TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    ended_at           TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_inference_sessions_account ON billing.inference_sessions(account_id, started_at);
CREATE INDEX IF NOT EXISTS idx_inference_sessions_project ON billing.inference_sessions(project_id, started_at);

-- Per-(route, direction) token accumulation within a session. A session
-- can touch more than one route (teepin/fast for grunt work, teepin/deep
-- for planning, per the hybrid-routing policy), and the customer's
-- eventual invoice line needs "kumbha/fast:input" / "kumbha/fast:output"
-- broken out separately from "kumbha/deep:*" — a single running total on
-- inference_sessions could not reconstruct that split at session close.
CREATE TABLE billing.inference_session_usage (
    session_id    UUID NOT NULL REFERENCES billing.inference_sessions(id) ON DELETE CASCADE,
    route         VARCHAR(40) NOT NULL,
    provider      VARCHAR(40) NOT NULL,
    input_tokens  BIGINT NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens BIGINT NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    PRIMARY KEY (session_id, route)
);

COMMIT;
