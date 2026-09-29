-- Prepaid credit top-ups (prepaid billing phase 2).
--
-- A customer buys credit up front; usage then draws it down to zero. Each
-- top-up is one payment attempt, tracked here from creation to settlement,
-- and — only once the payment provider confirms it — turned into a
-- 'purchase' row in the append-only credit ledger plus a paid receipt.

CREATE TABLE IF NOT EXISTS billing.credit_topups (
    id                       UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    account_id               UUID NOT NULL REFERENCES auth.accounts(id) ON DELETE RESTRICT,
    -- Exact amount of credit bought, in currency units (two decimals).
    amount                   DECIMAL(12,2) NOT NULL CHECK (amount > 0),
    currency                 VARCHAR(3) NOT NULL DEFAULT 'USD',
    provider                 VARCHAR(20) NOT NULL CHECK (provider IN ('stripe')),
    -- pending:    created, the customer has not completed payment
    -- processing: submitted, awaiting settlement (e.g. an ACH debit)
    -- succeeded:  settled; credit and receipt exist
    -- failed:     the payment did not go through; no credit was added
    status                   VARCHAR(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'processing', 'succeeded', 'failed')),
    stripe_payment_intent_id VARCHAR(255) UNIQUE,
    -- Human-readable payment method snapshot ("Visa ending 4242"), for
    -- the receipt and the top-up history.
    payment_method_summary   TEXT,
    failure_reason           TEXT,
    receipt_invoice_id       UUID REFERENCES billing.invoices(id) ON DELETE RESTRICT,
    succeeded_at             TIMESTAMP WITH TIME ZONE,
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_credit_topups_account
    ON billing.credit_topups(account_id, created_at DESC);

-- The ledger gains a 'purchase' kind: credit the customer paid for, as
-- opposed to an operator 'grant'. Purchases never expire. The CHECK name
-- is looked up rather than assumed (it was declared inline in migration
-- 014), the same way migration 058 handles inline constraints.
DO $$
DECLARE
    c_name TEXT;
BEGIN
    SELECT conname INTO c_name
    FROM pg_constraint
    WHERE conrelid = 'billing.credit_transactions'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%consumption%';
    IF c_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE billing.credit_transactions DROP CONSTRAINT %I', c_name);
    END IF;
END $$;

ALTER TABLE billing.credit_transactions
    ADD CONSTRAINT credit_transactions_kind_check
    CHECK (kind IN ('grant', 'purchase', 'consumption', 'expiry', 'revocation'));

-- Ties a purchase row to the top-up that paid for it. The unique index
-- makes crediting idempotent: a replayed payment webhook cannot add the
-- same top-up's credit twice.
ALTER TABLE billing.credit_transactions
    ADD COLUMN IF NOT EXISTS topup_id UUID REFERENCES billing.credit_topups(id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_tx_topup
    ON billing.credit_transactions(topup_id)
    WHERE topup_id IS NOT NULL;

-- A receipt for a credit purchase is an invoice issued already paid, with
-- its own source so it is never mistaken for a usage or negotiated bill.
ALTER TABLE billing.invoices
    DROP CONSTRAINT IF EXISTS invoices_source_check;

ALTER TABLE billing.invoices
    ADD CONSTRAINT invoices_source_check
    CHECK (source IN ('usage', 'manual', 'credit_purchase'));
