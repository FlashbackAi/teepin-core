// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package nodes manages persisted compute nodes: their enrollment (a
// one-time, class-bearing token exchanged for a per-node credential), their
// authentication on each agent connection, and their heartbeat-driven
// online/offline state.
//
// It exists because capacity used to be in-memory only (a live gRPC
// session), which a control-plane restart forgot. This package is the
// durable record, and the home-compute pilot's provisioning gate for
// consumer hardware. It is inert unless HOME_COMPUTE_ENABLED is set — the
// datacenter path continues to authenticate with the shared agent token.
package nodes

import (
	"time"

	"github.com/google/uuid"
)

// Class distinguishes the existing GPU fleet from consumer-grade capacity.
// It is authoritative on the enrollment TOKEN, copied onto the node at
// enrollment, and never taken from anything the agent asserts.
const (
	ClassDatacenter = "datacenter"
	ClassHome       = "home"
)

// Node status lifecycle: enrolled (credential issued, not yet connected) →
// online/offline (driven by heartbeat freshness) ; disabled is an operator
// action that removes it from scheduling regardless of connectivity.
const (
	StatusEnrolled = "enrolled"
	StatusOnline   = "online"
	StatusOffline  = "offline"
	StatusDisabled = "disabled"
)

// Node is a persisted compute node.
type Node struct {
	ID         uuid.UUID `json:"id"`
	NodeName   string    `json:"node_name"`
	ProviderID string    `json:"provider_id"`
	Class      string    `json:"class"`
	Region     string    `json:"region,omitempty"`

	// Specs. GPU fields are zero/false on CPU-only home nodes; a consumer
	// GPU is reported here as an attribute, never as sellable VRAM.
	CPUCores   int    `json:"cpu_cores,omitempty"`
	MemoryGB   int    `json:"memory_gb,omitempty"`
	GPUModel   string `json:"gpu_model,omitempty"`
	GPUCount   int    `json:"gpu_count"`
	MIGCapable bool   `json:"mig_capable"`

	// Rentable capacity: how much of the DETECTED specs the operator has
	// chosen to offer for rent. 0 (the default) means the node offers
	// nothing until a reservation is set, so it is never silently rented
	// out. Detected specs above are the ceiling these may not exceed.
	RentableCPUCores int `json:"rentable_cpu_cores"`
	RentableMemoryGB int `json:"rentable_memory_gb"`

	OS           string `json:"os,omitempty"`
	Arch         string `json:"arch,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`

	Status     string     `json:"status"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`

	// credentialHash / credentialPrefix are never serialised — the plaintext
	// is shown once at enrollment and only the hash is stored.
	RevokedAt *time.Time `json:"revoked_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NodeSpecs is what an enrolling agent reports about its host. The class is
// deliberately ABSENT: it is not the agent's to choose.
type NodeSpecs struct {
	NodeName     string
	ProviderID   string
	Region       string
	CPUCores     int
	MemoryGB     int
	GPUModel     string
	GPUCount     int
	MIGCapable   bool
	OS           string
	Arch         string
	AgentVersion string
}

// EnrollmentToken is an operator-minted, one-time, expiring credential that
// carries the class the enrolling node will take.
type EnrollmentToken struct {
	ID          uuid.UUID  `json:"id"`
	TokenPrefix string     `json:"token_prefix"`
	Class       string     `json:"class"`
	Label       string     `json:"label"`
	CreatedBy   string     `json:"created_by,omitempty"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ConsumedAt  *time.Time `json:"consumed_at,omitempty"`
	NodeID      *uuid.UUID `json:"node_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}
