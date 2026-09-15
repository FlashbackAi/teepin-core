-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- P-core/E-core split for hybrid consumer CPUs on home nodes, detected by
-- cmd/teepin-hostprobe (Windows/macOS, run on the real host OS before its
-- Linux guest exists) or natively by cmd/teepin-agent's own hostSpecs() on
-- a bare-metal Linux home node.
--
-- Nullable, matching cpu_cores/memory_gb (migration 016) rather than the
-- rentable_* columns (018): these are DETECTED specs, not an operator's
-- offer. NULL/0 both read as "no split detected" -- a homogeneous CPU, a
-- node whose agent predates this feature, or a hypervisor that does not
-- expose real core-type info to its guest -- and are treated identically
-- by nullInt()'s COALESCE-on-reenroll (a transient detection failure on
-- re-enroll must never stomp a previously-detected real split with a
-- fabricated zero). Placement (PlaceCPU, migration 045) treats "no split"
-- as "fall back to the single rentable_cpu_cores scalar, exactly as before
-- this migration" -- nothing regresses for the existing fleet.

BEGIN;

ALTER TABLE compute.nodes
    ADD COLUMN IF NOT EXISTS p_cores INT CHECK (p_cores IS NULL OR p_cores >= 0),
    ADD COLUMN IF NOT EXISTS e_cores INT CHECK (e_cores IS NULL OR e_cores >= 0);

COMMIT;
