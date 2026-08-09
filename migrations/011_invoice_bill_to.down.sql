-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

ALTER TABLE billing.invoices
    DROP COLUMN IF EXISTS bill_to_name,
    DROP COLUMN IF EXISTS bill_to_email,
    DROP COLUMN IF EXISTS bill_to_address,
    DROP COLUMN IF EXISTS bill_to_tax_id,
    DROP COLUMN IF EXISTS bill_to_account_number;
