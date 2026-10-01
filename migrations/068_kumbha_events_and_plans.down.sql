ALTER TABLE billing.inference_sessions DROP CONSTRAINT IF EXISTS inference_sessions_approved_plan_fk;
DROP TABLE IF EXISTS billing.kumbha_plans;
DROP TABLE IF EXISTS billing.kumbha_events;
ALTER TABLE billing.inference_sessions
    DROP COLUMN IF EXISTS approved_plan_id,
    DROP COLUMN IF EXISTS agent_launch_seq;
