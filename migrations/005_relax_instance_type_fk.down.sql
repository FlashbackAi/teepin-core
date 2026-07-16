-- Restore the FK to the static instance type catalog.
-- NOTE: this fails if instances reference dynamic types not present in
-- compute.instance_types; those rows must be cleaned up first.
ALTER TABLE compute.instances
    ADD CONSTRAINT instances_instance_type_id_fkey
    FOREIGN KEY (instance_type_id) REFERENCES compute.instance_types(id);
