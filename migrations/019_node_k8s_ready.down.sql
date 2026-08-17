-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverses 019. PlaceCPU falls back to gating on status='online' alone,
-- exactly its pre-019 behaviour.

BEGIN;

ALTER TABLE compute.nodes
    DROP COLUMN IF EXISTS k8s_ready;

COMMIT;
