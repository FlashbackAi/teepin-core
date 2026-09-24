-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Persists which Kumbha alias a session was started with, so the agent pod
-- (relaunched on a customer follow-up, or resumed after an idle timeout)
-- is always launched with the SAME TEEPIN_ROUTE the customer actually
-- chose — not a value re-derived or guessed at relaunch time. Defaults to
-- 'teepin/fast': every session created before this migration keeps
-- behaving exactly as it always has (TEEPIN_ROUTE was never even set on
-- the agent pod before this, so run.py's own "teepin/fast" default is
-- what every one of them was already running on).
ALTER TABLE billing.inference_sessions
    ADD COLUMN model_alias TEXT NOT NULL DEFAULT 'teepin/fast'
        CHECK (model_alias IN ('teepin/fast', 'teepin/deep', 'teepin/confidential'));
