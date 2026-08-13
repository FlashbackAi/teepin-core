-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverses 017. Part of the home-compute pilot's archivability path. The
-- CPU/memory rate columns and the instance provider_id are dropped; the
-- VRAM rate (added in migration 006) is left in place, as it predates this.

BEGIN;

ALTER TABLE billing.pricing
    DROP COLUMN IF EXISTS cpu_price_per_core_hour,
    DROP COLUMN IF EXISTS memory_price_per_gb_hour;

ALTER TABLE compute.instances
    DROP COLUMN IF EXISTS provider_id;

COMMIT;
