-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Guards against two overlapping "deploy" calls for the SAME session
-- racing each other. Found live 2026-09-01: an agent facing what looked
-- like a stuck redeploy called deploy again before the first attempt had
-- finished; both ran concurrently, each independently called
-- SetLastDeployStatus on its own completion, and whichever one finished
-- LAST won — regardless of which one was actually reflected in the live
-- pod. A genuinely successful, verified-live redeploy was left showing
-- "Last deploy failed" because an earlier, abandoned-in-spirit attempt
-- happened to complete afterward.
--
-- NULL means unlocked. A non-NULL timestamp is treated as locked, UNLESS
-- it is older than deployLockTimeout (pkg/api/kumbha_handlers.go) — a
-- crashed or killed process must not leave a session permanently
-- undeployable; a stale lock is reclaimed rather than held forever.

BEGIN;

ALTER TABLE billing.inference_sessions
    ADD COLUMN IF NOT EXISTS deploy_lock_acquired_at TIMESTAMPTZ;

COMMIT;
