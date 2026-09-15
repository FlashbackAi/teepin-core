-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverses 044.

BEGIN;

ALTER TABLE compute.nodes
    DROP COLUMN IF EXISTS p_cores,
    DROP COLUMN IF EXISTS e_cores;

COMMIT;
