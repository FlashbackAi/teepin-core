-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

BEGIN;

ALTER TABLE billing.pricing
    DROP COLUMN IF EXISTS object_storage_price_per_gb_month,
    DROP COLUMN IF EXISTS object_storage_price_per_gb_egress;

COMMIT;
