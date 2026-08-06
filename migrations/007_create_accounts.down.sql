-- Revert accounts and sub-users.
-- Note: this does NOT restore the data truncated by the up migration.

BEGIN;

ALTER TABLE billing.payment_methods DROP COLUMN IF EXISTS account_id;
ALTER TABLE billing.invoices        DROP COLUMN IF EXISTS account_id;
ALTER TABLE billing.usage_records   DROP COLUMN IF EXISTS account_id;
ALTER TABLE compute.instances       DROP COLUMN IF EXISTS account_id;

DROP TABLE IF EXISTS auth.project_access;

DROP INDEX IF EXISTS auth.idx_projects_account_name;
DROP INDEX IF EXISTS auth.idx_projects_account_slug;
DROP INDEX IF EXISTS auth.idx_projects_account;
ALTER TABLE auth.projects DROP COLUMN IF EXISTS account_id;
-- Restore the original global slug uniqueness.
ALTER TABLE auth.projects ADD CONSTRAINT projects_slug_key UNIQUE (slug);

DROP INDEX IF EXISTS auth.idx_users_account;
DROP INDEX IF EXISTS auth.idx_users_account_owner;
DROP INDEX IF EXISTS auth.idx_users_account_username;
ALTER TABLE auth.users
    DROP COLUMN IF EXISTS mfa_enabled_at,
    DROP COLUMN IF EXISTS mfa_secret,
    DROP COLUMN IF EXISTS email_verified_at,
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS role,
    DROP COLUMN IF EXISTS username,
    DROP COLUMN IF EXISTS account_id;

DROP TRIGGER IF EXISTS update_accounts_updated_at ON auth.accounts;
DROP TABLE IF EXISTS auth.accounts;

COMMIT;
