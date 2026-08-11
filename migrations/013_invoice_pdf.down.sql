-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Dropping these columns forgets WHERE each stored PDF lives, but does
-- NOT delete the objects in S3 — the bucket has no delete permission by
-- design (immutability by IAM). Re-applying 013 and re-issuing would
-- re-render fresh documents; the orphaned originals remain in the bucket
-- as prior object versions, which is the intended belt-and-braces.
ALTER TABLE billing.invoices
    DROP COLUMN IF EXISTS pdf_s3_key,
    DROP COLUMN IF EXISTS pdf_generated_at;
