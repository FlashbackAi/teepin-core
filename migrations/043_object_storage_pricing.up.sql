-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Teepin S3 (pkg/objectstore) billing rates. Same "the metering machinery
-- ships on, but nothing is billed until an operator deliberately sets a
-- rate" pattern as 017/023's own CPU/memory/storage columns — DEFAULT 0
-- means every bucket that exists before an operator sets a real rate
-- bills nothing, not a surprise retroactive charge once one is set.
--
-- Two dimensions, not three: an earlier design also proposed a
-- per-10k-requests rate, but nothing in pkg/objectstore counts requests
-- today — shipping a pricing column with no metering behind it would be
-- exactly the kind of half-finished feature worth avoiding. Add it later
-- if request-count metering is ever actually built.

BEGIN;

ALTER TABLE billing.pricing
    ADD COLUMN IF NOT EXISTS object_storage_price_per_gb_month DECIMAL(10,4) NOT NULL DEFAULT 0
        CHECK (object_storage_price_per_gb_month >= 0),
    ADD COLUMN IF NOT EXISTS object_storage_price_per_gb_egress DECIMAL(10,4) NOT NULL DEFAULT 0
        CHECK (object_storage_price_per_gb_egress >= 0);

COMMIT;
