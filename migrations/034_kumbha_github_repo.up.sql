-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Records the Teepin-owned GitHub repo (under the TeepinWebServices org)
-- each Kumbha session's code is pushed to at checkpoint time, alongside
-- billing.kumbha_workspace_versions. Set once, lazily, on the session's
-- first checkpoint (pkg/githubstore.Service.ProvisionRepo), reused on
-- every later push rather than re-provisioning. Nullable: the feature is
-- optional (TEEPIN_GITHUB_APP_* unset means no repo is ever created), and
-- a push failure must never block a deploy — see pkg/kumbha's own
-- SetGithubRepo/GetGithubRepo doc comments.
--
-- Deliberately NEVER selected by GetSession or exposed through
-- kumbhaSessionResponse (pkg/api/kumbha_handlers.go) — the customer must
-- never see this repo exists, only ever the ZIP download. Read/written
-- through narrow, dedicated Store methods instead of the shared Session
-- struct precisely so no existing SELECT needs to change and no existing
-- response-building code accidentally gains a new field to leak.

BEGIN;

ALTER TABLE billing.inference_sessions
    ADD COLUMN IF NOT EXISTS github_repo VARCHAR(255);

COMMIT;
