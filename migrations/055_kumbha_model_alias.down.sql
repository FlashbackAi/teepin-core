-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

DROP INDEX IF EXISTS idx_inference_models_kumbha_alias;

ALTER TABLE inference.models
    DROP COLUMN IF EXISTS kumbha_alias;
