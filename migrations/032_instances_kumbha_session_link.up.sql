-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Every instance a Kumbha agent provisions must be traceable back to the
-- session that created it — found live 2026-08-30/31: an agent working
-- around a broken `deploy` endpoint fell back to `create_instance`
-- directly, which has always gone straight to POST /v1/compute/instances
-- with zero Kumbha-specific bookkeeping. Two real instances came out of
-- one build (one broken, one working) and NEITHER was ever recorded on
-- billing.inference_sessions — invisible to the session, invisible to the
-- console, discoverable only by the customer noticing an extra bill.
--
-- kumbha_session_id is deliberately separate from
-- inference_sessions.agent_instance_id (the agent's OWN pod) and
-- inference_sessions.app_instance_id (the ONE instance DeployKumbhaSession
-- has designated "the app", singular) — an instance can be neither and
-- still need tracking (a create_instance sidecar, or an app-path instance
-- before/without ever being designated). This column answers "did a
-- Kumbha session create this, and which one" for every instance, so none
-- can go untracked again; SetAppInstanceID and this column are set
-- independently and can point at the same instance without conflict.

BEGIN;

ALTER TABLE compute.instances
    ADD COLUMN IF NOT EXISTS kumbha_session_id UUID REFERENCES billing.inference_sessions(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_instances_kumbha_session ON compute.instances(kumbha_session_id) WHERE kumbha_session_id IS NOT NULL;

COMMIT;
