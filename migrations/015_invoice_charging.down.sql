-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverses 015. Dropping these columns loses the charge-attempt history
-- and the PaymentIntent linkage; acceptable only because this migration
-- predates any real collected charge. stripe_invoice_id (added earlier)
-- is left in place — it is not owned by this migration.

BEGIN;

DROP INDEX IF EXISTS billing.idx_invoices_one_payment_intent;

ALTER TABLE billing.invoices
    DROP COLUMN IF EXISTS charge_attempts,
    DROP COLUMN IF EXISTS last_charge_attempt_at,
    DROP COLUMN IF EXISTS last_charge_error,
    DROP COLUMN IF EXISTS stripe_payment_intent_id;

COMMIT;
