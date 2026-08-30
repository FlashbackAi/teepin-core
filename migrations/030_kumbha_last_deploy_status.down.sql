-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

BEGIN;

ALTER TABLE billing.inference_sessions
    DROP COLUMN IF EXISTS last_deploy_failed,
    DROP COLUMN IF EXISTS last_deploy_error,
    DROP COLUMN IF EXISTS last_deploy_at;

COMMIT;
