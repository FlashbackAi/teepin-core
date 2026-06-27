-- Rollback auth schema
DROP TRIGGER IF EXISTS update_projects_updated_at ON auth.projects;
DROP TRIGGER IF EXISTS update_users_updated_at ON auth.users;
DROP FUNCTION IF EXISTS auth.update_updated_at_column();

DROP TABLE IF EXISTS auth.sessions;
DROP TABLE IF EXISTS auth.api_keys;
DROP TABLE IF EXISTS auth.projects;
DROP TABLE IF EXISTS auth.users;

DROP SCHEMA IF EXISTS auth CASCADE;
