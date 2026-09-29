-- Reverts 061. Only the invoice ("INV") counters survive: the primary key
-- goes back to year alone, so a second series cannot coexist. Receipt
-- counters are re-derived from the RCT- numbers already issued if 061 is
-- applied again, so no number is reused.
DELETE FROM billing.invoice_counters WHERE series <> 'INV';

ALTER TABLE billing.invoice_counters
    DROP CONSTRAINT IF EXISTS invoice_counters_pkey;

ALTER TABLE billing.invoice_counters
    ADD PRIMARY KEY (year);

ALTER TABLE billing.invoice_counters
    DROP COLUMN IF EXISTS series;
