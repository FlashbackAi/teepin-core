-- Reverts 060: removes expiry rows, merges consumption rows that the
-- backfill or lot-based spending split across lots back into one row per
-- usage record, then restores the original one-row-per-usage-record index.

DELETE FROM billing.credit_transactions
WHERE kind = 'expiry' AND lot_id IS NOT NULL;

WITH split AS (
    SELECT usage_record_id,
           (array_agg(id ORDER BY created_at, id))[1] AS keep_id,
           SUM(amount) AS total
    FROM billing.credit_transactions
    WHERE kind = 'consumption' AND usage_record_id IS NOT NULL
    GROUP BY usage_record_id
    HAVING COUNT(*) > 1
),
merged AS (
    UPDATE billing.credit_transactions t
    SET amount = s.total
    FROM split s
    WHERE t.id = s.keep_id
    RETURNING t.id
)
DELETE FROM billing.credit_transactions t
USING split s
WHERE t.usage_record_id = s.usage_record_id
  AND t.kind = 'consumption'
  AND t.id != s.keep_id;

DROP FUNCTION IF EXISTS billing.credit_balance(UUID);

DROP INDEX IF EXISTS billing.idx_credit_tx_expiry;
DROP INDEX IF EXISTS billing.idx_credit_tx_usage;
DROP INDEX IF EXISTS billing.idx_credit_tx_lot;

CREATE UNIQUE INDEX idx_credit_tx_usage
    ON billing.credit_transactions(usage_record_id)
    WHERE usage_record_id IS NOT NULL;

ALTER TABLE billing.credit_transactions
    DROP COLUMN IF EXISTS lot_id;
