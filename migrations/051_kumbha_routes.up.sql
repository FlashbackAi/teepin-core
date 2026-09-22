-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Per-route on/off, settable live from Control Center instead of only at
-- process boot via env vars. A route with no row here is enabled (the
-- same "ships on" default every other toggle in this schema uses) — only
-- an explicit row can turn one off.
CREATE TABLE IF NOT EXISTS billing.kumbha_routes (
    route_name TEXT PRIMARY KEY,
    enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    updated_by TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
