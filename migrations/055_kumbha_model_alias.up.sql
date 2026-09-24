-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Kumbha's own aliases ("teepin/fast", "teepin/deep", and now
-- "teepin/confidential") were never a property of any model row before
-- this — ListKumbhaModels (pkg/modelcatalog/service.go) returned every
-- kumbha_enabled model in one flat, alias-blind list, so every alias
-- resolved to the exact same backing models. That made offering a real,
-- customer-chosen "Confidential" tier impossible: there was no way to say
-- "only these models back this specific alias." kumbha_alias is that
-- missing property — see [[confidential-inference-hosting-model]].
--
-- Default 'teepin/fast': every model kumbha_enabled before this migration
-- keeps behaving exactly as it did (served under Fast, the only alias that
-- actually did anything), rather than silently vanishing from Kumbha.

ALTER TABLE inference.models
    ADD COLUMN kumbha_alias TEXT NOT NULL DEFAULT 'teepin/fast'
        CHECK (kumbha_alias IN ('teepin/fast', 'teepin/deep', 'teepin/confidential'));

-- Matches ListKumbhaModels' own WHERE/ORDER BY shape exactly (enabled AND
-- kumbha_enabled, filtered by alias, ordered by priority).
CREATE INDEX idx_inference_models_kumbha_alias
    ON inference.models (kumbha_alias, kumbha_priority)
    WHERE enabled AND kumbha_enabled;
