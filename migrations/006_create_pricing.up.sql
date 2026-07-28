-- Pricing configuration: admin-adjustable platform rates.
-- Single-row table (id = 1) holding the live GPU VRAM rate; the API
-- reads it on every allocation and billing collection so price changes
-- take effect immediately without a restart.
CREATE TABLE billing.pricing (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    vram_price_per_gb_hour DECIMAL(10, 4) NOT NULL CHECK (vram_price_per_gb_hour > 0),
    updated_by VARCHAR(255),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Seed with the launch rate ($0.10 per GB-hour).
INSERT INTO billing.pricing (id, vram_price_per_gb_hour) VALUES (1, 0.10);
