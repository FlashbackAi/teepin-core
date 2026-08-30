-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- The Deploy button's no-op guard (CodePanel, code-panel.tsx) was reading
-- the current version's is_checkpoint flag as a proxy for "this exact
-- content is already running" — true for the agent's OWN write pattern
-- (SaveVersion reuses a draft row in place until CheckpointWorkspace
-- flips it at a successful deploy, so "checkpointed" and "deployed" were
-- the same fact there), but is_checkpoint is ALSO set immediately on any
-- CUSTOMER save (so an edit shows up in History right away, without
-- waiting for a redeploy) — a write that has obviously never been
-- deployed. Found live 2026-08-31: a customer edited text in the IDE,
-- saved, and the Deploy button immediately disabled itself with "Already
-- deployed — nothing has changed since the last deploy," for a version
-- that had in fact never been deployed at all.
--
-- last_deployed_version is a second, narrower signal: the version number
-- CheckpointCurrentVersion actually stamped at a REAL successful deploy —
-- set only there, alongside (not instead of) is_checkpoint, which keeps
-- its existing "worth showing in History" meaning unchanged.

BEGIN;

ALTER TABLE billing.inference_sessions
    ADD COLUMN IF NOT EXISTS last_deployed_version INTEGER;

COMMIT;
