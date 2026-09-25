-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Replaces Kumbha's alias/tier abstraction with direct model selection.
--
-- Migration 055's kumbha_alias modeled "which bucket" (Fast/Deep/
-- Confidential) as one mutually-exclusive category per model — which does
-- not match reality: a model can be both tool-capable and confidential, or
-- neither, and a bucket with more than one backing model could fail over
-- from a customer's deliberately-chosen confidential model to a DIFFERENT,
-- non-confidential one, silently. A customer now picks a model directly,
-- by its real route, shown with independent badges (Confidential,
-- Self-hosted vs third-party) computed from its existing Provider field —
-- no separate category column needed for that.
--
-- kumbha_priority is kept: it's still meaningful as the picker's default
-- display/recommendation order, just no longer a failover sequence within
-- a bucket.
--
-- Constraint names are looked up rather than assumed (Postgres's own
-- auto-naming for an inline CHECK is table_column_check, but this doesn't
-- trust that rather than verify it) — same reasoning migration 012's own
-- dynamic FK-constraint lookup uses.
DO $$
DECLARE
    c_name TEXT;
BEGIN
    SELECT conname INTO c_name
    FROM pg_constraint
    WHERE conrelid = 'inference.models'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%kumbha_alias%';
    IF c_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE inference.models DROP CONSTRAINT %I', c_name);
    END IF;
END $$;

DROP INDEX IF EXISTS idx_inference_models_kumbha_alias;

ALTER TABLE inference.models
    DROP COLUMN IF EXISTS kumbha_alias;

-- billing.inference_sessions.model_alias (migration 056) becomes
-- model_route: the exact model a customer picked, bound for the session's
-- whole lifetime — never one of three fixed alias strings, so the CHECK
-- constraint restricting it to those three is dropped along with the
-- rename.
DO $$
DECLARE
    c_name TEXT;
BEGIN
    SELECT conname INTO c_name
    FROM pg_constraint
    WHERE conrelid = 'billing.inference_sessions'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%model_alias%';
    IF c_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE billing.inference_sessions DROP CONSTRAINT %I', c_name);
    END IF;
END $$;

ALTER TABLE billing.inference_sessions
    RENAME COLUMN model_alias TO model_route;
