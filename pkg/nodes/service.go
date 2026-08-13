// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nodes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors callers map to HTTP status codes.
var (
	ErrTokenInvalid  = errors.New("enrollment token is invalid")
	ErrTokenExpired  = errors.New("enrollment token has expired")
	ErrTokenConsumed = errors.New("enrollment token has already been used")
	ErrNodeInvalid   = errors.New("node credential is invalid")
	ErrNodeDisabled  = errors.New("node is disabled")
	ErrNodeRevoked   = errors.New("node credential has been revoked")
	ErrNotFound      = errors.New("node not found")
)

// maxTokenTTL caps how long an enrollment token may live. Enrollment is a
// moment, not a standing permission — a token good for a month is a month of
// exposure for no benefit.
const maxTokenTTL = 24 * time.Hour

// Service manages nodes and their credentials.
type Service struct {
	db *sql.DB
}

// NewService constructs the node service.
func NewService(db *sql.DB) *Service { return &Service{db: db} }

// CreateEnrollmentToken mints a one-time, class-bearing enrollment token and
// returns the PLAINTEXT exactly once — only its hash is stored. The class is
// fixed here, by the operator; the enrolling agent can never change it.
func (s *Service) CreateEnrollmentToken(ctx context.Context, label, class, createdBy string, ttl time.Duration) (plaintext string, tok *EnrollmentToken, err error) {
	if strings.TrimSpace(label) == "" {
		return "", nil, fmt.Errorf("a label is required")
	}
	if class != ClassHome && class != ClassDatacenter {
		return "", nil, fmt.Errorf("invalid class %q", class)
	}
	if ttl <= 0 || ttl > maxTokenTTL {
		ttl = maxTokenTTL
	}

	secret, hash, prefix, err := generateSecret(enrollTokenPrefix)
	if err != nil {
		return "", nil, err
	}
	expires := time.Now().Add(ttl)

	var id uuid.UUID
	var created time.Time
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO compute.node_enrollment_tokens
		(token_hash, token_prefix, class, label, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at
	`, hash, prefix, class, label, nullString(createdBy), expires).Scan(&id, &created)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create enrollment token: %w", err)
	}

	return secret, &EnrollmentToken{
		ID:          id,
		TokenPrefix: prefix,
		Class:       class,
		Label:       label,
		CreatedBy:   createdBy,
		ExpiresAt:   expires,
		CreatedAt:   created,
	}, nil
}

// Enroll redeems a one-time token and provisions a node, returning the
// PLAINTEXT per-node credential exactly once. The node's class comes from
// the TOKEN row, never from specs — an agent cannot self-elevate.
//
// Atomic and single-use: the token row is locked FOR UPDATE and its
// consumed_at is set in the same transaction that creates the node, so two
// concurrent redemptions cannot both succeed.
func (s *Service) Enroll(ctx context.Context, token string, specs NodeSpecs) (credential string, node *Node, err error) {
	prefix := secretPrefix(token)
	if prefix == "" || !strings.HasPrefix(token, enrollTokenPrefix) {
		return "", nil, ErrTokenInvalid
	}
	if strings.TrimSpace(specs.NodeName) == "" || strings.TrimSpace(specs.ProviderID) == "" {
		return "", nil, fmt.Errorf("node_name and provider_id are required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Look up candidate tokens by prefix, lock them, then hash-match. FOR
	// UPDATE serialises concurrent redemptions of the same token.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, token_hash, class, expires_at, consumed_at
		FROM compute.node_enrollment_tokens
		WHERE token_prefix = $1
		FOR UPDATE
	`, prefix)
	if err != nil {
		return "", nil, fmt.Errorf("failed to load enrollment token: %w", err)
	}
	var (
		tokenID   uuid.UUID
		class     string
		expiresAt time.Time
		consumed  sql.NullTime
		matched   bool
	)
	for rows.Next() {
		var id uuid.UUID
		var hash, cls string
		var exp time.Time
		var con sql.NullTime
		if err := rows.Scan(&id, &hash, &cls, &exp, &con); err != nil {
			rows.Close()
			return "", nil, fmt.Errorf("failed to scan enrollment token: %w", err)
		}
		if secretMatches(token, hash) {
			tokenID, class, expiresAt, consumed, matched = id, cls, exp, con, true
			// Keep draining rows is unnecessary; break after close.
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if !matched {
		return "", nil, ErrTokenInvalid
	}
	if consumed.Valid {
		return "", nil, ErrTokenConsumed
	}
	if time.Now().After(expiresAt) {
		return "", nil, ErrTokenExpired
	}

	// Mint the per-node credential.
	cred, credHash, credPrefix, err := generateSecret(credentialPrefix)
	if err != nil {
		return "", nil, err
	}

	var (
		nodeID  uuid.UUID
		created time.Time
		updated time.Time
	)
	err = tx.QueryRowContext(ctx, `
		INSERT INTO compute.nodes
		(node_name, provider_id, class, region, cpu_cores, memory_gb,
		 gpu_model, gpu_count, mig_capable, os, arch, agent_version,
		 status, credential_hash, credential_prefix)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'enrolled',$13,$14)
		RETURNING id, created_at, updated_at
	`, specs.NodeName, specs.ProviderID, class, nullString(specs.Region),
		nullInt(specs.CPUCores), nullInt(specs.MemoryGB), nullString(specs.GPUModel),
		specs.GPUCount, specs.MIGCapable, nullString(specs.OS), nullString(specs.Arch),
		nullString(specs.AgentVersion), credHash, credPrefix,
	).Scan(&nodeID, &created, &updated)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create node: %w", err)
	}

	// Consume the token, linking it to the node it created.
	if _, err := tx.ExecContext(ctx, `
		UPDATE compute.node_enrollment_tokens
		SET consumed_at = NOW(), node_id = $1
		WHERE id = $2
	`, nodeID, tokenID); err != nil {
		return "", nil, fmt.Errorf("failed to consume enrollment token: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", nil, fmt.Errorf("failed to commit enrollment: %w", err)
	}

	return cred, &Node{
		ID: nodeID, NodeName: specs.NodeName, ProviderID: specs.ProviderID,
		Class: class, Region: specs.Region, CPUCores: specs.CPUCores,
		MemoryGB: specs.MemoryGB, GPUModel: specs.GPUModel, GPUCount: specs.GPUCount,
		MIGCapable: specs.MIGCapable, OS: specs.OS, Arch: specs.Arch,
		AgentVersion: specs.AgentVersion, Status: StatusEnrolled,
		CreatedAt: created, UpdatedAt: updated,
	}, nil
}

// AuthenticateNode resolves a presented per-node credential to its node,
// rejecting revoked or disabled ones. Constant-time hash comparison; a
// single indexed lookup by prefix, not a scan.
func (s *Service) AuthenticateNode(ctx context.Context, credential string) (*Node, error) {
	prefix := secretPrefix(credential)
	if prefix == "" || !strings.HasPrefix(credential, credentialPrefix) {
		return nil, ErrNodeInvalid
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, node_name, provider_id, class, COALESCE(region,''),
		       COALESCE(cpu_cores,0), COALESCE(memory_gb,0), COALESCE(gpu_model,''),
		       gpu_count, mig_capable, COALESCE(os,''), COALESCE(arch,''),
		       COALESCE(agent_version,''), status, credential_hash, revoked_at
		FROM compute.nodes
		WHERE credential_prefix = $1
	`, prefix)
	if err != nil {
		return nil, fmt.Errorf("failed to load node: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var n Node
		var hash sql.NullString
		if err := rows.Scan(&n.ID, &n.NodeName, &n.ProviderID, &n.Class, &n.Region,
			&n.CPUCores, &n.MemoryGB, &n.GPUModel, &n.GPUCount, &n.MIGCapable,
			&n.OS, &n.Arch, &n.AgentVersion, &n.Status, &hash, &n.RevokedAt); err != nil {
			return nil, fmt.Errorf("failed to scan node: %w", err)
		}
		if !hash.Valid || !secretMatches(credential, hash.String) {
			continue
		}
		if n.RevokedAt != nil {
			return nil, ErrNodeRevoked
		}
		if n.Status == StatusDisabled {
			return nil, ErrNodeDisabled
		}
		return &n, nil
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return nil, ErrNodeInvalid
}

// RecordHeartbeat marks a node online, refreshes last_seen_at, and updates
// its reported specs. Called on each agent inventory report.
func (s *Service) RecordHeartbeat(ctx context.Context, nodeID uuid.UUID, specs NodeSpecs) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE compute.nodes
		SET status = CASE WHEN status = 'disabled' THEN status ELSE 'online' END,
		    last_seen_at = NOW(),
		    cpu_cores = COALESCE(NULLIF($2,0), cpu_cores),
		    memory_gb = COALESCE(NULLIF($3,0), memory_gb),
		    gpu_model = COALESCE(NULLIF($4,''), gpu_model),
		    gpu_count = $5,
		    mig_capable = $6,
		    agent_version = COALESCE(NULLIF($7,''), agent_version),
		    updated_at = NOW()
		WHERE id = $1
	`, nodeID, specs.CPUCores, specs.MemoryGB, specs.GPUModel,
		specs.GPUCount, specs.MIGCapable, specs.AgentVersion)
	if err != nil {
		return fmt.Errorf("failed to record heartbeat: %w", err)
	}
	return nil
}

// UpsertSeen records that a node is connected and reporting, creating a row
// if none exists. This is the write-through path from the gRPC session: it
// gives EVERY connected node a durable record (fixing the restart-amnesia
// gap platform-wide), keyed by node_name.
//
// It is deliberately NOT the placement source of truth — the allocator still
// reads live session inventory. This table is an observability/identity
// record that trails the live sessions, so datacenter placement behaviour is
// unchanged whether or not a row exists.
//
// A home node already has a row from enrollment (with a credential); this
// updates its liveness and specs without disturbing the credential. A
// datacenter node (shared token, never enrolled) is created here on first
// sight with class 'datacenter' and no credential.
func (s *Service) UpsertSeen(ctx context.Context, class string, specs NodeSpecs) error {
	if strings.TrimSpace(specs.NodeName) == "" {
		return fmt.Errorf("node_name is required")
	}
	if class != ClassHome && class != ClassDatacenter {
		class = ClassDatacenter
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO compute.nodes
		(node_name, provider_id, class, region, cpu_cores, memory_gb,
		 gpu_model, gpu_count, mig_capable, os, arch, agent_version,
		 status, last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'online',NOW())
		ON CONFLICT (node_name) DO UPDATE SET
			-- Never resurrect a disabled node via a heartbeat.
			status = CASE WHEN compute.nodes.status = 'disabled'
			              THEN compute.nodes.status ELSE 'online' END,
			last_seen_at = NOW(),
			provider_id = EXCLUDED.provider_id,
			region = COALESCE(EXCLUDED.region, compute.nodes.region),
			cpu_cores = COALESCE(EXCLUDED.cpu_cores, compute.nodes.cpu_cores),
			memory_gb = COALESCE(EXCLUDED.memory_gb, compute.nodes.memory_gb),
			gpu_model = COALESCE(EXCLUDED.gpu_model, compute.nodes.gpu_model),
			gpu_count = EXCLUDED.gpu_count,
			mig_capable = EXCLUDED.mig_capable,
			agent_version = COALESCE(EXCLUDED.agent_version, compute.nodes.agent_version),
			-- class is authoritative from enrollment; do NOT let a
			-- write-through change it (a datacenter heartbeat must never flip
			-- an enrolled home node, and vice versa).
			updated_at = NOW()
	`, specs.NodeName, specs.ProviderID, class, nullString(specs.Region),
		nullInt(specs.CPUCores), nullInt(specs.MemoryGB), nullString(specs.GPUModel),
		specs.GPUCount, specs.MIGCapable, nullString(specs.OS), nullString(specs.Arch),
		nullString(specs.AgentVersion))
	if err != nil {
		return fmt.Errorf("failed to upsert node: %w", err)
	}
	return nil
}

// MarkStaleOffline flips online nodes with no recent heartbeat to offline.
// A disabled node is left disabled. Returns the number transitioned.
func (s *Service) MarkStaleOffline(ctx context.Context, threshold time.Duration) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE compute.nodes
		SET status = 'offline', updated_at = NOW()
		WHERE status = 'online'
		  AND (last_seen_at IS NULL OR last_seen_at < NOW() - $1::interval)
	`, threshold.String())
	if err != nil {
		return 0, fmt.Errorf("failed to mark stale nodes offline: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ListNodes returns all nodes, newest first, for the control centre.
func (s *Service) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, node_name, provider_id, class, COALESCE(region,''),
		       COALESCE(cpu_cores,0), COALESCE(memory_gb,0), COALESCE(gpu_model,''),
		       gpu_count, mig_capable, COALESCE(os,''), COALESCE(arch,''),
		       COALESCE(agent_version,''), status, last_seen_at, revoked_at,
		       created_at, updated_at
		FROM compute.nodes
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	defer rows.Close()

	out := []Node{}
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.NodeName, &n.ProviderID, &n.Class, &n.Region,
			&n.CPUCores, &n.MemoryGB, &n.GPUModel, &n.GPUCount, &n.MIGCapable,
			&n.OS, &n.Arch, &n.AgentVersion, &n.Status, &n.LastSeenAt, &n.RevokedAt,
			&n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan node: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DisableNode takes a node out of service: it is no longer schedulable and
// its credential stops authenticating. Idempotent.
func (s *Service) DisableNode(ctx context.Context, nodeID uuid.UUID) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE compute.nodes
		SET status = 'disabled', revoked_at = COALESCE(revoked_at, NOW()), updated_at = NOW()
		WHERE id = $1
	`, nodeID)
	if err != nil {
		return fmt.Errorf("failed to disable node: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func nullString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(i int) interface{} {
	if i == 0 {
		return nil
	}
	return i
}
