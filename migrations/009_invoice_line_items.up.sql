-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Invoice line items and manual invoicing.
--
-- Until now an invoice was a single total with no explanation of what it
-- was for. That is unacceptable on a document a customer files for tax:
-- "$588.00" tells them nothing, and support cannot reconstruct it later
-- either.
--
-- Manual invoicing exists because early customers are negotiated deals,
-- not metered ones. A flat monthly rate is agreed by email, and the
-- platform must be able to bill that while STILL metering usage in the
-- background, so the switch to usage-based billing later is a change of
-- policy rather than a change of system.

CREATE TABLE IF NOT EXISTS billing.invoice_line_items (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    invoice_id UUID NOT NULL REFERENCES billing.invoices(id) ON DELETE CASCADE,

    description TEXT NOT NULL,

    -- Quantity and unit price are stored separately from the line total
    -- so an invoice can show "740 GPU-hours x $1.00" rather than a bare
    -- figure the customer has to take on trust.
    --
    -- NUMERIC, never floating point: 0.1 + 0.2 != 0.3 in binary floating
    -- point, and a cent of drift on a financial document is a support
    -- ticket at best.
    quantity NUMERIC(14, 4) NOT NULL DEFAULT 1,
    unit VARCHAR(32) NOT NULL DEFAULT 'item',
    unit_price NUMERIC(14, 4) NOT NULL,

    -- Stored rather than computed on read: the arithmetic must stay
    -- exactly as it was when the invoice was issued, even if rounding
    -- rules change later.
    amount NUMERIC(14, 2) NOT NULL,

    -- Ordering is meaningful on an invoice; without it lines come back
    -- in whatever order the planner chose.
    position INT NOT NULL DEFAULT 0,

    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_invoice_line_items_invoice
    ON billing.invoice_line_items(invoice_id, position);

-- Distinguishes an invoice a human wrote from one the collector
-- generated.
--
-- Load-bearing: the usage collector must never overwrite or supersede a
-- manually issued invoice, and a customer disputing a charge needs to
-- know whether a person set the number or a meter did.
ALTER TABLE billing.invoices
    ADD COLUMN IF NOT EXISTS source VARCHAR(16) NOT NULL DEFAULT 'usage';

ALTER TABLE billing.invoices
    DROP CONSTRAINT IF EXISTS invoices_source_check;

ALTER TABLE billing.invoices
    ADD CONSTRAINT invoices_source_check
    CHECK (source IN ('usage', 'manual'));

-- Who issued a manual invoice, and any note explaining why. Auditability
-- matters more here than anywhere else in the schema: this is the one
-- place a human can set a number the platform did not calculate.
ALTER TABLE billing.invoices
    ADD COLUMN IF NOT EXISTS issued_by TEXT;

ALTER TABLE billing.invoices
    ADD COLUMN IF NOT EXISTS notes TEXT;

-- Customer-visible due date. Absent on usage invoices, which are billed
-- in arrears against a payment method.
ALTER TABLE billing.invoices
    ADD COLUMN IF NOT EXISTS due_date DATE;

-- Currency, so the figure on the document is never ambiguous. Defaults
-- to USD, which is what every existing invoice was implicitly in.
ALTER TABLE billing.invoices
    ADD COLUMN IF NOT EXISTS currency VARCHAR(3) NOT NULL DEFAULT 'USD';

-- The existing status column is free text. Constrain it now, before
-- there is enough data to make this painful, so an invoice cannot end up
-- in a state nothing handles.
UPDATE billing.invoices SET status = 'draft'
    WHERE status NOT IN ('draft', 'open', 'paid', 'void', 'uncollectible');

ALTER TABLE billing.invoices
    DROP CONSTRAINT IF EXISTS invoices_status_check;

ALTER TABLE billing.invoices
    ADD CONSTRAINT invoices_status_check
    CHECK (status IN ('draft', 'open', 'paid', 'void', 'uncollectible'));
