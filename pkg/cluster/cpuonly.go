// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"io"
)

// CPUOnly is a Client for a node that contributes CPU capacity and runs no
// GPU workloads yet — the home-compute pilot's Stage 1 shape. It reports an
// EMPTY (but successful) inventory and status list, so the agent connects,
// heartbeats, and gets its durable node record without a GPU cluster present.
//
// It differs from Unavailable deliberately: Unavailable ERRORS on Inventory
// and ListInstanceStatuses (the correct behaviour for "the GPU datacenter is
// unreachable"), whereas a CPU-only home node genuinely HAS no GPU inventory
// and no instances — an empty success, not a failure. Returning the error
// here would spam the agent's logs and stop it heartbeating.
//
// Instance execution is not wired in Stage 1: Create/Delete/Logs return
// ErrClusterUnavailable until Stage 2 swaps in a k3s-backed DirectClient.
type CPUOnly struct{}

var _ Client = (*CPUOnly)(nil)

// NewCPUOnly returns a CPU-only cluster client.
func NewCPUOnly() *CPUOnly { return &CPUOnly{} }

func (c *CPUOnly) CreateInstance(context.Context, InstanceSpec) (*InstanceResult, error) {
	return nil, ErrClusterUnavailable
}

func (c *CPUOnly) UpdateInstance(context.Context, Scope, InstanceSpec) (*InstanceResult, error) {
	return nil, ErrClusterUnavailable
}

func (c *CPUOnly) DeleteInstance(context.Context, Scope, string) error {
	return ErrClusterUnavailable
}

func (c *CPUOnly) GetInstanceStatus(context.Context, Scope, string) (*InstanceStatus, error) {
	return nil, ErrNotFound
}

// ListInstanceStatuses returns an empty list, not an error: a CPU-only home
// node in Stage 1 runs nothing, and that is a truthful, successful report.
func (c *CPUOnly) ListInstanceStatuses(context.Context, Scope) ([]InstanceStatus, error) {
	return []InstanceStatus{}, nil
}

func (c *CPUOnly) StreamLogs(context.Context, Scope, string, LogOptions, io.Writer) error {
	return ErrClusterUnavailable
}

// Inventory returns an empty list: no GPU capacity, reported successfully.
// The control plane records the node under its own identity (the CPU-only
// branch of reportInventorySeen) so it still shows online.
func (c *CPUOnly) Inventory(context.Context) ([]NodeInventory, error) {
	return []NodeInventory{}, nil
}

func (c *CPUOnly) Healthy(context.Context) bool { return true }

func (c *CPUOnly) ResolveInstanceAddress(context.Context, string, int32) (string, error) {
	return "", ErrClusterUnavailable
}
