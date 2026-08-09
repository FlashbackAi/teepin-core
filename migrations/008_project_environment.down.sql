-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

ALTER TABLE auth.projects
    DROP CONSTRAINT IF EXISTS projects_environment_check;

ALTER TABLE auth.projects
    DROP COLUMN IF EXISTS environment;
