-- Separate, gapless numbering series per document type.
--
-- Invoices keep "INV-<year>-<n>". Receipts for prepaid credit purchases get
-- their own "RCT-<year>-<n>" series with their own counter. Two prefixes
-- drawn from ONE counter would leave holes in both series (every INV number
-- would skip the RCT numbers issued in between), and tax authorities that
-- require gapless numbering — India's GST, which also wants receipt vouchers
-- and invoices in separate series — read a hole as a removed document.
ALTER TABLE billing.invoice_counters
    ADD COLUMN IF NOT EXISTS series VARCHAR(8) NOT NULL DEFAULT 'INV';

ALTER TABLE billing.invoice_counters
    DROP CONSTRAINT IF EXISTS invoice_counters_pkey;

ALTER TABLE billing.invoice_counters
    ADD PRIMARY KEY (series, year);

-- If receipts were already numbered RCT-… (this migration re-applied after a
-- rollback), resume each year's series after the highest issued number
-- instead of restarting at 1 and colliding with invoice_number's unique
-- constraint.
INSERT INTO billing.invoice_counters (series, year, last_number)
SELECT 'RCT',
       split_part(invoice_number, '-', 2)::INT,
       MAX(split_part(invoice_number, '-', 3)::BIGINT)
FROM billing.invoices
WHERE invoice_number ~ '^RCT-[0-9]{4}-[0-9]+$'
GROUP BY split_part(invoice_number, '-', 2)
ON CONFLICT (series, year) DO NOTHING;
