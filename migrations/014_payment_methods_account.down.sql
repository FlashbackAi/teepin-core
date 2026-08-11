-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverses 014. Dropping credit_transactions destroys the credit ledger;
-- acceptable only because this migration predates any real granted
-- credit. Re-adding payment_methods.project_id restores the legacy
-- column as nullable — it cannot be NOT NULL again without a project to
-- point every row at, and the account-level design never repopulates it.

BEGIN;

DROP TABLE IF EXISTS billing.credit_transactions;

ALTER TABLE auth.accounts
    DROP COLUMN IF EXISTS stripe_customer_id,
    DROP COLUMN IF EXISTS payment_failed_at;

DROP INDEX IF EXISTS billing.idx_payment_methods_one_default;

ALTER TABLE billing.payment_methods
    DROP COLUMN IF EXISTS verified_at,
    DROP COLUMN IF EXISTS stripe_setup_intent_id,
    DROP COLUMN IF EXISTS status,
    ADD COLUMN IF NOT EXISTS project_id UUID REFERENCES auth.projects(id) ON DELETE CASCADE;

COMMIT;
