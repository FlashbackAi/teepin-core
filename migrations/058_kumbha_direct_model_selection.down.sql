-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverts to migration 055/056's alias columns. Fails if any row's
-- model_route/model_route is not one of the three original alias strings
-- — a customer's own directly-picked model route must be re-pointed or
-- cleared first, same as any down-migration that re-narrows a column.

ALTER TABLE billing.inference_sessions
    RENAME COLUMN model_route TO model_alias;

ALTER TABLE billing.inference_sessions
    ADD CONSTRAINT inference_sessions_model_alias_check
    CHECK (model_alias IN ('teepin/fast', 'teepin/deep', 'teepin/confidential'));

ALTER TABLE inference.models
    ADD COLUMN kumbha_alias TEXT NOT NULL DEFAULT 'teepin/fast'
        CHECK (kumbha_alias IN ('teepin/fast', 'teepin/deep', 'teepin/confidential'));

CREATE INDEX idx_inference_models_kumbha_alias
    ON inference.models (kumbha_alias, kumbha_priority)
    WHERE enabled AND kumbha_enabled;
