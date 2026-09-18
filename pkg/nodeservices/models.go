// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package nodeservices is the generic, Control-Center-driven mount/unmount
// primitive for anything Teepin itself — not a customer — runs on a node.
// An inference model server is the first user; pushing a teepin-agent
// binary update to a host is the second, planned from the start rather than
// bolted on later. Both are a `kind` under the same desired-vs-observed
// state row, reconciled the same way compute.instances already is.
//
// Deliberately separate from compute.instances: that table is customer-
// owned and billed to a project. A node_services row is operator-owned and
// lives at the node level — bolting nullable customer-billing fields onto
// compute.instances to fit a non-customer workload would be the wrong shape.
package nodeservices

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Kind discriminates what a node_services row represents. New kinds do not
// need a schema change — Config is opaque JSONB, kind-specific.
type Kind string

const (
	KindInferenceModel Kind = "inference_model"
	KindAgentBinary    Kind = "agent_binary"
)

// DesiredState is what Control Center asked for.
type DesiredState string

const (
	DesiredMounted   DesiredState = "mounted"
	DesiredUnmounted DesiredState = "unmounted"
)

// ObservedState is what whatever actually runs the thing (teepin-modeld on
// a Mac, the k3s pod's own status on a Linux node) last reported. Pending
// until the first report arrives after a mount is requested.
type ObservedState string

const (
	ObservedPending   ObservedState = "pending"
	ObservedMounted   ObservedState = "mounted"
	ObservedUnmounted ObservedState = "unmounted"
	ObservedError     ObservedState = "error"
)

// NodeService is one desired/observed pair: "this node should be running
// this thing" plus "here's what it's actually doing."
type NodeService struct {
	ID     uuid.UUID `json:"id"`
	NodeID uuid.UUID `json:"node_id"`
	Kind   Kind      `json:"kind"`
	// Config is kind-specific: for KindInferenceModel, something like
	// {"model_route": "teepin/qwen3-omni-7b", "engine": "vllm-omni"}; for
	// KindAgentBinary, {"version": "1.4.2"}. Never interpreted by this
	// package — only by whatever mounts/unmounts the underlying thing.
	Config        json.RawMessage `json:"config"`
	DesiredState  DesiredState    `json:"desired_state"`
	ObservedState ObservedState   `json:"observed_state"`
	ObservedError *string         `json:"observed_error,omitempty"`
	// ObservedEndpoint is the reachable address the reconciler resolved
	// after actually starting the thing (e.g. a k3s Service's ClusterIP
	// URL) — never operator-typed, unlike Config. nil until first
	// reported, and cleared whenever the state isn't ObservedMounted (see
	// ReportObserved), so a stale address can never outlive the mount it
	// described.
	ObservedEndpoint *string    `json:"observed_endpoint,omitempty"`
	ObservedAt       *time.Time `json:"observed_at,omitempty"`
	CreatedBy        string     `json:"created_by"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}
