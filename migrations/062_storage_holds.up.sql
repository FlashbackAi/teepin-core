-- When an account runs out of credit its stored data (object storage now,
-- instance disks later) is held for a fixed period, unbilled and
-- inaccessible, then deleted unless the account adds credit first.
--
-- One row per hold. Rows are kept after release/purge as history: the
-- object-storage meter needs the latest released_at so the held period is
-- never billed once access comes back.
CREATE TABLE billing.storage_holds (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    account_id   UUID NOT NULL REFERENCES auth.accounts(id) ON DELETE CASCADE,
    held_since   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delete_after TIMESTAMPTZ NOT NULL,
    released_at  TIMESTAMPTZ,
    purged_at    TIMESTAMPTZ,
    last_error   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT storage_holds_not_both CHECK (released_at IS NULL OR purged_at IS NULL)
);

-- At most one active hold per account.
CREATE UNIQUE INDEX storage_holds_one_active
    ON billing.storage_holds (account_id)
    WHERE released_at IS NULL AND purged_at IS NULL;

CREATE INDEX storage_holds_due
    ON billing.storage_holds (delete_after)
    WHERE released_at IS NULL AND purged_at IS NULL;
