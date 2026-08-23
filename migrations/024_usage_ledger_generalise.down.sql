-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverses 024.

BEGIN;

-- Child before parent: inference_session_usage FKs to inference_sessions.
DROP TABLE IF EXISTS billing.inference_session_usage;
DROP TABLE IF EXISTS billing.inference_sessions;

ALTER TABLE billing.pricing
    DROP COLUMN IF EXISTS llm_price_per_million_input,
    DROP COLUMN IF EXISTS llm_price_per_million_output;

DROP INDEX IF EXISTS billing.idx_usage_subject;
DROP INDEX IF EXISTS billing.idx_usage_provider;

ALTER TABLE billing.usage_records
    ALTER COLUMN quantity   TYPE DECIMAL(10,4),
    ALTER COLUMN unit_price TYPE DECIMAL(10,4),
    ALTER COLUMN total_cost TYPE DECIMAL(10,4);

ALTER TABLE billing.usage_records
    ALTER COLUMN instance_id SET NOT NULL;

ALTER TABLE billing.usage_records
    DROP COLUMN IF EXISTS subject_type,
    DROP COLUMN IF EXISTS subject_id,
    DROP COLUMN IF EXISTS cost_basis,
    DROP COLUMN IF EXISTS provider;

COMMIT;
