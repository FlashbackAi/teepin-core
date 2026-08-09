-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Project environment: dev | staging | prod.
--
-- The console previously INFERRED this from the project name, which is a
-- guess dressed as a fact. The badge exists to stop someone deleting the
-- wrong thing, so it must reflect what the customer declared, not what a
-- regex thought their name meant.
--
-- Nullable rather than defaulted: a project with no declared environment
-- is a real state, and defaulting every existing project to 'dev' would
-- label production workloads wrongly on the day this ships.
ALTER TABLE auth.projects
    ADD COLUMN IF NOT EXISTS environment VARCHAR(16);

-- Constrained at the database rather than only in Go: this column drives
-- a visual warning before destructive actions, and a typo that stored
-- 'prd' would silently show no badge on a production project.
ALTER TABLE auth.projects
    DROP CONSTRAINT IF EXISTS projects_environment_check;

ALTER TABLE auth.projects
    ADD CONSTRAINT projects_environment_check
    CHECK (environment IS NULL OR environment IN ('dev', 'staging', 'prod'));
