-- Where each account stands in the low-credit warning ladder, so every warning
-- is sent once and re-armed only after credit is added.
--
--   level         0 none, 1 low (about 3 days or $20 left), 2 about 24 hours,
--                 3 about 6 hours, 4 out of credit.
--   last_balance  the balance at the last evaluation. Spending only ever lowers
--                 the balance, so a rise means credit was added, which re-arms
--                 the ladder.
CREATE TABLE billing.credit_alert_state (
    account_id   UUID PRIMARY KEY REFERENCES auth.accounts(id) ON DELETE CASCADE,
    level        SMALLINT NOT NULL DEFAULT 0 CHECK (level BETWEEN 0 AND 4),
    last_balance NUMERIC(18,6) NOT NULL DEFAULT 0,
    notified_at  TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
