-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Off-session charging of issued usage invoices.
--
-- The payment-gate phase (migration 014) put a verified card on file and
-- built the inert 24h suspension sweeper. This migration adds the invoice-
-- side bookkeeping the ChargeCollector needs to charge that card, retry a
-- failed charge a few times, and — only when retries are exhausted — arm
-- the grace clock the sweeper already watches.
--
-- stripe_invoice_id (added earlier) still holds the id of the Stripe object
-- that SETTLED the invoice, set only on success. The new
-- stripe_payment_intent_id holds the id of the PaymentIntent we STARTED,
-- set as soon as we create one — the two are deliberately distinct so a
-- crash between "created a PaymentIntent" and "it succeeded" is recoverable
-- (the webhook reconciles by the pi id) rather than double-charging.

BEGIN;

ALTER TABLE billing.invoices
    -- How many times we have tried to charge this invoice. Bounds the
    -- dunning retries and, once exhausted, gates arming the grace clock.
    ADD COLUMN IF NOT EXISTS charge_attempts          INT NOT NULL DEFAULT 0,
    -- When the last charge was attempted, so the collector can back off
    -- between retries rather than hammering a declining card every sweep.
    ADD COLUMN IF NOT EXISTS last_charge_attempt_at   TIMESTAMPTZ,
    -- Human-readable reason the last attempt failed (Stripe decline code /
    -- "no payment method"), surfaced to operators. Never shown to a
    -- customer as-is.
    ADD COLUMN IF NOT EXISTS last_charge_error        TEXT,
    -- The PaymentIntent we started for this invoice. Set before we trust
    -- the create's return, so an interrupted attempt is recoverable.
    ADD COLUMN IF NOT EXISTS stripe_payment_intent_id TEXT;

-- One in-flight PaymentIntent per invoice: makes "create a PaymentIntent
-- for this invoice" idempotent under a racing sweep — a second sweeper
-- cannot start a parallel charge for the same invoice.
CREATE UNIQUE INDEX IF NOT EXISTS idx_invoices_one_payment_intent
    ON billing.invoices(stripe_payment_intent_id)
    WHERE stripe_payment_intent_id IS NOT NULL;

COMMIT;
