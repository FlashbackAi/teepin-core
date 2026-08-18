-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverses 021.

BEGIN;

ALTER TABLE compute.instances
    DROP COLUMN IF EXISTS container_port;

COMMIT;
