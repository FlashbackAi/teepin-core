-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Reverts 041_storage_backend_health: the Teepin S3 health probe
-- (pkg/objectstore/probe.go) was removed after live testing against
-- Shelby showed it flagging Shelby's own documented eventual-consistency
-- delay as a failure (the probe called Backend.Get directly, bypassing
-- the consistency-retry logic Service.GetObject already has) — a false
-- alarm, not a real signal, and the decision was to remove monitoring
-- rather than patch the probe to match Service's retry behavior.
--
-- A new migration rather than editing/deleting 041's files: 041 already
-- applied against the real dev database, so its up/down stay as the
-- accurate historical record of what was built and why; this migration
-- is the equally-real record of undoing it.

BEGIN;

DROP TABLE IF EXISTS storage.backend_health;

COMMIT;
