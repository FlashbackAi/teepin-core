-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Stores the most recent screenshot of a Kumbha session's deployed app —
-- what the console's Preview tab shows instead of embedding the live site
-- (2026-08-29 product decision). One row per session, overwritten on every
-- successful (re)deploy, same as app_instance_id itself: there is no
-- history of past screenshots, only "what does the current deployment look
-- like right now."
--
-- Stored directly in Postgres (BYTEA), not object storage: this platform
-- has no production object-storage service today (MinIO is local-dev only
-- — see CLAUDE.md), and a single PNG of a browser viewport (tens to a few
-- hundred KB) is trivial row size at Kumbha's session volume — the same
-- reasoning kumbha_workspace_versions already applies to whole workspace
-- snapshots, which are far larger than one screenshot.

BEGIN;

ALTER TABLE billing.inference_sessions
    ADD COLUMN IF NOT EXISTS screenshot BYTEA,
    ADD COLUMN IF NOT EXISTS screenshot_captured_at TIMESTAMPTZ;

COMMIT;
