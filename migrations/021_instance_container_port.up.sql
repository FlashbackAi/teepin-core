-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Stage 3 tunnel. The customer's container port (ports[0].container on the
-- create request) was used once, at pod-build/endpoint-provisioning time,
-- and never persisted -- so nothing could later answer "which port does
-- this running instance listen on". The proxy handler needs exactly that
-- to route a request to the right port on a resolved pod/Service address.
--
-- NULL (not 0) for an instance created with no ports at all -- 0 is not a
-- valid TCP port and would be indistinguishable from "not set".

BEGIN;

ALTER TABLE compute.instances
    ADD COLUMN IF NOT EXISTS container_port INT;

COMMIT;
