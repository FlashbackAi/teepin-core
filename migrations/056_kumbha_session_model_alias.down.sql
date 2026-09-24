-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

ALTER TABLE billing.inference_sessions
    DROP COLUMN IF EXISTS model_alias;
