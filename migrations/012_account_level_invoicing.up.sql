-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Moves invoicing from project-scoped to account-scoped, with an
-- optional per-project breakdown on each line item.
--
-- Why: an account customer thinks in terms of "what do I owe this
-- month", the same way the AWS bill lists service-level charges under
-- one account invoice, not one invoice per service. Forcing an invoice
-- to belong to exactly one project meant a customer with three active
-- projects could never see one combined bill, and an operator issuing a
-- manual invoice had to arbitrarily pick a project to hang it on even
-- for a flat account-wide deal.
--
-- The breakdown does not disappear — it moves DOWN to the line item.
-- invoice_line_items.project_id is nullable: most lines carry a project
-- (so the per-project rollup this was built for still works), but a
-- platform fee, a one-time setup charge, or a credit that is not tied to
-- any single project's usage now has somewhere to live instead of being
-- forced onto an arbitrary project.

-- project_id on the invoice itself becomes optional. account_id (added
-- in migration 007) is now the primary owner; NOT NULL is enforced by
-- the application layer (CreateManualInvoice always resolves an
-- account), not the database, because usage invoices generated before
-- this migration may only have project_id populated. A backfill is
-- provided below.
ALTER TABLE billing.invoices
    ALTER COLUMN project_id DROP NOT NULL;

-- billing.invoices.project_id was created ON DELETE CASCADE (migration
-- 003) and never fixed, despite INVOICE-DESIGN.md documenting it as
-- already RESTRICT — that document described the intended schema, not
-- what had actually shipped.
--
-- NOT currently reachable: pkg/auth/service.go DeleteProject only ever
-- soft-deletes (UPDATE ... SET deleted_at = NOW()), which never fires a
-- foreign key action. This is a LATENT hazard, not a live bug — it
-- matters the moment anything does a real DELETE FROM auth.projects: a
-- future hard-delete/purge feature, a GDPR erasure request, or a manual
-- operator cleanup. Fixing it now, while it is still theoretical, is
-- cheaper than fixing it after it has silently destroyed financial
-- records once.
--
-- SET NULL rather than RESTRICT: an invoice becoming account-level with
-- no project (exactly the shape this migration already introduces for
-- manual invoices) is the correct outcome of a project disappearing,
-- not a delete an operator has to fight through. The invoice survives;
-- only the pointer to a project that no longer exists is cleared.
--
-- The existing constraint is found by its DEFINITION (source table/column
-- and target table), not by guessing Postgres's default naming
-- convention. This is a financial schema; a wrong guess should fail
-- loudly rather than silently leave the dangerous CASCADE in place.
DO $$
DECLARE
    fk_name TEXT;
BEGIN
    SELECT tc.constraint_name INTO fk_name
    FROM information_schema.table_constraints tc
    JOIN information_schema.key_column_usage kcu
        ON kcu.constraint_name = tc.constraint_name
       AND kcu.table_schema = tc.table_schema
    JOIN information_schema.constraint_column_usage ccu
        ON ccu.constraint_name = tc.constraint_name
       AND ccu.table_schema = tc.table_schema
    WHERE tc.constraint_type = 'FOREIGN KEY'
      AND tc.table_schema = 'billing'
      AND tc.table_name = 'invoices'
      AND kcu.column_name = 'project_id'
      AND ccu.table_name = 'projects';

    IF fk_name IS NULL THEN
        RAISE EXCEPTION 'could not locate the invoices.project_id foreign key to fix its ON DELETE CASCADE';
    END IF;

    EXECUTE format('ALTER TABLE billing.invoices DROP CONSTRAINT %I', fk_name);
END $$;

ALTER TABLE billing.invoices
    ADD CONSTRAINT invoices_project_id_fkey
    FOREIGN KEY (project_id) REFERENCES auth.projects(id) ON DELETE SET NULL;

-- billing.usage_records.project_id has the same latent CASCADE hazard —
-- a real project delete would destroy metering history, which is the
-- evidence behind every invoice generated from it. RESTRICT rather than
-- SET NULL: unlike an invoice, a usage record with no project is not a
-- coherent object (nothing reads a project-less usage_records row), so
-- the correct behaviour if a hard delete is ever attempted is refusing
-- it while usage history exists, not producing orphaned rows.
DO $$
DECLARE
    fk_name TEXT;
BEGIN
    SELECT tc.constraint_name INTO fk_name
    FROM information_schema.table_constraints tc
    JOIN information_schema.key_column_usage kcu
        ON kcu.constraint_name = tc.constraint_name
       AND kcu.table_schema = tc.table_schema
    JOIN information_schema.constraint_column_usage ccu
        ON ccu.constraint_name = tc.constraint_name
       AND ccu.table_schema = tc.table_schema
    WHERE tc.constraint_type = 'FOREIGN KEY'
      AND tc.table_schema = 'billing'
      AND tc.table_name = 'usage_records'
      AND kcu.column_name = 'project_id'
      AND ccu.table_name = 'projects';

    IF fk_name IS NULL THEN
        RAISE EXCEPTION 'could not locate the usage_records.project_id foreign key to fix its ON DELETE CASCADE';
    END IF;

    EXECUTE format('ALTER TABLE billing.usage_records DROP CONSTRAINT %I', fk_name);
END $$;

ALTER TABLE billing.usage_records
    ADD CONSTRAINT usage_records_project_id_fkey
    FOREIGN KEY (project_id) REFERENCES auth.projects(id) ON DELETE RESTRICT;

-- Backfill account_id for any invoice that predates migration 007's
-- account_id column and still has it NULL. Safe to run unconditionally:
-- WHERE account_id IS NULL makes it a no-op on rows already populated.
UPDATE billing.invoices i
SET account_id = p.account_id
FROM auth.projects p
WHERE i.project_id = p.id AND i.account_id IS NULL;

ALTER TABLE billing.invoice_line_items
    ADD COLUMN IF NOT EXISTS project_id UUID REFERENCES auth.projects(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_invoice_line_items_project
    ON billing.invoice_line_items(project_id) WHERE project_id IS NOT NULL;

-- unit_price was NOT NULL, which rejects a pure flat-amount line (the
-- "manual invoice, flat negotiated price" case this whole feature exists
-- for) unless the caller supplies a meaningless 0. Amount is already the
-- authoritative figure — see the column comment in migration 009 — so
-- unit_price is now optional, matching quantity and unit which were
-- already nullable-in-practice via defaults.
ALTER TABLE billing.invoice_line_items
    ALTER COLUMN unit_price DROP NOT NULL;

ALTER TABLE billing.invoice_line_items
    ALTER COLUMN quantity DROP NOT NULL,
    ALTER COLUMN quantity DROP DEFAULT;

ALTER TABLE billing.invoice_line_items
    ALTER COLUMN unit DROP DEFAULT,
    ALTER COLUMN unit DROP NOT NULL;
