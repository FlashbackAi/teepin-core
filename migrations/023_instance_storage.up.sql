-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Persistent volumes: a customer-chosen PersistentVolumeClaim per
-- instance, mounted at /data, billed by GB-month.
--
-- storage_gb DEFAULT 0 means "no volume" — buildPod only mounts a PVC
-- when this is > 0, so every existing instance stays exactly as it was.
-- storage_price_per_gb_month DEFAULT 0 follows the same pattern migration
-- 017 established for cpu_price_per_core_hour/memory_price_per_gb_hour:
-- the metering machinery ships on, but nothing is billed until an
-- operator deliberately sets a rate.

BEGIN;

ALTER TABLE compute.instances
    ADD COLUMN IF NOT EXISTS storage_gb INT NOT NULL DEFAULT 0
        CHECK (storage_gb >= 0);

ALTER TABLE billing.pricing
    ADD COLUMN IF NOT EXISTS storage_price_per_gb_month DECIMAL(10,4) NOT NULL DEFAULT 0
        CHECK (storage_price_per_gb_month >= 0);

COMMIT;
