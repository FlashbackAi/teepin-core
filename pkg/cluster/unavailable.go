// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"io"
)

// Unavailable is a Client with no route to GPU capacity. Every operation
// fails with ErrClusterUnavailable.
//
// This exists so that "no capacity is reachable" is a normal, handled
// condition rather than a nil-pointer panic. It is used when the control
// plane runs in agent mode before any agent has connected — which is the
// steady state on AWS today, and a transient one whenever an agent
// reconnects.
//
// The distinction it preserves is the entire point of the control-plane
// split: accounts, billing, usage and invoices keep serving while
// compute reports a clear, retryable error. A customer whose GPU
// datacenter is unreachable can still see their bill and manage their
// team.
type Unavailable struct {
	// Reason is surfaced in logs to explain which of the several ways to
	// have no capacity applies. Not returned to customers, who get the
	// sentinel error's generic wording instead.
	Reason string
}

var _ Client = (*Unavailable)(nil)

// NewUnavailable returns a Client that refuses every cluster operation.
func NewUnavailable(reason string) *Unavailable {
	return &Unavailable{Reason: reason}
}

func (u *Unavailable) CreateInstance(context.Context, InstanceSpec) (*InstanceResult, error) {
	return nil, ErrClusterUnavailable
}

func (u *Unavailable) UpdateInstance(context.Context, Scope, InstanceSpec) (*InstanceResult, error) {
	return nil, ErrClusterUnavailable
}

func (u *Unavailable) DeleteInstance(context.Context, Scope, string) error {
	return ErrClusterUnavailable
}

func (u *Unavailable) GetInstanceStatus(context.Context, Scope, string) (*InstanceStatus, error) {
	return nil, ErrClusterUnavailable
}

// ListInstanceStatuses returns the error rather than an empty list.
//
// This matters more than it looks: the reconciler marks instances
// terminated when they are absent from this list, so returning "no
// instances" for an unreachable cluster would stop billing for every
// running instance on the platform and tell customers their workloads
// had vanished.
func (u *Unavailable) ListInstanceStatuses(context.Context, Scope) ([]InstanceStatus, error) {
	return nil, ErrClusterUnavailable
}

func (u *Unavailable) StreamLogs(context.Context, Scope, string, LogOptions, io.Writer) error {
	return ErrClusterUnavailable
}

func (u *Unavailable) Inventory(context.Context) ([]NodeInventory, error) {
	return nil, ErrClusterUnavailable
}

func (u *Unavailable) InstanceMetrics(context.Context) ([]InstanceMetric, error) {
	return nil, ErrClusterUnavailable
}

func (u *Unavailable) Healthy(context.Context) bool { return false }

func (u *Unavailable) ResolveInstanceAddress(context.Context, string, int32) (string, error) {
	return "", ErrClusterUnavailable
}
