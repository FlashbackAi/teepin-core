-- Invoice v2: customer-facing service grouping, tax detail, payment terms,
-- and per-year gapless invoice numbers.

-- The catalog service a line belongs to ("GPU compute", "Inference", ...),
-- recorded at issue time so a stored invoice keeps the grouping it was sent
-- with even if the catalog is later renamed. Empty for pre-v2 invoices.
ALTER TABLE billing.invoice_line_items
    ADD COLUMN IF NOT EXISTS service TEXT NOT NULL DEFAULT '';

-- Country (ISO 3166-1 alpha-2) of the bill-to snapshot, the input tax
-- decisions are made on. tax_details is the itemised tax that produced
-- invoices.tax (name, rate, amount, jurisdiction, registration code); an
-- empty list means no tax was charged. payment_terms is printed verbatim.
ALTER TABLE billing.invoices
    ADD COLUMN IF NOT EXISTS bill_to_country VARCHAR(2),
    ADD COLUMN IF NOT EXISTS tax_details JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS payment_terms TEXT;

-- One counter row per calendar year. Numbers are allocated inside the
-- invoice-creating transaction, so a rollback returns the number: the
-- sequence stays gapless, which many tax authorities require. Replaces the
-- single global billing.invoice_number_seq (left in place: it already
-- numbered existing invoices, and dropping it would gain nothing).
CREATE TABLE IF NOT EXISTS billing.invoice_counters (
    year        INT    PRIMARY KEY,
    last_number BIGINT NOT NULL CHECK (last_number >= 0)
);
