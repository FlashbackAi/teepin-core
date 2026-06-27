-- Rollback compute schema
DROP TRIGGER IF EXISTS update_instances_updated_at ON compute.instances;
DROP TRIGGER IF EXISTS update_instance_types_updated_at ON compute.instance_types;

DROP TABLE IF EXISTS compute.instances;
DROP TABLE IF EXISTS compute.instance_types;

DROP SCHEMA IF EXISTS compute CASCADE;
