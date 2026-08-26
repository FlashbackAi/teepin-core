-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Distinguishes a real, customer-visible version from an agent's
-- in-progress draft. Found live 2026-08-26: the agent calls
-- PUT .../workspace once per file_editor step, and the original
-- always-INSERT design (migration 025) turned that into one new row per
-- file write — a real session's "Version history" filled with dozens of
-- near-identical entries, exactly what MaxWorkspaceVersions' own comment
-- already warned an unbounded build could do.
--
-- New behaviour (see pkg/kumbha/workspace.go's SaveVersion): an agent
-- auto-save now UPDATEs the current row in place as long as it is not yet
-- a checkpoint (is_checkpoint = false) — no new row, no growth, during
-- active editing between deploys. A customer's own explicit IDE save
-- always inserts a new, immediately-checkpointed row (a deliberate
-- action). CheckpointCurrentVersion flips the current draft to
-- is_checkpoint = true once a deploy actually succeeds — the first
-- moment that version becomes visible in the console's "Version history"
-- list and the first moment a later agent write must start a NEW draft
-- rather than mutating it further.
--
-- Defaulted true for any row that predates this migration: an existing
-- session's already-saved versions were all real, visible history under
-- the old design, and should stay exactly as visible as they were.

BEGIN;

ALTER TABLE billing.kumbha_workspace_versions
    ADD COLUMN IF NOT EXISTS is_checkpoint BOOLEAN NOT NULL DEFAULT true;

COMMIT;
