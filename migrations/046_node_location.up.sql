-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Operator-provided location for a compute node — a manual, self-reported
-- coordinate/label, never derived from IP geolocation. An operator enrolling
-- their own machine knows its true location far better than an IP-based
-- lookup would guess, and this avoids taking on a GeoIP dataset or a
-- third-party geolocation service for what is, for now, purely a "show it
-- on a map in Control Centre" feature with no placement/billing effect.
--
-- All three nullable: sets nothing by default. Set once via Control Centre
-- (PUT /v1/admin/nodes/:id/location), after enrollment — not part of the
-- enrollment flow itself, so it can be corrected any time without a
-- re-enroll.

BEGIN;

ALTER TABLE compute.nodes
    ADD COLUMN IF NOT EXISTS latitude DOUBLE PRECISION CHECK (latitude IS NULL OR (latitude BETWEEN -90 AND 90)),
    ADD COLUMN IF NOT EXISTS longitude DOUBLE PRECISION CHECK (longitude IS NULL OR (longitude BETWEEN -180 AND 180)),
    ADD COLUMN IF NOT EXISTS location_label TEXT;

COMMIT;
