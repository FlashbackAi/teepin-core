-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- NOTE: reversing this loses no data (project_id stays populated where
-- it was), but re-adding NOT NULL will FAIL if any account-level
-- invoice (project_id IS NULL) was created while this migration was
-- applied. Delete or backfill those rows first if downgrading after use.

ALTER TABLE billing.invoice_line_items
    ALTER COLUMN unit SET DEFAULT 'item',
    ALTER COLUMN unit SET NOT NULL;

ALTER TABLE billing.invoice_line_items
    ALTER COLUMN quantity SET DEFAULT 1,
    ALTER COLUMN quantity SET NOT NULL;

ALTER TABLE billing.invoice_line_items
    ALTER COLUMN unit_price SET NOT NULL;

DROP INDEX IF EXISTS billing.idx_invoice_line_items_project;

ALTER TABLE billing.invoice_line_items
    DROP COLUMN IF EXISTS project_id;

ALTER TABLE billing.invoices
    ALTER COLUMN project_id SET NOT NULL;

-- Restore the original (unsafe) CASCADE behaviour on downgrade, so a
-- rollback returns the schema to exactly what migration 011 left
-- behind, not a hybrid.
ALTER TABLE billing.usage_records
    DROP CONSTRAINT IF EXISTS usage_records_project_id_fkey;
ALTER TABLE billing.usage_records
    ADD CONSTRAINT usage_records_project_id_fkey
    FOREIGN KEY (project_id) REFERENCES auth.projects(id) ON DELETE CASCADE;

ALTER TABLE billing.invoices
    DROP CONSTRAINT IF EXISTS invoices_project_id_fkey;
ALTER TABLE billing.invoices
    ADD CONSTRAINT invoices_project_id_fkey
    FOREIGN KEY (project_id) REFERENCES auth.projects(id) ON DELETE CASCADE;
