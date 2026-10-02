-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Which model describes the images a customer attaches to a Teepin Build.
--
-- The builder model (GLM) cannot see images. When a customer attaches one, the
-- platform asks a vision model to describe it once and hands the description to
-- the builder. That is a separate role from building, so it is its own toggle on
-- the one model registry (beside kumbha_enabled, "Builder"), not a second config:
-- a model can be a reader without being a builder, which is the point.
ALTER TABLE inference.models
    ADD COLUMN kumbha_image_reader BOOLEAN NOT NULL DEFAULT FALSE;
