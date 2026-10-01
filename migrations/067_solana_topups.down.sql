-- Rolling this back would orphan real payments (their receipts and ledger
-- rows), so it refuses while any Solana top-up exists rather than deleting
-- payment records.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM billing.credit_topups WHERE provider = 'solana') THEN
        RAISE EXCEPTION 'cannot roll back migration 067: Solana top-ups exist and have receipts and ledger entries';
    END IF;
END $$;

DROP INDEX IF EXISTS billing.credit_topups_solana_pending;
DROP INDEX IF EXISTS billing.credit_topups_solana_signature_key;
DROP INDEX IF EXISTS billing.credit_topups_solana_reference_key;

ALTER TABLE billing.credit_topups
    DROP CONSTRAINT IF EXISTS credit_topups_solana_reference_check,
    DROP COLUMN IF EXISTS last_checked_at,
    DROP COLUMN IF EXISTS received_micro,
    DROP COLUMN IF EXISTS payer_address,
    DROP COLUMN IF EXISTS solana_signature,
    DROP COLUMN IF EXISTS solana_reference,
    DROP CONSTRAINT IF EXISTS credit_topups_status_check,
    DROP CONSTRAINT IF EXISTS credit_topups_provider_check;

ALTER TABLE billing.credit_topups
    ADD CONSTRAINT credit_topups_provider_check CHECK (provider IN ('stripe')),
    ADD CONSTRAINT credit_topups_status_check
        CHECK (status IN ('pending', 'processing', 'succeeded', 'failed'));
