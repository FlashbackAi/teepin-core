-- Restores the columns 073 dropped, empty (the detected splits, placements and
-- prices are not recoverable; a node reports its split again on reconnect only
-- with an agent that still has the feature).

ALTER TABLE compute.nodes
    ADD COLUMN IF NOT EXISTS p_cores INT CHECK (p_cores IS NULL OR p_cores >= 0),
    ADD COLUMN IF NOT EXISTS e_cores INT CHECK (e_cores IS NULL OR e_cores >= 0);

ALTER TABLE compute.instances
    ADD COLUMN IF NOT EXISTS p_cores_used INT CHECK (p_cores_used IS NULL OR p_cores_used >= 0),
    ADD COLUMN IF NOT EXISTS e_cores_used INT CHECK (e_cores_used IS NULL OR e_cores_used >= 0);

ALTER TABLE billing.pricing
    ADD COLUMN IF NOT EXISTS p_core_price_per_hour DECIMAL(10,4) NOT NULL DEFAULT 0
        CHECK (p_core_price_per_hour >= 0),
    ADD COLUMN IF NOT EXISTS e_core_price_per_hour DECIMAL(10,4) NOT NULL DEFAULT 0
        CHECK (e_core_price_per_hour >= 0);
