-- Credit lots (prepaid billing).
--
-- Each grant and each purchase is a "lot" with its own remaining amount.
-- Spending draws from lots in a fixed order and records which lot each
-- draw came from (lot_id), so that when an expiring grant lapses only its
-- UNSPENT part is forfeited. Before this, an expired grant was dropped from
-- the balance whole while the spending drawn from it stayed on the ledger,
-- so a partly-spent grant that expired drove the balance negative.
--
-- Lot remaining = the lot's amount + every ledger row that names it as
-- lot_id (consumption and expiry rows are negative). Draw order (in
-- application code): grants that expire soonest first, then lots that never
-- expire, oldest first.

ALTER TABLE billing.credit_transactions
    ADD COLUMN IF NOT EXISTS lot_id UUID REFERENCES billing.credit_transactions(id) ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS idx_credit_tx_lot
    ON billing.credit_transactions(lot_id)
    WHERE lot_id IS NOT NULL;

-- One usage record may now be paid from several lots, so idempotency is per
-- (usage record, lot). A NULL lot (unattributable legacy remainder) is
-- coalesced so it still collides with itself.
DROP INDEX IF EXISTS billing.idx_credit_tx_usage;

CREATE UNIQUE INDEX idx_credit_tx_usage
    ON billing.credit_transactions(usage_record_id, COALESCE(lot_id, '00000000-0000-0000-0000-000000000000'::uuid))
    WHERE usage_record_id IS NOT NULL;

-- A lot is forfeited at most once.
CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_tx_expiry
    ON billing.credit_transactions(lot_id)
    WHERE kind = 'expiry';

-- The single source of truth for an account's spendable balance: every
-- ledger row, minus whatever is left in lots that have expired but not yet
-- had their expiry row written by the sweeper. The two are equal once the
-- sweeper has run, so a reader is exact at any moment.
CREATE OR REPLACE FUNCTION billing.credit_balance(p_account UUID)
RETURNS NUMERIC
LANGUAGE sql
STABLE
AS $$
    SELECT COALESCE((SELECT SUM(t.amount)
                       FROM billing.credit_transactions t
                      WHERE t.account_id = p_account), 0)
         - COALESCE((SELECT SUM(GREATEST(x.remaining, 0))
                       FROM (SELECT l.amount + COALESCE((SELECT SUM(c.amount)
                                                           FROM billing.credit_transactions c
                                                          WHERE c.lot_id = l.id), 0) AS remaining
                               FROM billing.credit_transactions l
                              WHERE l.account_id = p_account
                                AND l.kind IN ('grant', 'purchase')
                                AND l.expires_at IS NOT NULL
                                AND l.expires_at <= NOW()) x), 0)
$$;

-- Attribute existing consumption to grants (the only lots that existed),
-- soonest-expiring first, by walking each account's consumption rows in
-- time order. A row spanning two grants is split into two. Whatever cannot
-- be attributed to any grant stays lot-less and is reported.
DO $$
DECLARE
    acct   RECORD;
    lot    RECORD;
    crow   RECORD;
    lots   UUID[];
    rems   NUMERIC[];
    i      INT;
    need   NUMERIC;
    take   NUMERIC;
    first_piece BOOLEAN;
BEGIN
    FOR acct IN
        SELECT DISTINCT account_id
        FROM billing.credit_transactions
        WHERE kind = 'consumption' AND lot_id IS NULL
    LOOP
        lots := ARRAY[]::UUID[];
        rems := ARRAY[]::NUMERIC[];
        FOR lot IN
            SELECT id, amount
            FROM billing.credit_transactions
            WHERE account_id = acct.account_id AND kind = 'grant'
            ORDER BY (expires_at IS NULL), expires_at, created_at, id
        LOOP
            lots := lots || lot.id;
            rems := rems || lot.amount::NUMERIC;
        END LOOP;

        i := 1;
        FOR crow IN
            SELECT id, amount, usage_record_id, created_at
            FROM billing.credit_transactions
            WHERE account_id = acct.account_id AND kind = 'consumption' AND lot_id IS NULL
            ORDER BY created_at, id
        LOOP
            need := -crow.amount;
            first_piece := TRUE;
            WHILE need > 0 AND i <= COALESCE(array_length(lots, 1), 0) LOOP
                take := LEAST(need, rems[i]);
                IF take > 0 THEN
                    IF first_piece THEN
                        UPDATE billing.credit_transactions
                        SET amount = -take, lot_id = lots[i]
                        WHERE id = crow.id;
                        first_piece := FALSE;
                    ELSE
                        INSERT INTO billing.credit_transactions
                            (account_id, amount, kind, reason, usage_record_id, lot_id, created_at)
                        VALUES
                            (acct.account_id, -take, 'consumption', 'usage', crow.usage_record_id, lots[i], crow.created_at);
                    END IF;
                    rems[i] := rems[i] - take;
                    need := need - take;
                END IF;
                IF rems[i] <= 0 THEN
                    i := i + 1;
                END IF;
            END LOOP;

            IF need > 0 THEN
                RAISE NOTICE 'credit lots backfill: account % has % of consumption no grant can cover', acct.account_id, need;
                IF NOT first_piece THEN
                    INSERT INTO billing.credit_transactions
                        (account_id, amount, kind, reason, usage_record_id, lot_id, created_at)
                    VALUES
                        (acct.account_id, -need, 'consumption', 'usage', crow.usage_record_id, NULL, crow.created_at);
                END IF;
            END IF;
        END LOOP;
    END LOOP;
END $$;

-- Forfeit the unspent part of every grant that has already expired, dated
-- to when it lapsed, so the ledger shows it and the balance is a plain sum.
INSERT INTO billing.credit_transactions (account_id, amount, kind, reason, lot_id, created_at)
SELECT x.account_id, -x.remaining, 'expiry', 'Grant expired', x.id, x.expires_at
FROM (
    SELECT l.id, l.account_id, l.expires_at,
           l.amount + COALESCE((SELECT SUM(c.amount)
                                  FROM billing.credit_transactions c
                                 WHERE c.lot_id = l.id), 0) AS remaining
    FROM billing.credit_transactions l
    WHERE l.kind = 'grant' AND l.expires_at IS NOT NULL AND l.expires_at <= NOW()
) x
WHERE x.remaining > 0
ON CONFLICT (lot_id) WHERE kind = 'expiry' DO NOTHING;
