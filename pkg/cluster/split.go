// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"errors"
	"io"
)

// SplitClient runs one machine's workloads across two runtimes behind a
// single agent identity: containers (Kubernetes) for customer compute, and
// native host processes for things that cannot live in a container, such as
// MLX model servers that need direct Metal access on macOS. The machine
// enrolls once; each workload is routed by what it is.
//
// Routing rule: a spec with no container image but an executable Command is
// native; everything else is a container. Operations addressed by instance
// ID go to whichever runtime actually holds that instance.
type SplitClient struct {
	containers Client
	native     Client
}

// NewSplitClient combines a container runtime and a native runtime.
func NewSplitClient(containers, native Client) *SplitClient {
	return &SplitClient{containers: containers, native: native}
}

var _ Client = (*SplitClient)(nil)

func isNativeSpec(spec InstanceSpec) bool {
	return spec.Image == "" && len(spec.Command) > 0
}

// holdsNative reports whether the native runtime knows this instance. Checked
// by lookup rather than assumed: native deletes are idempotent, so guessing
// wrong would silently swallow a container instance's delete.
func (s *SplitClient) holdsNative(ctx context.Context, scope Scope, id string) bool {
	_, err := s.native.GetInstanceStatus(ctx, scope, id)
	return err == nil
}

func (s *SplitClient) CreateInstance(ctx context.Context, spec InstanceSpec) (*InstanceResult, error) {
	if isNativeSpec(spec) {
		return s.native.CreateInstance(ctx, spec)
	}
	return s.containers.CreateInstance(ctx, spec)
}

func (s *SplitClient) UpdateInstance(ctx context.Context, scope Scope, spec InstanceSpec) (*InstanceResult, error) {
	if isNativeSpec(spec) {
		return s.native.UpdateInstance(ctx, scope, spec)
	}
	return s.containers.UpdateInstance(ctx, scope, spec)
}

func (s *SplitClient) DeleteInstance(ctx context.Context, scope Scope, id string) error {
	if s.holdsNative(ctx, scope, id) {
		return s.native.DeleteInstance(ctx, scope, id)
	}
	return s.containers.DeleteInstance(ctx, scope, id)
}

func (s *SplitClient) GetInstanceStatus(ctx context.Context, scope Scope, id string) (*InstanceStatus, error) {
	if st, err := s.native.GetInstanceStatus(ctx, scope, id); err == nil {
		return st, nil
	}
	return s.containers.GetInstanceStatus(ctx, scope, id)
}

func (s *SplitClient) ListInstanceStatuses(ctx context.Context, scope Scope) ([]InstanceStatus, error) {
	native, nerr := s.native.ListInstanceStatuses(ctx, scope)
	containers, cerr := s.containers.ListInstanceStatuses(ctx, scope)
	if nerr != nil && cerr != nil {
		return nil, errors.Join(nerr, cerr)
	}
	// One runtime failing must not hide the other's instances.
	return append(native, containers...), nil
}

func (s *SplitClient) StreamLogs(ctx context.Context, scope Scope, id string, opts LogOptions, w io.Writer) error {
	if s.holdsNative(ctx, scope, id) {
		return s.native.StreamLogs(ctx, scope, id, opts, w)
	}
	return s.containers.StreamLogs(ctx, scope, id, opts, w)
}

// Inventory is the container runtime's: native processes reserve no
// schedulable GPU or CPU slice, they run on what the operator kept back.
func (s *SplitClient) Inventory(ctx context.Context) ([]NodeInventory, error) {
	return s.containers.Inventory(ctx)
}

func (s *SplitClient) InstanceMetrics(ctx context.Context) ([]InstanceMetric, error) {
	native, nerr := s.native.InstanceMetrics(ctx)
	containers, cerr := s.containers.InstanceMetrics(ctx)
	if nerr != nil && cerr != nil {
		return nil, errors.Join(nerr, cerr)
	}
	return append(native, containers...), nil
}

// Healthy is the container runtime's: it gates customer container
// scheduling, and a native-only machine correctly reports not-healthy so it
// is never offered container work.
func (s *SplitClient) Healthy(ctx context.Context) bool {
	return s.containers.Healthy(ctx)
}

func (s *SplitClient) ResolveInstanceAddress(ctx context.Context, id string, port int32) (string, error) {
	if addr, err := s.native.ResolveInstanceAddress(ctx, id, port); err == nil {
		return addr, nil
	}
	return s.containers.ResolveInstanceAddress(ctx, id, port)
}

// StartupReporter is implemented by runtimes that can tell which instances a
// previous run of the agent left recorded but that no longer exist.
type StartupReporter interface {
	InstancesGoneAtStartup() []string
}

// InstancesGoneAtStartup delegates to the native runtime; container
// instances survive an agent restart (Kubernetes owns them), so there is
// nothing to report for them.
func (s *SplitClient) InstancesGoneAtStartup() []string {
	if sr, ok := s.native.(StartupReporter); ok {
		return sr.InstancesGoneAtStartup()
	}
	return nil
}

var _ StartupReporter = (*SplitClient)(nil)
