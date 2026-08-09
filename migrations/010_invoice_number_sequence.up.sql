-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- The gapless sequential invoice number the service code already
-- expects (billing.Service.nextInvoiceNumber, format INV-000001).
--
-- Migration 009 shipped the numbering CODE without the sequence it
-- reads from, so the first real call failed with:
--   "failed to allocate invoice number: pq: relation
--    billing.invoice_number_seq does not exist"
--
-- Seeded past the current row count (not started at 1) so a platform
-- with existing invoices never reissues a number already on file —
-- reuse would be indistinguishable from a bug when this is reconciled
-- for tax.
DO $$
DECLARE
    existing_count BIGINT;
BEGIN
    SELECT COUNT(*) INTO existing_count FROM billing.invoices;

    EXECUTE format(
        'CREATE SEQUENCE IF NOT EXISTS billing.invoice_number_seq START WITH %s',
        existing_count + 1
    );
END $$;
