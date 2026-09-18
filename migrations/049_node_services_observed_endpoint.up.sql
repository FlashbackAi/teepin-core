-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- The reachable address a mounted node_services row actually resolved to,
-- reported by the reconciler after CreateInstance succeeds — never typed
-- by an operator. Deliberately separate from `config` (desired, operator-
-- authored input): config says "mount this model, downloaded from this
-- source"; observed_endpoint says "and here's where it actually ended up
-- being reachable," the same desired-vs-observed split this table's other
-- columns already use. Teepin Inference's router (pkg/inferencegateway)
-- reads this instead of any operator-typed base_url.

BEGIN;

ALTER TABLE compute.node_services
    ADD COLUMN IF NOT EXISTS observed_endpoint TEXT;

COMMIT;
