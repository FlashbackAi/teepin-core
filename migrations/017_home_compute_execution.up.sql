-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Stage 2 of the home-compute pilot: run a workload on a home node and
-- meter it.
--
-- Two additions:
--   1. compute.instances.provider_id — which PROVIDER (agent session) an
--      instance was dispatched to. Migration 016 added node_id (which
--      hardware); provider_id is how the control plane routes a later
--      delete/logs command back to the SAME session that created the pod.
--      Without it, multi-provider dispatch falls back to an arbitrary
--      session (the registry.Any() bug), which is correct only while there
--      is exactly one provider.
--   2. CPU/memory pricing rates. GPU has always been metered on VRAM; CPU
--      instances were billed $0 (the collector skipped them). These rates
--      let the collector charge CPU nodes. They DEFAULT TO 0 on purpose:
--      the metering machinery ships on, but bills nothing until an operator
--      deliberately sets a rate — so nothing running free today is
--      retroactively charged.

BEGIN;

ALTER TABLE compute.instances
    ADD COLUMN IF NOT EXISTS provider_id TEXT;

ALTER TABLE billing.pricing
    ADD COLUMN IF NOT EXISTS cpu_price_per_core_hour  DECIMAL(10,4) NOT NULL DEFAULT 0
        CHECK (cpu_price_per_core_hour >= 0),
    ADD COLUMN IF NOT EXISTS memory_price_per_gb_hour DECIMAL(10,4) NOT NULL DEFAULT 0
        CHECK (memory_price_per_gb_hour >= 0);

COMMIT;
