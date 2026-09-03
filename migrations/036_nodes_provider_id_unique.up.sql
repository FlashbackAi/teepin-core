-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Fixes a real duplicate-row bug found live 2026-09-03: renaming a node only
-- ever updated compute.nodes.node_name (RenameNode, pkg/nodes/service.go),
-- but the agent's own periodic heartbeat (UpsertSeen) re-identifies itself by
-- its ORIGINAL name (persisted once, client-side, at enroll time — it never
-- learns about a console-side rename). Since UpsertSeen upserts ON CONFLICT
-- (node_name), the very next heartbeat after a rename found no row under the
-- agent's own (stale) name and INSERTED A FRESH ONE instead of updating the
-- renamed row — a genuine duplicate, and one that is born with NULL specs,
-- since the ongoing heartbeat message never carries real cpu_cores/memory_gb
-- at all (only the one-time enroll HTTP call does; see UpsertSeen's own
-- caller, cmd/api-server/adapters.go's nodeReporterAdapter).
--
-- provider_id is the actual stable identity: set once at enrollment
-- (defaulting to node_name if not explicitly overridden — see enroll.go's
-- own `if provider == "" { provider = name }`) and never touched by a
-- rename. This migration makes it unique so UpsertSeen and EnrollNode can
-- both key their ON CONFLICT off it instead of the mutable node_name — see
-- those functions' own updated doc comments in pkg/nodes/service.go.
--
-- Will FAIL to apply if duplicate provider_id rows already exist (exactly
-- what this bug itself produces) — clean those up first:
--   SELECT provider_id, COUNT(*) FROM compute.nodes GROUP BY provider_id HAVING COUNT(*) > 1;
-- then decide, per duplicate, which row to keep (the one with real specs
-- and/or the correct node_name) and DELETE the other — or reassign the
-- stale row's node_id references first if it has any instance history
-- worth keeping (ON DELETE SET NULL on compute.instances.node_id already
-- makes a plain DELETE safe for billing/history either way).

BEGIN;

ALTER TABLE compute.nodes
    ADD CONSTRAINT nodes_provider_id_key UNIQUE (provider_id);

COMMIT;
