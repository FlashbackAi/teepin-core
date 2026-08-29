-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Tracks the customer-facing app instance a Kumbha session's Deploy has
-- most recently created — distinct from agent_instance_id (the AGENT's
-- own pod, Kumbha's own workload, never customer-visible). This one names
-- a REAL row in compute.instances, created through the exact same
-- POST /v1/compute/instances path any customer-created instance goes
-- through (billed, endpoint-provisioned, manageable from Compute).
--
-- Redeploying (Save an edit, click Deploy again) swaps THIS instance's
-- pod in place for one running the newly built image — same id, same
-- hostname/TLS cert, same compute.instances row (see
-- cluster.Client.UpdateInstance and pkg/api.redeployKumbhaInstance) —
-- and this column is how the handler finds which instance to update.
-- (Originally, before that in-place capability existed, a redeploy
-- created a brand new instance and tore down the one this column named;
-- kept here as historical context for why the column reads "most
-- recently deployed" rather than "the only one ever deployed.")

BEGIN;

ALTER TABLE billing.inference_sessions
    ADD COLUMN IF NOT EXISTS app_instance_id VARCHAR(50);

COMMIT;
