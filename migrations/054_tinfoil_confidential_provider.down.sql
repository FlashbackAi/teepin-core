-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverts to migration 053's original constraint. Fails if any row already
-- has provider = 'tinfoil_confidential' — that row must be deleted or
-- re-pointed first, same as any down-migration that narrows a check
-- constraint's allowed values.
ALTER TABLE inference.models
    DROP CONSTRAINT IF EXISTS models_provider_check;

ALTER TABLE inference.models
    ADD CONSTRAINT models_provider_check
    CHECK (provider IN ('node', 'anthropic', 'openai_compatible'));
