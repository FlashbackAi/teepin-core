-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- A queue of customer follow-up messages for a Kumbha build session — the
-- other half of "chat + resume": until this existed, the ONLY thing a
-- customer could ever tell the agent was the session's initial prompt.
-- The agent's own generalized wait loop (deploy/kumbha-agent/run.py's
-- wait_for_next_instruction, superseding the narrower
-- wait_for_deploy_approval) polls for undelivered rows and feeds them
-- into the SAME conversation object — full history intact, a real
-- continuation, not a new conversation that has forgotten everything.
--
-- Polled and marked delivered together (see pkg/kumbha's PollMessages),
-- not a separate ack step: at-most-once delivery is the right default
-- here — a message re-delivered after a network hiccup would have the
-- agent act on the same instruction twice, which is worse than the rare
-- case of a message lost to a crash between poll and processing (the
-- customer can simply say it again, and sees from the activity feed
-- whether it landed).

BEGIN;

CREATE TABLE IF NOT EXISTS billing.kumbha_messages (
    id           BIGSERIAL   PRIMARY KEY,
    session_id   UUID        NOT NULL REFERENCES billing.inference_sessions(id) ON DELETE CASCADE,
    content      TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- NULL until PollMessages delivers it. Indexed below so polling (the
    -- agent's own frequent, per-session query) never scans delivered
    -- history.
    delivered_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_kumbha_messages_undelivered
    ON billing.kumbha_messages(session_id, id)
    WHERE delivered_at IS NULL;

COMMIT;
