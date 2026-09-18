-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverses 049.

BEGIN;

ALTER TABLE compute.node_services
    DROP COLUMN IF EXISTS observed_endpoint;

COMMIT;
