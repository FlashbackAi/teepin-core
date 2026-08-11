-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- A stored, rendered PDF is the DOCUMENT the customer holds — distinct
-- from the invoice ROW (subtotal/tax/total/line items as data) that
-- already exists. The row can be re-read; the document must be preserved
-- verbatim, because an invoice is a financial record and must be
-- byte-for-byte what was sent even if the rendering template changes
-- later (see INVOICE-DESIGN.md, "What an invoice actually has to be").
--
-- Both columns are set ONCE, inside IssueInvoice (draft -> open), and
-- never overwritten — the same write-once discipline as the bill_to_*
-- snapshot columns added in migration 011. They stay NULL for drafts
-- (no document exists until an invoice is issued) and for invoices
-- issued while S3 storage was not configured (local dev), which is why
-- neither is NOT NULL.
--
-- pdf_s3_key holds the object key, not a URL: the bucket is deployment
-- config (an env var), and a stored absolute URL would rot the moment
-- the bucket moved. The key ({account_id}/{invoice_number}.pdf) is the
-- stable per-invoice identity. Reads happen through a short-lived
-- presigned URL minted on demand, never a persisted link.
ALTER TABLE billing.invoices
    ADD COLUMN IF NOT EXISTS pdf_s3_key       TEXT,
    ADD COLUMN IF NOT EXISTS pdf_generated_at TIMESTAMPTZ;
