-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

BEGIN;

CREATE TABLE storage.backend_health (
    id          BIGSERIAL PRIMARY KEY,
    backend     VARCHAR(32) NOT NULL,
    probe       VARCHAR(16) NOT NULL,
    ok          BOOLEAN NOT NULL,
    latency_ms  INTEGER NOT NULL,
    error       TEXT,
    checked_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_backend_health_backend_checked
    ON storage.backend_health (backend, checked_at DESC);

COMMIT;
