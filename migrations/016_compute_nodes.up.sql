-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Persisted compute nodes, per-node credentials, and enrollment tokens.
--
-- Until now the platform had NO persisted record of its own hardware.
-- Capacity existed only as in-memory state on a live gRPC session
-- (pkg/cluster/registry.go): a control-plane restart forgot every node
-- until each agent reconnected and re-reported. That is tolerable with one
-- datacenter agent in the same rack; it is not tolerable for a node sitting
-- in someone's house on a residential link.
--
-- Two other gaps this closes:
--
--   1. Agent identity was a SINGLE SHARED SECRET (TEEPIN_AGENT_TOKEN), and
--      the provider_id an agent claimed was self-asserted and unbound to
--      that secret — any holder could claim any provider id and evict the
--      incumbent session. Per-node credentials make identity provable and
--      revocable one machine at a time.
--
--   2. compute.instances recorded no hardware, so "what ran this workload"
--      was unanswerable — needed for capacity planning and for any future
--      provider revenue share.
--
-- CLASS INTEGRITY: a node's class is fixed by the OPERATOR when the
-- enrollment token is minted, and is read from the token row at enrollment.
-- An agent never sends its class and cannot self-elevate to 'datacenter'.
-- This is why class lives on the token table, not just on the node.

BEGIN;

-- ---------------------------------------------------------------
-- 1. Nodes.
-- ---------------------------------------------------------------
CREATE TABLE compute.nodes (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- Stable identity. node_name matches the Kubernetes node name so a
    -- persisted row can be joined to live session inventory.
    node_name     TEXT NOT NULL UNIQUE,
    provider_id   TEXT NOT NULL,

    -- 'datacenter' = the existing GPU fleet. 'home' = consumer-grade
    -- capacity (this pilot). Set from the enrollment token, never from the
    -- agent, and immutable afterwards except by an explicit admin action.
    class         VARCHAR(20) NOT NULL DEFAULT 'datacenter'
        CHECK (class IN ('datacenter','home')),
    region        TEXT,

    -- Reported specs. GPU columns stay NULL/0/false on CPU-only home nodes.
    -- A consumer GPU is recorded as an ATTRIBUTE of the node, never as
    -- sellable VRAM: it cannot be MIG-partitioned or attested, so it must
    -- not enter the VRAM allocation model.
    cpu_cores     INTEGER,
    memory_gb     INTEGER,
    gpu_model     TEXT,
    gpu_count     INTEGER NOT NULL DEFAULT 0,
    mig_capable   BOOLEAN NOT NULL DEFAULT FALSE,

    os            TEXT,
    arch          TEXT,
    agent_version TEXT,

    -- enrolled: credential issued, never yet connected.
    -- online/offline: driven by heartbeat freshness.
    -- disabled: an operator took it out of service; never scheduled.
    status        VARCHAR(20) NOT NULL DEFAULT 'enrolled'
        CHECK (status IN ('enrolled','online','offline','disabled')),
    last_seen_at  TIMESTAMPTZ,

    -- Per-node credential, stored as a SHA-256 hash of a high-entropy
    -- random token. Deliberately not bcrypt: this is verified on every
    -- agent connection and the secret is random (not a low-entropy
    -- password), so a slow KDF buys nothing and would force either a table
    -- scan or a per-connection bcrypt cost. The prefix column makes the
    -- lookup a single indexed read rather than a scan.
    credential_hash   TEXT,
    credential_prefix TEXT,
    revoked_at        TIMESTAMPTZ,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_nodes_class_status ON compute.nodes(class, status);
CREATE UNIQUE INDEX idx_nodes_credential_prefix
    ON compute.nodes(credential_prefix)
    WHERE credential_prefix IS NOT NULL;

-- ---------------------------------------------------------------
-- 2. Enrollment tokens — one-time, expiring, class-bearing.
-- ---------------------------------------------------------------
-- Hashed like an API key: the plaintext is shown to the operator exactly
-- once at mint time and is never recoverable from the database.
CREATE TABLE compute.node_enrollment_tokens (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    token_hash   TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL,

    -- THE authority on what class the enrolling node becomes.
    class        VARCHAR(20) NOT NULL DEFAULT 'home'
        CHECK (class IN ('datacenter','home')),
    -- Human label for the machine this token was minted for ("mac-mini").
    label        TEXT NOT NULL,
    created_by   TEXT,

    -- Enrollment is a moment, not a standing permission: tokens are
    -- short-lived so a leaked one stops working quickly.
    expires_at   TIMESTAMPTZ NOT NULL,
    -- Single-use. Set atomically when redeemed; a second attempt fails.
    consumed_at  TIMESTAMPTZ,
    node_id      UUID REFERENCES compute.nodes(id) ON DELETE SET NULL,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_node_enrollment_tokens_prefix
    ON compute.node_enrollment_tokens(token_prefix);

-- ---------------------------------------------------------------
-- 3. Which node ran an instance.
-- ---------------------------------------------------------------
-- Nullable: every instance predating this migration has no known node, and
-- datacenter placement continues to work whether or not it is recorded.
-- ON DELETE SET NULL — removing a node must never delete billing-relevant
-- instance history.
ALTER TABLE compute.instances
    ADD COLUMN IF NOT EXISTS node_id UUID REFERENCES compute.nodes(id) ON DELETE SET NULL;

CREATE INDEX idx_instances_node ON compute.instances(node_id);

COMMIT;
