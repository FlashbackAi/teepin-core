-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

BEGIN;

ALTER TABLE billing.inference_sessions
    DROP COLUMN IF EXISTS deploy_lock_acquired_at;

COMMIT;
