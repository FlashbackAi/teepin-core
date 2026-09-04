-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Time-series utilization history for compute.nodes — the foundation for
-- real node telemetry (stats/graphs, the status page, a marketing-site
-- globe visualization — see ROADMAP.md's 2026-09-03 entry). compute.nodes
-- itself only ever holds the LATEST reading (UpsertSeen overwrites it on
-- every report, same as every other liveness field there); this table is
-- what lets anything plot a trend over time instead of just a current
-- value.
--
-- ON DELETE CASCADE, deliberately unlike compute.instances.node_id's own
-- ON DELETE SET NULL: that one preserves billing/audit history across a
-- node's removal on purpose. This table is pure observability data with
-- no financial or audit weight — once the node it describes is gone, its
-- historical utilization readings have no ongoing purpose, so letting
-- them cascade away is correct, not a gap.
--
-- No FK to anything billing-relevant, and REAL (not a scaled integer)
-- for both readings: these are one machine's own CPU%/memory-GB
-- snapshots, not commercial facts anyone is billed against.
--
-- gpu_vram_used_gb: for a GPU/datacenter node row (one row per reported
-- GPU device — see reportInventorySeen's own per-node loop), the CURRENT
-- VRAM in use, alongside compute.nodes.memory_gb which already holds
-- that same GPU's VRAM CAPACITY. A CPU-only home node's rows simply
-- never populate this (stays at the column default, 0) rather than
-- using NULL to mean "not a GPU node" — compute.nodes.gpu_count already
-- answers that question for anything that needs to tell the two apart,
-- so this table does not need a second way to say it.
--
-- network_rx_mbps/network_tx_mbps/storage_read_mbps/storage_write_mbps:
-- host-wide throughput RATES in MB/s at the moment of this reading, same
-- session-level shape as cpu_used_percent/memory_used_gb above (added to
-- this same not-yet-deployed migration rather than a new one, following
-- the same precedent as gpu_vram_used_gb) — not cumulative byte counters,
-- see GPUInventory's proto comment for why a rate is what is meaningful
-- here.

BEGIN;

CREATE TABLE compute.node_metrics (
    id                  BIGSERIAL PRIMARY KEY,
    node_id             UUID NOT NULL REFERENCES compute.nodes(id) ON DELETE CASCADE,
    recorded_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    cpu_used_percent    REAL NOT NULL,
    memory_used_gb      REAL NOT NULL,
    gpu_vram_used_gb    REAL NOT NULL DEFAULT 0,
    network_rx_mbps     REAL NOT NULL DEFAULT 0,
    network_tx_mbps     REAL NOT NULL DEFAULT 0,
    storage_read_mbps   REAL NOT NULL DEFAULT 0,
    storage_write_mbps  REAL NOT NULL DEFAULT 0
);

-- The only query shape this table serves: "this node's readings, most
-- recent first, since some cutoff" — a composite index in that order
-- covers it as a single index scan rather than a filter-then-sort.
CREATE INDEX idx_node_metrics_node_recorded
    ON compute.node_metrics (node_id, recorded_at DESC);

COMMIT;
