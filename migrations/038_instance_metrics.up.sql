-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Time-series utilization history for compute.instances — the
-- customer-facing analogue of compute.node_metrics (migration 037),
-- added the same week once per-instance (not just per-host) metrics
-- were built. Same shape and reasoning throughout: ON DELETE CASCADE
-- (pure observability, no billing/audit weight — an instance's history
-- has no purpose once the instance itself is gone), REAL columns (not
-- scaled integers, not commercial facts), no zero-filling on the write
-- side (a row here is always a genuine reading, never a placeholder).
--
-- storage_used_gb is a SNAPSHOT of ephemeral storage usage, NOT a
-- throughput rate — deliberately unlike node_metrics' own
-- storage_read_mbps/storage_write_mbps. Per-pod disk I/O RATE has no
-- equivalent in the kubelet's /stats/summary (the data source here);
-- that lives in cAdvisor's Prometheus /metrics/cadvisor endpoint, out of
-- scope for this pass. Reporting a fabricated rate would be actively
-- misleading, so this reports the usage gauge honestly instead — see
-- cluster.InstanceMetric's own doc comment.

BEGIN;

CREATE TABLE compute.instance_metrics (
    id                BIGSERIAL PRIMARY KEY,
    instance_id       VARCHAR(50) NOT NULL REFERENCES compute.instances(id) ON DELETE CASCADE,
    recorded_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    cpu_used_percent  REAL NOT NULL,
    memory_used_gb    REAL NOT NULL,
    network_rx_mbps   REAL NOT NULL DEFAULT 0,
    network_tx_mbps   REAL NOT NULL DEFAULT 0,
    storage_used_gb   REAL NOT NULL DEFAULT 0
);

-- Same query shape as idx_node_metrics_node_recorded: "this instance's
-- readings, most recent first, since some cutoff".
CREATE INDEX idx_instance_metrics_instance_recorded
    ON compute.instance_metrics (instance_id, recorded_at DESC);

COMMIT;
