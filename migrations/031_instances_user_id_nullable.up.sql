-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- compute.instances.user_id was declared `NOT NULL REFERENCES auth.users(id)
-- ON DELETE SET NULL` (002_create_compute_schema) — self-contradictory even
-- at the time (a user deletion firing ON DELETE SET NULL would itself be
-- rejected by the NOT NULL constraint), but latent until now because every
-- credential that could reach POST /v1/compute/instances carried a real
-- human user_id.
--
-- The Kumbha session-scoped JWT (auth.MintSessionToken) broke that
-- assumption: it authenticates via auth.Middleware.RequireAuth() same as
-- any other credential, but has no attributable user, leaving
-- auth.Claims.UserID at its Go zero value (uuid.Nil). pkg/api/server.go's
-- CreateInstance handler and pkg/compute/store.go's Create() both already
-- pass that zero UUID straight through with no NULL conversion (unlike
-- every other optional field on this INSERT, which goes through
-- nullIfEmpty/sql.NullInt64) — so the literal all-zeros UUID was inserted
-- as user_id, violating instances_user_id_fkey since no user row has that
-- id. Confirmed live 2026-08-30: the Kumbha agent's own create_instance
-- and deploy calls both failed with exactly this error.
--
-- Fix is two-sided: this migration makes the column genuinely nullable
-- (matching what ON DELETE SET NULL always implied it should be), and
-- pkg/compute/store.go's Create() now converts uuid.Nil to SQL NULL before
-- inserting, same as its sibling optional fields.

BEGIN;

ALTER TABLE compute.instances
    ALTER COLUMN user_id DROP NOT NULL;

COMMIT;
