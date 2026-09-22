DROP TABLE IF EXISTS billing.invoice_counters;
ALTER TABLE billing.invoices
    DROP COLUMN IF EXISTS payment_terms,
    DROP COLUMN IF EXISTS tax_details,
    DROP COLUMN IF EXISTS bill_to_country;
ALTER TABLE billing.invoice_line_items DROP COLUMN IF EXISTS service;
