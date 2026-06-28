-- Rollback registry credentials table

DROP TRIGGER IF EXISTS update_registry_credentials_updated_at ON auth.registry_credentials;
DROP INDEX IF EXISTS idx_registry_credentials_harbor_project;
DROP INDEX IF EXISTS idx_registry_credentials_project;
DROP TABLE IF EXISTS auth.registry_credentials;
