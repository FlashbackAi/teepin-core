-- Teepin Build (Kumbha): what a build did, and what the customer approved.
--
-- 1. kumbha_events: the agent's activity feed, kept on the control plane. It
--    used to exist only in the agent pod's log, so it vanished with the pod
--    and a build that had finished (or died) showed an empty feed. A row is one
--    sanitized event line the agent wrote. (session_id, launch_seq, line_no)
--    is unique: a pod's log is replayed from its first line on every
--    reconnect, so recording the same line twice must be a no-op.
--    launch_seq counts the session's agent launches (a relaunched pod reuses
--    its name and starts a fresh log, so the name cannot tell launches apart).
--    Retention: rows go with the session (ON DELETE CASCADE), i.e. when the
--    customer deletes the build.
--
-- 2. kumbha_plans: every deployment plan the agent presented, with the
--    resources it asked for. Approval is of ONE plan, not of the session: the
--    session records which plan was approved, and a deploy may only use
--    resources that plan covers. A new plan asking for more has to be approved
--    in its own right.
ALTER TABLE billing.inference_sessions
    ADD COLUMN agent_launch_seq INT NOT NULL DEFAULT 0,
    ADD COLUMN approved_plan_id UUID;

CREATE TABLE billing.kumbha_events (
    id         BIGSERIAL   PRIMARY KEY,
    session_id UUID        NOT NULL REFERENCES billing.inference_sessions(id) ON DELETE CASCADE,
    launch_seq INT         NOT NULL,
    line_no    INT         NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    payload    JSONB       NOT NULL,
    UNIQUE (session_id, launch_seq, line_no)
);
CREATE INDEX kumbha_events_session_order ON billing.kumbha_events (session_id, id);

CREATE TABLE billing.kumbha_plans (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id  UUID        NOT NULL REFERENCES billing.inference_sessions(id) ON DELETE CASCADE,
    resources   JSONB       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    approved_at TIMESTAMPTZ
);
CREATE INDEX kumbha_plans_session ON billing.kumbha_plans (session_id, created_at DESC);

ALTER TABLE billing.inference_sessions
    ADD CONSTRAINT inference_sessions_approved_plan_fk
    FOREIGN KEY (approved_plan_id) REFERENCES billing.kumbha_plans(id) ON DELETE SET NULL;
