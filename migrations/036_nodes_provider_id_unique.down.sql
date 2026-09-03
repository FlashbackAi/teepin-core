-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

BEGIN;

ALTER TABLE compute.nodes
    DROP CONSTRAINT IF EXISTS nodes_provider_id_key;

COMMIT;
