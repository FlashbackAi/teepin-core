-- Stop-and-hold for instances with a persistent disk.
--
-- When an account runs out of credit its compute must stop, but a disk-backed
-- instance cannot simply be deleted: the customer's data lives on the disk and
-- is held for a period so a top-up can bring the instance back. That needs:
--
--   launch_spec  the encrypted cluster.InstanceSpec (image, command, args, env
--                including secrets, ports, placement), so "Start" can relaunch
--                the same instance. Stored only for instances that have a
--                disk; NULL otherwise. AES-GCM sealed, never plaintext.
--   stopped_at   when the pod was stopped with the disk kept (status 'stopped').
--   resumed_at   when it was last started again. Billing resumes from here so
--                the stopped gap is never charged as running time.
ALTER TABLE compute.instances
    ADD COLUMN launch_spec BYTEA,
    ADD COLUMN stopped_at  TIMESTAMPTZ,
    ADD COLUMN resumed_at  TIMESTAMPTZ;

CREATE INDEX idx_instances_stopped
    ON compute.instances (account_id)
    WHERE status = 'stopped' AND terminated_at IS NULL;
