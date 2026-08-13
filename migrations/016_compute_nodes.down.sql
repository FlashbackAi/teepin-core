-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverses 016. This is the archivability path for the home-compute pilot:
-- with HOME_COMPUTE_ENABLED=false and this migration reverted, the platform
-- is exactly as it was before the pilot. Dropping compute.nodes discards
-- enrolled-node records and per-node credentials (agents must re-enroll if
-- the feature is later restored); acceptable because this predates any
-- production reliance on persisted nodes.

BEGIN;

DROP INDEX IF EXISTS compute.idx_instances_node;
ALTER TABLE compute.instances DROP COLUMN IF EXISTS node_id;

DROP TABLE IF EXISTS compute.node_enrollment_tokens;
DROP TABLE IF EXISTS compute.nodes;

COMMIT;
