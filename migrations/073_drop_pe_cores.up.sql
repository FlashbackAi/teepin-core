-- The P-core/E-core split (migrations 044 and 045) is removed: home-node CPU
-- is sold and billed as plain cores again (owner decision, 2026-10-03). Every
-- column it added goes. A home instance is billed on cpu_units, which it always
-- carried, so no billing record depends on these.

ALTER TABLE billing.pricing
    DROP COLUMN IF EXISTS p_core_price_per_hour,
    DROP COLUMN IF EXISTS e_core_price_per_hour;

ALTER TABLE compute.instances
    DROP COLUMN IF EXISTS p_cores_used,
    DROP COLUMN IF EXISTS e_cores_used;

ALTER TABLE compute.nodes
    DROP COLUMN IF EXISTS p_cores,
    DROP COLUMN IF EXISTS e_cores;
