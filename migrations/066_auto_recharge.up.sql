-- Automatic recharge (prepaid billing phase 6): when an account's credit
-- falls below a threshold, charge its saved default card for a fixed amount.
--
-- One row per account. The customer's own rule (threshold, amount, monthly
-- cap) lives here, together with the state needed to retry safely:
--   consecutive_failures  charges that failed in a row; three switch it off.
--   next_attempt_at       earliest time of the next attempt (a claim, so two
--                         replicas or two sweeps never charge together, and
--                         the backoff after a failure).
--   disabled_reason       why it was switched off automatically, shown to the
--                         customer.
--   cap_notified_month    first day of the month the "monthly cap reached"
--                         email was last sent for, so it is sent once a month.
--   enabled_by/at         who agreed to be charged automatically, and when.
CREATE TABLE billing.auto_recharge (
    account_id           UUID PRIMARY KEY REFERENCES auth.accounts(id) ON DELETE CASCADE,
    enabled              BOOLEAN NOT NULL DEFAULT FALSE,
    threshold            NUMERIC(12,2) NOT NULL CHECK (threshold >= 5),
    amount               NUMERIC(12,2) NOT NULL CHECK (amount >= 20 AND amount <= 1000),
    monthly_cap          NUMERIC(12,2) NOT NULL CHECK (monthly_cap >= amount AND monthly_cap <= 10000),
    consecutive_failures SMALLINT NOT NULL DEFAULT 0,
    last_attempt_at      TIMESTAMPTZ,
    next_attempt_at      TIMESTAMPTZ,
    disabled_reason      TEXT,
    cap_notified_month   DATE,
    enabled_by           UUID,
    enabled_at           TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_auto_recharge_enabled ON billing.auto_recharge (account_id) WHERE enabled;

-- A top-up is either bought by the customer in the console ('manual') or
-- charged by the rule above ('auto'). The monthly cap counts only 'auto'.
ALTER TABLE billing.credit_topups
    ADD COLUMN source VARCHAR(10) NOT NULL DEFAULT 'manual'
        CHECK (source IN ('manual', 'auto'));
