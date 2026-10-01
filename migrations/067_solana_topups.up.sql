-- USDC top-ups on Solana (prepaid billing phase 8).
--
-- A customer pays by sending USDC to Teepin's wallet; a Solana Pay "reference"
-- (a random 32-byte key, unique per top-up) rides along in the payment so the
-- one transaction can be found on chain afterwards. Credit is added only once
-- that transaction is FINALIZED and verified (see pkg/solana, pkg/billing).
--
--   solana_reference   the per-top-up reference, base58. Unique.
--   solana_signature   the transaction that paid it, base58. UNIQUE across all
--                      top-ups: a Solana Pay link may list several references,
--                      so without this one payment could be presented as
--                      payment for several top-ups and credited several times.
--   payer_address      the paying wallet, for support.
--   received_micro    what actually arrived, in USDC base units (1e-6).
--   last_checked_at    when the watcher last looked for this payment.
--
-- The new 'review' status marks a payment that arrived but cannot be credited
-- automatically (an amount outside the allowed range). It needs a person.

-- The provider and status checks were declared inline (migration 059); look
-- their names up rather than assume them, as migration 059 itself does.
DO $$
DECLARE
    c_name TEXT;
BEGIN
    FOR c_name IN
        SELECT conname FROM pg_constraint
        WHERE conrelid = 'billing.credit_topups'::regclass
          AND contype = 'c'
          AND (pg_get_constraintdef(oid) LIKE '%provider%' OR pg_get_constraintdef(oid) LIKE '%status%')
    LOOP
        EXECUTE format('ALTER TABLE billing.credit_topups DROP CONSTRAINT %I', c_name);
    END LOOP;
END $$;

ALTER TABLE billing.credit_topups
    ADD CONSTRAINT credit_topups_provider_check
        CHECK (provider IN ('stripe', 'solana')),
    ADD CONSTRAINT credit_topups_status_check
        CHECK (status IN ('pending', 'processing', 'succeeded', 'failed', 'review')),
    ADD COLUMN solana_reference TEXT,
    ADD COLUMN solana_signature TEXT,
    ADD COLUMN payer_address    TEXT,
    ADD COLUMN received_micro   BIGINT,
    ADD COLUMN last_checked_at  TIMESTAMPTZ;

-- A Solana top-up must have its reference; a Stripe one must not need it.
ALTER TABLE billing.credit_topups
    ADD CONSTRAINT credit_topups_solana_reference_check
        CHECK (provider <> 'solana' OR solana_reference IS NOT NULL);

CREATE UNIQUE INDEX credit_topups_solana_reference_key
    ON billing.credit_topups (solana_reference) WHERE solana_reference IS NOT NULL;

CREATE UNIQUE INDEX credit_topups_solana_signature_key
    ON billing.credit_topups (solana_signature) WHERE solana_signature IS NOT NULL;

-- What the watcher scans: unpaid Solana top-ups.
CREATE INDEX credit_topups_solana_pending
    ON billing.credit_topups (created_at)
    WHERE provider = 'solana' AND status = 'pending';
