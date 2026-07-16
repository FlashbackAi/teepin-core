-- Instance types are now derived from live GPU hardware discovery
-- (e.g. gpu.h100.2g.20gb, gpu.a100.custom-25gb), so they can no longer
-- be foreign-keyed to the static compute.instance_types catalog.
ALTER TABLE compute.instances DROP CONSTRAINT IF EXISTS instances_instance_type_id_fkey;
