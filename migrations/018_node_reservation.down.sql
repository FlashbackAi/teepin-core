-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverses 018. Part of the home-compute pilot's archivability path: dropping
-- the rentable columns leaves the node's detected specs (migration 016)
-- untouched.

BEGIN;

ALTER TABLE compute.nodes
    DROP COLUMN IF EXISTS rentable_cpu_cores,
    DROP COLUMN IF EXISTS rentable_memory_gb;

COMMIT;
