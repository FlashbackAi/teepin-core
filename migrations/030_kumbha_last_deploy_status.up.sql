-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Persists the OUTCOME of a session's most recent build/deploy attempt —
-- before this, a build/deploy failure was only ever shown once, in the
-- moment, to whoever triggered it (the HTTP error response), with no
-- trace left for a later read to find. Without it, the "Previous builds"
-- list has no way to show "Failed" for a session whose latest deploy
-- attempt broke, even though the customer's own currently-running app
-- (from an earlier successful deploy, if any) is untouched and still
-- billing normally — see pkg/api.buildKumbhaImage/DeployKumbhaSession's
-- own doc comments on why a failed build never touches the existing
-- instance.
--
-- Cleared (set back to false/NULL) on the NEXT successful build or
-- deploy, so a stale failure from days ago never lingers once the
-- customer has since shipped successfully.

BEGIN;

ALTER TABLE billing.inference_sessions
    ADD COLUMN IF NOT EXISTS last_deploy_failed BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS last_deploy_error TEXT,
    ADD COLUMN IF NOT EXISTS last_deploy_at TIMESTAMPTZ;

COMMIT;
