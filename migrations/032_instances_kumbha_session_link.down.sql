-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

BEGIN;

DROP INDEX IF EXISTS compute.idx_instances_kumbha_session;
ALTER TABLE compute.instances
    DROP COLUMN IF EXISTS kumbha_session_id;

COMMIT;
