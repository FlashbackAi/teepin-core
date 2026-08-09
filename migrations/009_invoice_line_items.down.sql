-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

DROP TABLE IF EXISTS billing.invoice_line_items;

ALTER TABLE billing.invoices DROP CONSTRAINT IF EXISTS invoices_status_check;
ALTER TABLE billing.invoices DROP CONSTRAINT IF EXISTS invoices_source_check;

ALTER TABLE billing.invoices DROP COLUMN IF EXISTS currency;
ALTER TABLE billing.invoices DROP COLUMN IF EXISTS due_date;
ALTER TABLE billing.invoices DROP COLUMN IF EXISTS notes;
ALTER TABLE billing.invoices DROP COLUMN IF EXISTS issued_by;
ALTER TABLE billing.invoices DROP COLUMN IF EXISTS source;
