-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Interactive exec ("connect to instance terminal" from the console or
-- the teepin CLI) session history. This is the audit trail for the most
-- sensitive customer-facing action on the platform (arbitrary code
-- execution in a running container), and it doubles as the "history"
-- list the console shows under an instance's Terminal tab.
--
-- Written in two passes: a row is inserted at ticket issue (pkg/api,
-- where identity is known — the WebSocket attach step never re-derives
-- tenancy), then updated at session end (pkg/cluster's ExecHandler,
-- keyed by ticket_id). A row with ended_at NULL means the ticket was
-- issued but the session never attached, or attached and is still open.
--
-- Deliberately does NOT store output content — full recording/playback
-- is a materially bigger storage/privacy decision, flagged as a
-- fast-follow rather than blocking this.

BEGIN;

CREATE TABLE compute.exec_sessions (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    ticket_id    TEXT NOT NULL UNIQUE,

    instance_id  VARCHAR(50) NOT NULL REFERENCES compute.instances(id) ON DELETE CASCADE,
    account_id   UUID NOT NULL REFERENCES auth.accounts(id) ON DELETE CASCADE,
    project_id   UUID NOT NULL REFERENCES auth.projects(id) ON DELETE CASCADE,
    user_id      UUID REFERENCES auth.users(id) ON DELETE SET NULL,

    container    TEXT,        -- empty = the pod's first/only container
    command      TEXT[],      -- empty = the agent's default shell probe

    pod_name     TEXT,        -- filled in from ExecOpen once attached
    exit_code    INTEGER,
    close_reason TEXT,        -- "exit" | "idle" | "node_offline" | ... (see ExecHandler's close codes)

    started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ended_at     TIMESTAMPTZ
);

CREATE INDEX idx_exec_sessions_instance ON compute.exec_sessions(instance_id, started_at DESC);
CREATE INDEX idx_exec_sessions_account  ON compute.exec_sessions(account_id, started_at DESC);

COMMIT;
