-- Reverses 072: every "build" name goes back to "kumbha".

UPDATE inference.model_probe_reports r
SET report = jsonb_set(r.report, '{checks}', (
    SELECT jsonb_agg(CASE c->>'capability'
        WHEN 'build'      THEN jsonb_set(c, '{capability}', '"kumbha"')
        WHEN 'build_text' THEN jsonb_set(c, '{capability}', '"kumbha_text"')
        ELSE c END ORDER BY ord)
    FROM jsonb_array_elements(r.report->'checks') WITH ORDINALITY AS t(c, ord)))
WHERE jsonb_typeof(r.report->'checks') = 'array'
  AND (r.report->'checks' @> '[{"capability":"build"}]' OR r.report->'checks' @> '[{"capability":"build_text"}]');

UPDATE storage.buckets SET name = 'kumbha-attachments'
WHERE name = 'build-attachments'
  AND NOT EXISTS (
      SELECT 1 FROM storage.buckets b2
      WHERE b2.project_id = storage.buckets.project_id
        AND lower(b2.name) = 'kumbha-attachments' AND b2.deleted_at IS NULL);

UPDATE billing.usage_records
SET resource_type = 'kumbha/' || substr(resource_type, length('build/') + 1)
WHERE resource_type LIKE 'build/%';

ALTER TABLE compute.instances RENAME CONSTRAINT instances_build_session_id_fkey TO instances_kumbha_session_id_fkey;
ALTER TABLE billing.build_workspace_versions RENAME CONSTRAINT build_workspace_versions_created_by_check TO kumbha_workspace_versions_created_by_check;
ALTER TABLE billing.build_workspace_versions RENAME CONSTRAINT build_workspace_versions_byte_size_check TO kumbha_workspace_versions_byte_size_check;
ALTER TABLE billing.build_workspace_versions RENAME CONSTRAINT build_workspace_versions_file_count_check TO kumbha_workspace_versions_file_count_check;
ALTER TABLE billing.build_workspace_versions RENAME CONSTRAINT build_workspace_versions_session_id_fkey TO kumbha_workspace_versions_session_id_fkey;
ALTER TABLE billing.build_workspace_versions RENAME CONSTRAINT build_workspace_versions_pkey TO kumbha_workspace_versions_pkey;
ALTER TABLE billing.build_session_secrets RENAME CONSTRAINT build_session_secrets_session_id_fkey TO kumbha_session_secrets_session_id_fkey;
ALTER TABLE billing.build_session_secrets RENAME CONSTRAINT build_session_secrets_pkey TO kumbha_session_secrets_pkey;
ALTER TABLE billing.build_plans RENAME CONSTRAINT build_plans_session_id_fkey TO kumbha_plans_session_id_fkey;
ALTER TABLE billing.build_plans RENAME CONSTRAINT build_plans_pkey TO kumbha_plans_pkey;
ALTER TABLE billing.build_messages RENAME CONSTRAINT build_messages_session_id_fkey TO kumbha_messages_session_id_fkey;
ALTER TABLE billing.build_messages RENAME CONSTRAINT build_messages_pkey TO kumbha_messages_pkey;
ALTER TABLE billing.build_events RENAME CONSTRAINT build_events_session_id_fkey TO kumbha_events_session_id_fkey;
ALTER TABLE billing.build_events RENAME CONSTRAINT build_events_session_id_launch_seq_line_no_key TO kumbha_events_session_id_launch_seq_line_no_key;
ALTER TABLE billing.build_events RENAME CONSTRAINT build_events_pkey TO kumbha_events_pkey;

ALTER INDEX inference.idx_inference_models_build          RENAME TO idx_inference_models_kumbha;
ALTER INDEX compute.idx_instances_build_session           RENAME TO idx_instances_kumbha_session;
ALTER INDEX billing.build_plans_session                   RENAME TO kumbha_plans_session;
ALTER INDEX billing.build_events_session_order            RENAME TO kumbha_events_session_order;
ALTER INDEX billing.idx_build_workspace_versions_session  RENAME TO idx_kumbha_workspace_versions_session;
ALTER INDEX billing.idx_build_messages_undelivered        RENAME TO idx_kumbha_messages_undelivered;

ALTER TABLE inference.models  RENAME COLUMN build_image_reader TO kumbha_image_reader;
ALTER TABLE inference.models  RENAME COLUMN build_priority     TO kumbha_priority;
ALTER TABLE inference.models  RENAME COLUMN build_enabled      TO kumbha_enabled;
ALTER TABLE compute.instances RENAME COLUMN build_session_id   TO kumbha_session_id;

ALTER SEQUENCE billing.build_messages_id_seq RENAME TO kumbha_messages_id_seq;
ALTER SEQUENCE billing.build_events_id_seq   RENAME TO kumbha_events_id_seq;

ALTER TABLE billing.build_workspace_versions RENAME TO kumbha_workspace_versions;
ALTER TABLE billing.build_session_secrets   RENAME TO kumbha_session_secrets;
ALTER TABLE billing.build_plans             RENAME TO kumbha_plans;
ALTER TABLE billing.build_messages          RENAME TO kumbha_messages;
ALTER TABLE billing.build_events            RENAME TO kumbha_events;
