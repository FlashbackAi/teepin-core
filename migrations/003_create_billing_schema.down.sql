-- Rollback billing schema
DROP TRIGGER IF EXISTS update_payment_methods_updated_at ON billing.payment_methods;
DROP TRIGGER IF EXISTS update_invoices_updated_at ON billing.invoices;

DROP TABLE IF EXISTS billing.payment_methods;
DROP TABLE IF EXISTS billing.invoices;
DROP TABLE IF EXISTS billing.usage_records;

DROP SCHEMA IF EXISTS billing CASCADE;
