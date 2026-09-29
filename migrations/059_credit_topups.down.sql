-- Reverts 059. Fails if any credit was purchased or any receipt issued:
-- those are financial records, and the down path refuses to silently drop
-- them rather than succeed by destroying a customer's paid balance.

ALTER TABLE billing.invoices
    DROP CONSTRAINT IF EXISTS invoices_source_check;

ALTER TABLE billing.invoices
    ADD CONSTRAINT invoices_source_check
    CHECK (source IN ('usage', 'manual'));

DROP INDEX IF EXISTS billing.idx_credit_tx_topup;

ALTER TABLE billing.credit_transactions
    DROP COLUMN IF EXISTS topup_id;

ALTER TABLE billing.credit_transactions
    DROP CONSTRAINT IF EXISTS credit_transactions_kind_check;

ALTER TABLE billing.credit_transactions
    ADD CONSTRAINT credit_transactions_kind_check
    CHECK (kind IN ('grant', 'consumption', 'expiry', 'revocation'));

DROP TABLE IF EXISTS billing.credit_topups;
