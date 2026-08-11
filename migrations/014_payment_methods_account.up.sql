-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Payment methods, account-level credit ledger, and the plumbing for the
-- "no validated card, no resources" gate.
--
-- billing.payment_methods was created (migration 003) keyed by
-- project_id, then migration 007 added account_id NOT NULL and truncated
-- every table — so the table is EMPTY and the legacy project_id column is
-- both wrong (payment belongs to the ACCOUNT, not a project) and safe to
-- drop with no backfill.

BEGIN;

-- ---------------------------------------------------------------
-- 1. payment_methods: drop the legacy project key, add validation state.
-- ---------------------------------------------------------------
ALTER TABLE billing.payment_methods DROP COLUMN IF EXISTS project_id;

-- A row existing is NOT proof the card works — only proof someone typed
-- one in. Only Stripe confirming a SetupIntent makes it spendable, and
-- only status='verified' opens the provisioning gate.
ALTER TABLE billing.payment_methods
    ADD COLUMN IF NOT EXISTS verified_at            TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS stripe_setup_intent_id TEXT,
    ADD COLUMN IF NOT EXISTS status                 VARCHAR(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','verified','failed','removed'));

-- At most one default card per account, and only a verified card may be
-- the default — a pending/failed card must never be what we would charge.
CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_methods_one_default
    ON billing.payment_methods(account_id)
    WHERE is_default AND status = 'verified';

-- ---------------------------------------------------------------
-- 2. accounts: one Stripe customer per account, and the grace clock.
-- ---------------------------------------------------------------
-- stripe_customer_id belongs to the ACCOUNT, not per-card: one Stripe
-- customer per TEEPIN account, N cards beneath it.
--
-- payment_failed_at starts the 24h suspension grace period. It lives on
-- the account, not the card, because the grace clock is a property of
-- the billing relationship and must survive the customer deleting the
-- card that failed. Nothing sets it from charging yet (charging is a
-- later phase) — only an explicit card-failure webhook does — so the
-- suspension sweeper is inert until a card actually goes bad.
ALTER TABLE auth.accounts
    ADD COLUMN IF NOT EXISTS stripe_customer_id TEXT UNIQUE,
    ADD COLUMN IF NOT EXISTS payment_failed_at  TIMESTAMPTZ;

-- ---------------------------------------------------------------
-- 3. Credit ledger — APPEND-ONLY.
-- ---------------------------------------------------------------
-- Grants are positive rows, consumption negative rows; balance is the
-- SUM. Rows are NEVER updated or deleted. An operator can mint spendable
-- value here, so "who granted what, when, and why" must be answerable
-- forever — the same immutability discipline as invoices.
CREATE TABLE billing.credit_transactions (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    account_id      UUID NOT NULL REFERENCES auth.accounts(id) ON DELETE RESTRICT,
    -- Positive for a grant, negative for consumption/expiry/revocation.
    amount          DECIMAL(12,4) NOT NULL,
    kind            VARCHAR(20) NOT NULL
        CHECK (kind IN ('grant','consumption','expiry','revocation')),
    -- Required: an unexplained credit is indistinguishable from fraud in
    -- an audit.
    reason          TEXT NOT NULL,
    -- Operator identity, on grants only.
    granted_by      TEXT,
    -- Grants only; NULL means it never expires.
    expires_at      TIMESTAMPTZ,
    -- Links a consumption row to the metered interval it paid for.
    usage_record_id UUID REFERENCES billing.usage_records(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_credit_tx_account
    ON billing.credit_transactions(account_id, created_at);

-- One consumption row per usage record: makes credit application
-- idempotent if the collector re-processes an interval.
CREATE UNIQUE INDEX idx_credit_tx_usage
    ON billing.credit_transactions(usage_record_id)
    WHERE usage_record_id IS NOT NULL;

COMMIT;
