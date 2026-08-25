-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Tracks the customer-facing app instance a Kumbha session's Deploy has
-- most recently created — distinct from agent_instance_id (the AGENT's
-- own pod, Kumbha's own workload, never customer-visible). This one names
-- a REAL row in compute.instances, created through the exact same
-- POST /v1/compute/instances path any customer-created instance goes
-- through (billed, endpoint-provisioned, manageable from Compute).
--
-- Redeploying (Save an edit, click Deploy again) creates a NEW instance
-- from the newly built image and tears down the one this column named —
-- there is no in-place "swap the image on a running instance" capability
-- in the compute layer yet, so each deploy is a fresh instance with a
-- fresh id/endpoint, and this column is how the handler finds the
-- previous one to clean up rather than leaking one per click.

BEGIN;

ALTER TABLE billing.inference_sessions
    ADD COLUMN IF NOT EXISTS app_instance_id VARCHAR(50);

COMMIT;
