-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- The bill-to snapshot columns billing.Service already reads and writes
-- (resolveBillTo / CreateManualInvoice), which no earlier migration
-- created — the same class of gap as migration 010's missing sequence.
-- The first manual invoice attempt failed with:
--   "column \"bill_to_name\" of relation \"invoices\" does not exist"
--
-- SNAPSHOTTED at issue time, not read live from auth.accounts: a
-- customer who renames their company or updates their tax ID next year
-- must not retroactively alter an invoice already sent. That is what
-- makes an invoice a record instead of a rendering of current state —
-- the same reasoning as bill_to_account_number carrying the account
-- number as of issue, even though accounts are never renumbered.
ALTER TABLE billing.invoices
    ADD COLUMN IF NOT EXISTS bill_to_name             VARCHAR(255),
    ADD COLUMN IF NOT EXISTS bill_to_email             VARCHAR(255),
    ADD COLUMN IF NOT EXISTS bill_to_address           TEXT,
    ADD COLUMN IF NOT EXISTS bill_to_tax_id            VARCHAR(64),
    ADD COLUMN IF NOT EXISTS bill_to_account_number    VARCHAR(32);
