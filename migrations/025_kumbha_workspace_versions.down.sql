-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Drops the workspace version history and its pointer column. Safe: this
-- is a cache/convenience layer over what the agent pod holds on its own
-- PVC while a session is live — no billing or audit record depends on it.

BEGIN;

ALTER TABLE billing.inference_sessions
    DROP COLUMN IF EXISTS current_workspace_version;

DROP TABLE IF EXISTS billing.kumbha_workspace_versions;

COMMIT;
