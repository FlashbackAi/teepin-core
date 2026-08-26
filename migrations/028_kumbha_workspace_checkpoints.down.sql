-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

BEGIN;

ALTER TABLE billing.kumbha_workspace_versions
    DROP COLUMN IF EXISTS is_checkpoint;

COMMIT;
