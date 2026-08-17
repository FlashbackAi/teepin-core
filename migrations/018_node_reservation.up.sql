-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Stage 2.5 of the home-compute pilot: rentable capacity.
--
-- A home node's DETECTED specs (cpu_cores, memory_gb, added in migration 016)
-- are the CEILING. These new columns are what the operator chooses to OFFER
-- for rent -- a machine the operator also uses should not rent out everything.
--
-- Default 0: a node offers NOTHING until the operator sets a reservation, so a
-- freshly enrolled node is never silently rented out. Placement (PlaceCPU)
-- treats a node with 0 rentable capacity as ineligible.
--
-- "Used" capacity is not stored here -- it is derived at read time by summing
-- cpu_units/memory_gb over the node's active instances (compute.instances,
-- joined on node_id), so it can never drift from the actual running workloads.
-- Free = rentable - used.

BEGIN;

ALTER TABLE compute.nodes
    ADD COLUMN IF NOT EXISTS rentable_cpu_cores INT NOT NULL DEFAULT 0
        CHECK (rentable_cpu_cores >= 0),
    ADD COLUMN IF NOT EXISTS rentable_memory_gb INT NOT NULL DEFAULT 0
        CHECK (rentable_memory_gb >= 0);

COMMIT;
