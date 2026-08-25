-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- The customer's own view of what Kumbha built — versioned, not
-- overwrite-in-place, because the console IDE lets the customer edit
-- files and redeploy directly. An editable IDE with a Deploy button that
-- can break a working app needs a way back to the last good state; a
-- single current-snapshot row cannot offer that.
--
-- Append-only: every save (agent OR customer) inserts a new version
-- rather than mutating an old one, so "roll back to version 3" is always
-- possible even after version 7 has shipped. inference_sessions gets a
-- pointer to which version is CURRENT — the one the file browser shows
-- and the one `deploy`/the console's Deploy button acts on — separately
-- from the version count, so rollback is a pointer update, not a delete.
--
-- Deliberately NOT a git remote — see the original (non-versioned)
-- design note this replaces: a repository URL in a shared Teepin GitHub
-- org exposes that org's other repositories to anyone who follows the
-- link, and a download/version-history UI has no such surface.
--
-- Storage shape: JSONB of {path, content} per version, same reasoning as
-- the original design — these are small text-file web apps, individual
-- files serve both the file browser and the ZIP download without a
-- second storage format, and this moves to S3 behind the same API if
-- history ever grows past what is comfortable in a row.

BEGIN;

CREATE TABLE IF NOT EXISTS billing.kumbha_workspace_versions (
    session_id  UUID        NOT NULL REFERENCES billing.inference_sessions(id) ON DELETE CASCADE,
    -- 1, 2, 3... per session — assigned by SaveVersion via
    -- "SELECT COALESCE(MAX(version),0)+1", not a sequence, so it reads as
    -- an ordinary history number ("version 3") rather than a global id.
    version     INTEGER     NOT NULL,
    files       JSONB       NOT NULL DEFAULT '[]'::jsonb,
    -- Paths deliberately not stored (binary, too large), each
    -- {path, reason} — surfaced to the customer, never silent.
    skipped     JSONB       NOT NULL DEFAULT '[]'::jsonb,
    file_count  INTEGER     NOT NULL DEFAULT 0 CHECK (file_count >= 0),
    byte_size   BIGINT      NOT NULL DEFAULT 0 CHECK (byte_size >= 0),
    -- 'agent' (an automatic save after a file_editor call) or 'customer'
    -- (an explicit edit-and-save in the console IDE) — the history list
    -- shows which, since "the agent wrote this" and "I changed this
    -- myself" are different things to scan when picking a rollback target.
    created_by  VARCHAR(20) NOT NULL DEFAULT 'agent' CHECK (created_by IN ('agent', 'customer')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (session_id, version)
);

CREATE INDEX IF NOT EXISTS idx_kumbha_workspace_versions_session
    ON billing.kumbha_workspace_versions(session_id, version DESC);

-- Which version is CURRENT — what the file browser shows and what a
-- deploy/redeploy acts on. NULL until the first version is saved. Kept on
-- inference_sessions (not inferred as "the highest version number") so
-- rollback is a single UPDATE, independent of how many versions exist.
ALTER TABLE billing.inference_sessions
    ADD COLUMN IF NOT EXISTS current_workspace_version INTEGER;

COMMIT;
