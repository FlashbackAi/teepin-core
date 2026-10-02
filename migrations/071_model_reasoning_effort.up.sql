-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- How hard a model thinks before each answer, sent with every Teepin Build
-- request as "reasoning_effort". Empty means "send nothing" and leaves the model's
-- own default. GLM-5.3's default is "max": on the builder's route it sometimes
-- spent its whole answer allowance thinking and returned nothing, while an
-- explicit "high" or "low" cut a simple call's reasoning from up to 766 tokens to
-- under 15 (measured 2026-10-02). One field on the one model registry, beside
-- max_output_tokens, not a per-product setting.
ALTER TABLE inference.models
    ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT ''
        CHECK (reasoning_effort IN ('', 'low', 'medium', 'high', 'max'));
