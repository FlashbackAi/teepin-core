-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Time-series health-probe history for a Teepin S3 backend
-- (pkg/objectstore/probe.go). Every reading is a real put/get/delete
-- cycle run against a reserved, non-customer key prefix — this table is
-- how "is Shelby actually healthy right now" stops being a guess and
-- becomes something the storage service tab can show directly, since
-- Shelby (the whole reason this exists) has no SLA and has been
-- confirmed to fail silently under load (see shelby-eval/).
--
-- Same time-series shape as compute.node_metrics (migration 037): only
-- ever queried as "this backend's readings, most recent first" — a
-- composite index in that order covers it as a single index scan.

BEGIN;

CREATE TABLE storage.backend_health (
    id          BIGSERIAL PRIMARY KEY,
    backend     VARCHAR(32) NOT NULL,
    probe       VARCHAR(16) NOT NULL, -- 'put' | 'get' | 'delete'
    ok          BOOLEAN NOT NULL,
    latency_ms  INTEGER NOT NULL,
    error       TEXT,
    checked_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_backend_health_backend_checked
    ON storage.backend_health (backend, checked_at DESC);

COMMIT;
