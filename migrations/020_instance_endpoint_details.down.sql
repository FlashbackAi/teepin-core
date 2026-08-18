-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverses 020. compute.instances.endpoint (migration 002) is untouched;
-- only the four new fields are dropped.

BEGIN;

ALTER TABLE compute.instances
    DROP COLUMN IF EXISTS dns_name,
    DROP COLUMN IF EXISTS public_ip,
    DROP COLUMN IF EXISTS tls_enabled,
    DROP COLUMN IF EXISTS tls_ready;

COMMIT;
