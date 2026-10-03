-- Kumbha is now Teepin Build (owner decision 2026-10-03). Every database name
-- that said "kumbha" is renamed to "build". Renames are metadata-only in
-- Postgres (no table rewrite), and foreign keys, indexes and sequences follow
-- the objects they belong to; only their names change here.
--
-- Earlier migrations keep their old names on purpose: they are the history of
-- how the schema got here and must still apply, in order, to a fresh database.

-- Tables (and their sequences, indexes and constraints).
ALTER TABLE billing.kumbha_events            RENAME TO build_events;
ALTER TABLE billing.kumbha_messages          RENAME TO build_messages;
ALTER TABLE billing.kumbha_plans             RENAME TO build_plans;
ALTER TABLE billing.kumbha_session_secrets   RENAME TO build_session_secrets;
ALTER TABLE billing.kumbha_workspace_versions RENAME TO build_workspace_versions;

ALTER SEQUENCE billing.kumbha_events_id_seq   RENAME TO build_events_id_seq;
ALTER SEQUENCE billing.kumbha_messages_id_seq RENAME TO build_messages_id_seq;

-- Columns.
ALTER TABLE compute.instances RENAME COLUMN kumbha_session_id TO build_session_id;
ALTER TABLE inference.models  RENAME COLUMN kumbha_enabled      TO build_enabled;
ALTER TABLE inference.models  RENAME COLUMN kumbha_priority     TO build_priority;
ALTER TABLE inference.models  RENAME COLUMN kumbha_image_reader TO build_image_reader;

-- Indexes (primary-key and unique indexes are renamed with their constraints
-- below; renaming a constraint renames its index).
ALTER INDEX billing.idx_kumbha_messages_undelivered        RENAME TO idx_build_messages_undelivered;
ALTER INDEX billing.idx_kumbha_workspace_versions_session  RENAME TO idx_build_workspace_versions_session;
ALTER INDEX billing.kumbha_events_session_order            RENAME TO build_events_session_order;
ALTER INDEX billing.kumbha_plans_session                   RENAME TO build_plans_session;
ALTER INDEX compute.idx_instances_kumbha_session           RENAME TO idx_instances_build_session;
ALTER INDEX inference.idx_inference_models_kumbha          RENAME TO idx_inference_models_build;

-- Constraints.
ALTER TABLE billing.build_events RENAME CONSTRAINT kumbha_events_pkey TO build_events_pkey;
ALTER TABLE billing.build_events RENAME CONSTRAINT kumbha_events_session_id_launch_seq_line_no_key TO build_events_session_id_launch_seq_line_no_key;
ALTER TABLE billing.build_events RENAME CONSTRAINT kumbha_events_session_id_fkey TO build_events_session_id_fkey;
ALTER TABLE billing.build_messages RENAME CONSTRAINT kumbha_messages_pkey TO build_messages_pkey;
ALTER TABLE billing.build_messages RENAME CONSTRAINT kumbha_messages_session_id_fkey TO build_messages_session_id_fkey;
ALTER TABLE billing.build_plans RENAME CONSTRAINT kumbha_plans_pkey TO build_plans_pkey;
ALTER TABLE billing.build_plans RENAME CONSTRAINT kumbha_plans_session_id_fkey TO build_plans_session_id_fkey;
ALTER TABLE billing.build_session_secrets RENAME CONSTRAINT kumbha_session_secrets_pkey TO build_session_secrets_pkey;
ALTER TABLE billing.build_session_secrets RENAME CONSTRAINT kumbha_session_secrets_session_id_fkey TO build_session_secrets_session_id_fkey;
ALTER TABLE billing.build_workspace_versions RENAME CONSTRAINT kumbha_workspace_versions_pkey TO build_workspace_versions_pkey;
ALTER TABLE billing.build_workspace_versions RENAME CONSTRAINT kumbha_workspace_versions_session_id_fkey TO build_workspace_versions_session_id_fkey;
ALTER TABLE billing.build_workspace_versions RENAME CONSTRAINT kumbha_workspace_versions_file_count_check TO build_workspace_versions_file_count_check;
ALTER TABLE billing.build_workspace_versions RENAME CONSTRAINT kumbha_workspace_versions_byte_size_check TO build_workspace_versions_byte_size_check;
ALTER TABLE billing.build_workspace_versions RENAME CONSTRAINT kumbha_workspace_versions_created_by_check TO build_workspace_versions_created_by_check;
ALTER TABLE compute.instances RENAME CONSTRAINT instances_kumbha_session_id_fkey TO instances_build_session_id_fkey;

-- Data: usage lines for Teepin Build were recorded as "kumbha/<model>:<dir>";
-- they become "build/<model>:<dir>". Invoices and receipts keep their own
-- line text, so issued documents are unchanged; statements and the bills page
-- name the service from this prefix.
UPDATE billing.usage_records
SET resource_type = 'build/' || substr(resource_type, length('kumbha/') + 1)
WHERE resource_type LIKE 'kumbha/%';

-- The per-project attachments bucket is found by name; its objects are stored
-- by id, so renaming the catalog entry keeps every existing attachment.
UPDATE storage.buckets SET name = 'build-attachments'
WHERE name = 'kumbha-attachments'
  AND NOT EXISTS (
      SELECT 1 FROM storage.buckets b2
      WHERE b2.project_id = storage.buckets.project_id
        AND lower(b2.name) = 'build-attachments' AND b2.deleted_at IS NULL);

-- Stored model capability reports name the build checks "kumbha" and
-- "kumbha_text"; the code now reads "build" and "build_text". Rewrite the name
-- inside each check so a model's readiness survives without a re-probe.
UPDATE inference.model_probe_reports r
SET report = jsonb_set(r.report, '{checks}', (
    SELECT jsonb_agg(CASE c->>'capability'
        WHEN 'kumbha'      THEN jsonb_set(c, '{capability}', '"build"')
        WHEN 'kumbha_text' THEN jsonb_set(c, '{capability}', '"build_text"')
        ELSE c END ORDER BY ord)
    FROM jsonb_array_elements(r.report->'checks') WITH ORDINALITY AS t(c, ord)))
WHERE jsonb_typeof(r.report->'checks') = 'array'
  AND (r.report->'checks' @> '[{"capability":"kumbha"}]' OR r.report->'checks' @> '[{"capability":"kumbha_text"}]');
