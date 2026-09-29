DROP INDEX IF EXISTS compute.idx_instances_stopped;
ALTER TABLE compute.instances
    DROP COLUMN IF EXISTS resumed_at,
    DROP COLUMN IF EXISTS stopped_at,
    DROP COLUMN IF EXISTS launch_spec;
