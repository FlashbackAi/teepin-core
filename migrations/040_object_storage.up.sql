-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Catalog for Teepin S3 (pkg/objectstore) — a customer-facing object
-- storage service backed by a pluggable Backend (MinIO today; Shelby, a
-- third-party S3-compatible network evaluated in shelby-eval/, and AWS S3
-- later). This catalog, not any backend, is the source of truth for what
-- buckets/objects exist and who owns them: Shelby in particular ties one
-- API key to a single flat account-scoped namespace with no real
-- per-customer bucket concept of its own (its ListBuckets always returns
-- empty), so tenant isolation has to live here.
--
-- storage.buckets is a catalog construct only — it never maps 1:1 onto a
-- backend-side bucket, even on MinIO/S3 where that would be possible.
-- Every object's actual bytes live at a server-derived physical_key (see
-- pkg/objectstore/keys.go) that carries no customer-supplied name, so a
-- Teepin "bucket" costs nothing on the backend side and backend swapping
-- stays a config change rather than a per-bucket provisioning step.

BEGIN;

CREATE SCHEMA IF NOT EXISTS storage;

CREATE TABLE storage.buckets (
    id            UUID PRIMARY KEY,
    account_id    UUID NOT NULL REFERENCES auth.accounts(id),
    project_id    UUID NOT NULL REFERENCES auth.projects(id),
    name          VARCHAR(63) NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$'),
    -- Stamped at creation, never changes. This one column is what makes a
    -- future backend migration (Shelby -> S3) a per-bucket copy job
    -- instead of a schema change: buckets on different backends coexist.
    backend       VARCHAR(32) NOT NULL,
    object_count  BIGINT NOT NULL DEFAULT 0,
    total_bytes   BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ
);

CREATE UNIQUE INDEX idx_buckets_project_name
    ON storage.buckets (project_id, lower(name))
    WHERE deleted_at IS NULL;

CREATE INDEX idx_buckets_account
    ON storage.buckets (account_id)
    WHERE deleted_at IS NULL;

CREATE TABLE storage.objects (
    id               UUID PRIMARY KEY,
    -- ON DELETE RESTRICT, deliberately unlike most FKs in this codebase:
    -- cascading a bucket delete would silently orphan real bytes on a
    -- backend (Shelby) already confirmed to sometimes refuse to delete
    -- them. A bucket can only be removed once every object in it has gone
    -- through the real object-deletion path.
    bucket_id        UUID NOT NULL REFERENCES storage.buckets(id) ON DELETE RESTRICT,
    -- Denormalised from the bucket, same reasoning as
    -- compute.instances.account_id: every tenancy check and billing
    -- aggregation filters on these directly, without a join.
    account_id       UUID NOT NULL REFERENCES auth.accounts(id),
    project_id       UUID NOT NULL REFERENCES auth.projects(id),
    key              TEXT NOT NULL,
    physical_key     TEXT NOT NULL UNIQUE,
    size_bytes       BIGINT NOT NULL DEFAULT 0,
    -- content_type/metadata are authoritative for EVERY backend, not a
    -- Shelby-only workaround — confirmed in shelby-eval that Shelby never
    -- preserves either, so a read path must never rely on the backend
    -- handing these back.
    content_type     TEXT,
    metadata         JSONB NOT NULL DEFAULT '{}',
    -- Computed by us during upload; a backend ETag cannot be trusted
    -- uniformly across backends to mean the same thing.
    checksum_sha256  CHAR(64),
    status           VARCHAR(16) NOT NULL,
    backend          VARCHAR(32) NOT NULL,
    upload_error     TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    uploaded_at      TIMESTAMPTZ,
    deleted_at       TIMESTAMPTZ
);

CREATE UNIQUE INDEX idx_objects_bucket_key
    ON storage.objects (bucket_id, key)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_objects_bucket_key_prefix
    ON storage.objects (bucket_id, key)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_objects_status
    ON storage.objects (status, updated_at);

CREATE INDEX idx_objects_account_created
    ON storage.objects (account_id, created_at DESC);

COMMIT;
