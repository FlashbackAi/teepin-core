-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverses 045.

BEGIN;

ALTER TABLE billing.pricing
    DROP COLUMN IF EXISTS p_core_price_per_hour,
    DROP COLUMN IF EXISTS e_core_price_per_hour;

ALTER TABLE compute.instances
    DROP COLUMN IF EXISTS p_cores_used,
    DROP COLUMN IF EXISTS e_cores_used;

COMMIT;
