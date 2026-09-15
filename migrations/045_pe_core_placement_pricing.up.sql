-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- P-core/E-core-aware placement and pricing for home-node CPU instances,
-- built on the node-level detection added in migration 044.
--
-- compute.instances.p_cores_used/e_cores_used record what an instance
-- actually got, mirroring cpu_units/memory_gb -- placement's free-capacity
-- check (PlaceCPU) needs to SUM these per node the same way it already
-- sums cpu_units, and a customer's own explicit split (or the platform's
-- proportional default when none is given) has to be durable, not
-- recomputed later from nothing. Nullable, unlike cpu_units/memory_gb:
-- an instance placed on a node with no detected P/E split (migration 044's
-- "0/0 means unknown" case) has neither value, by design -- it is billed
-- and placed via the single undifferentiated cpu_units path exactly as
-- before this migration.
--
-- billing.pricing.p_core_price_per_hour/e_core_price_per_hour follow the
-- exact "ships on, bills nothing until an operator sets it" contract every
-- other rate in this table already uses (migration 017's own comment).
-- cpu_price_per_core_hour is UNCHANGED and remains the rate applied to an
-- instance with no p_cores_used/e_cores_used (the undifferentiated path) --
-- these two new columns are additive, not a replacement.

BEGIN;

ALTER TABLE compute.instances
    ADD COLUMN IF NOT EXISTS p_cores_used INT CHECK (p_cores_used IS NULL OR p_cores_used >= 0),
    ADD COLUMN IF NOT EXISTS e_cores_used INT CHECK (e_cores_used IS NULL OR e_cores_used >= 0);

ALTER TABLE billing.pricing
    ADD COLUMN IF NOT EXISTS p_core_price_per_hour DECIMAL(10,4) NOT NULL DEFAULT 0
        CHECK (p_core_price_per_hour >= 0),
    ADD COLUMN IF NOT EXISTS e_core_price_per_hour DECIMAL(10,4) NOT NULL DEFAULT 0
        CHECK (e_core_price_per_hour >= 0);

COMMIT;
