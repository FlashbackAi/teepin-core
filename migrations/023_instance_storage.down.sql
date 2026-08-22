-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverses 023.

BEGIN;

ALTER TABLE billing.pricing
    DROP COLUMN IF EXISTS storage_price_per_gb_month;

ALTER TABLE compute.instances
    DROP COLUMN IF EXISTS storage_gb;

COMMIT;
