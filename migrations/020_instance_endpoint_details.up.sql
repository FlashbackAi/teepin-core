-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Stage 3 (public HTTPS endpoints). compute.instances.endpoint (migration
-- 002) was the only endpoint field ever persisted -- dns_name, public_ip,
-- tls_enabled and tls_ready were rendered by the console
-- (teepin-console/src/app/(app)/compute/[id]/page.tsx) but never actually
-- written anywhere, so a customer's endpoint state never survived a
-- control-plane restart.
--
-- public_ip is VARCHAR(64), not the VARCHAR(50) migration 002 used for
-- internal_ip -- ingressPublicIP (pkg/networking/loadbalancer.go) can
-- return an ELB DNS name, not just an IP, and those exceed 50 characters.
--
-- tls_enabled/tls_ready default FALSE: an instance created before this
-- migration (or before TLS was enabled) is correctly "no TLS" rather than
-- silently claiming a certificate that was never provisioned.

BEGIN;

ALTER TABLE compute.instances
    ADD COLUMN IF NOT EXISTS dns_name    VARCHAR(255),
    ADD COLUMN IF NOT EXISTS public_ip   VARCHAR(64),
    ADD COLUMN IF NOT EXISTS tls_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS tls_ready   BOOLEAN NOT NULL DEFAULT FALSE;

COMMIT;
