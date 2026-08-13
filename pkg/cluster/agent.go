// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// AgentClient reaches GPU capacity through agents dialling in over gRPC.
//
// This is what lets the control plane run on AWS: it holds no Kubernetes
// credentials and opens no connection to the GPU datacenter. The agent
// connects outbound and this side pushes commands down the stream it
// holds open.
//
// Instance status is served from a cache the agents keep current, not by
// asking on every read. A customer polling their instance list must not
// produce a round trip across the internet per request, and the agent
// reports transitions as they happen anyway.
type AgentClient struct {
	registry *Registry

	mu sync.RWMutex
	// statuses is the last known state of every instance, keyed by
	// instance ID. Updated by unsolicited InstanceStatus messages.
	statuses map[string]InstanceStatus
}

var _ Client = (*AgentClient)(nil)

func NewAgentClient(registry *Registry) *AgentClient {
	return &AgentClient{
		registry: registry,
		statuses: make(map[string]InstanceStatus),
	}
}

// RecordStatus stores a status reported by an agent. Called by the gRPC
// server when an InstanceStatus arrives.
func (c *AgentClient) RecordStatus(status InstanceStatus) {
	if status.InstanceID == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Terminated is final. A late-arriving "running" for an instance
	// already reported gone would otherwise resurrect it in the cache and
	// restart billing for a workload that no longer exists.
	if existing, ok := c.statuses[status.InstanceID]; ok {
		if existing.Status == "terminated" && status.Status != "terminated" {
			return
		}
	}

	c.statuses[status.InstanceID] = status
}

// ForgetInstance drops an instance from the cache after deletion.
func (c *AgentClient) ForgetInstance(instanceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.statuses, instanceID)
}

// scopeAllows reports whether a cached status is visible to a scope.
//
// The cluster enforces tenancy through label selectors; the cache has no
// labels, so the scope is checked against the tenancy recorded when the
// instance was created. Instances with no recorded tenancy are visible
// only to unrestricted scopes — failing closed, because an instance
// whose owner is unknown must not be shown to a guess.
func scopeAllows(scope Scope, status InstanceStatus) bool {
	if !scope.IsRestricted() {
		return true
	}
	if scope.ProjectID != "" && status.ProjectID != scope.ProjectID {
		return false
	}
	if scope.AccountID != "" && status.AccountID != scope.AccountID {
		return false
	}
	return true
}

func (c *AgentClient) CreateInstance(ctx context.Context, spec InstanceSpec) (*InstanceResult, error) {
	// Route to the OWNING provider when placement resolved one (home-class,
	// multi-provider). Falls back to Any() only for the single-provider
	// datacenter path where no provider was resolved. Sending a create for
	// provider B's node to provider A — the old Any()-always behaviour —
	// would run the workload on the wrong hardware the moment two providers
	// are connected.
	var session *AgentSession
	var ok bool
	if spec.ProviderID != "" {
		session, ok = c.registry.ByProvider(spec.ProviderID)
	} else {
		session, ok = c.registry.Any()
	}
	if !ok {
		return nil, ErrClusterUnavailable
	}

	ports := make([]*agentpb.PortMapping, 0, len(spec.Ports))
	for _, p := range spec.Ports {
		ports = append(ports, &agentpb.PortMapping{
			Container: int32(p.Container),
			Protocol:  p.Protocol,
		})
	}

	cmd := &agentpb.CreateInstanceCommand{
		InstanceId:      spec.InstanceID,
		AccountId:       spec.AccountID,
		ProjectId:       spec.ProjectID,
		Image:           spec.Image,
		Command:         spec.Command,
		Args:            spec.Args,
		CpuUnits:        int32(spec.CPUUnits),
		MemoryGb:        int32(spec.MemoryGB),
		GpuResource:     spec.GPUResource,
		GpuQuantity:     int32(spec.GPUQuantity),
		GpuVramGb:       int32(spec.GPUVRAMGB),
		NodeName:        spec.NodeName,
		Env:             spec.Env,
		Labels:          spec.Labels,
		Ports:           ports,
		EndpointDomain:  spec.EndpointDomain,
		EnableTls:       spec.EnableTLS,
		TlsIssuer:       spec.TLSIssuer,
		ImagePullSecret: spec.ImagePullSecret,
	}

	// The instance ID is the idempotency key: a command redelivered after
	// a reconnect must not produce a second pod.
	result, err := session.dispatch(ctx, &agentpb.ControlMessage{
		RequestId: spec.InstanceID,
		Payload:   &agentpb.ControlMessage_CreateInstance{CreateInstance: cmd},
	})
	if err != nil {
		return nil, err
	}
	if !result.Success {
		return nil, errorFromResult(result)
	}

	// Seed the cache so an immediate read-after-create does not report
	// the instance missing while waiting for the first status push.
	c.RecordStatus(InstanceStatus{
		InstanceID: spec.InstanceID,
		AccountID:  spec.AccountID,
		ProjectID:  spec.ProjectID,
		Status:     "pending",
		PodName:    result.PodName,
		ObservedAt: time.Now().UTC(),
	})

	return &InstanceResult{
		PodName:     result.PodName,
		EndpointURL: result.EndpointUrl,
		PublicIP:    result.PublicIp,
	}, nil
}

func (c *AgentClient) DeleteInstance(ctx context.Context, scope Scope, instanceID string) error {
	// Tenancy is checked here, against the cache, because the agent
	// executes what it is told: it has no view of which customer owns
	// what. An out-of-scope instance is treated as absent, and delete is
	// idempotent, so this returns success without touching it.
	if !c.visible(scope, instanceID) {
		return nil
	}

	session, ok := c.registry.Any()
	if !ok {
		return ErrClusterUnavailable
	}

	result, err := session.dispatch(ctx, &agentpb.ControlMessage{
		RequestId: "delete-" + instanceID,
		Payload: &agentpb.ControlMessage_DeleteInstance{
			DeleteInstance: &agentpb.DeleteInstanceCommand{InstanceId: instanceID},
		},
	})
	if err != nil {
		return err
	}
	if !result.Success && result.ErrorCode != agentpb.ErrorCode_ERROR_CODE_NOT_FOUND {
		return errorFromResult(result)
	}

	c.ForgetInstance(instanceID)
	return nil
}

func (c *AgentClient) GetInstanceStatus(_ context.Context, scope Scope, instanceID string) (*InstanceStatus, error) {
	c.mu.RLock()
	status, ok := c.statuses[instanceID]
	c.mu.RUnlock()

	if !ok || !scopeAllows(scope, status) {
		return nil, ErrNotFound
	}

	// No agent connected means the cache is stale and cannot be trusted
	// for anything a customer acts on.
	if c.registry.Count() == 0 {
		return nil, ErrClusterUnavailable
	}

	return &status, nil
}

func (c *AgentClient) ListInstanceStatuses(_ context.Context, scope Scope) ([]InstanceStatus, error) {
	// CRITICAL: with no agent connected this must fail rather than return
	// an empty list. The reconciler marks instances terminated when they
	// are absent from this result, so an empty list during a disconnect
	// would stop billing for every instance on the platform.
	if c.registry.Count() == 0 {
		return nil, ErrClusterUnavailable
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]InstanceStatus, 0, len(c.statuses))
	for _, status := range c.statuses {
		if scopeAllows(scope, status) {
			out = append(out, status)
		}
	}
	return out, nil
}

func (c *AgentClient) StreamLogs(ctx context.Context, scope Scope, instanceID string, opts LogOptions, w io.Writer) error {
	if !c.visible(scope, instanceID) {
		return ErrNotFound
	}

	session, ok := c.registry.Any()
	if !ok {
		return ErrClusterUnavailable
	}

	requestID := "logs-" + instanceID + "-" + uuid.New().String()[:8]
	chunks := session.openLogStream(requestID)
	defer session.closeLogStream(requestID)

	if err := session.send(&agentpb.ControlMessage{
		RequestId: requestID,
		Payload: &agentpb.ControlMessage_FetchLogs{
			FetchLogs: &agentpb.FetchLogsCommand{
				InstanceId: instanceID,
				TailLines:  int32(opts.TailLines),
				Follow:     opts.Follow,
			},
		},
	}); err != nil {
		return fmt.Errorf("%w: %v", ErrClusterUnavailable, err)
	}

	// Follow streams until the caller goes away; a bounded fetch ends at
	// EOF or the timeout below.
	deadline := time.NewTimer(pendingTimeout)
	defer deadline.Stop()

	for {
		select {
		case chunk, open := <-chunks:
			if !open {
				// Channel closed: the agent disconnected mid-stream.
				return ErrClusterUnavailable
			}
			if len(chunk.Data) > 0 {
				if _, err := w.Write(chunk.Data); err != nil {
					return err
				}
			}
			if chunk.Eof {
				return nil
			}
			if !opts.Follow {
				// Each chunk extends the window: a slow but progressing
				// log fetch should not be cut off by a fixed deadline.
				deadline.Reset(pendingTimeout)
			}

		case <-ctx.Done():
			// Customer disconnected. Not an error.
			return nil

		case <-deadline.C:
			if opts.Follow {
				// A follow stream that is simply quiet is healthy; keep
				// waiting until the caller's context ends.
				deadline.Reset(pendingTimeout)
				continue
			}
			return fmt.Errorf("%w: no log data within %s", ErrClusterUnavailable, pendingTimeout)
		}
	}
}

func (c *AgentClient) Inventory(context.Context) ([]NodeInventory, error) {
	sessions := c.registry.Sessions()
	if len(sessions) == 0 {
		return nil, ErrClusterUnavailable
	}

	var all []NodeInventory
	for _, session := range sessions {
		nodes, observedAt := session.Inventory()
		if observedAt.IsZero() {
			continue
		}
		// Stale inventory is worse than none: placing against capacity
		// reported minutes ago produces allocations the agent then
		// rejects, and the customer sees a failure rather than a
		// slightly slower success elsewhere.
		if time.Since(observedAt) > 2*time.Minute {
			continue
		}
		all = append(all, nodes...)
	}

	return all, nil
}

func (c *AgentClient) Healthy(context.Context) bool {
	return c.registry.Count() > 0
}

// visible reports whether an instance is in scope, failing closed when
// it is unknown to the cache.
func (c *AgentClient) visible(scope Scope, instanceID string) bool {
	if !scope.IsRestricted() {
		return true
	}

	c.mu.RLock()
	status, ok := c.statuses[instanceID]
	c.mu.RUnlock()

	return ok && scopeAllows(scope, status)
}

// errorFromResult maps an agent's error code to a sentinel the control
// plane branches on.
//
// The codes exist so this does not become string matching: the control
// plane reallocates on RESOURCE_EXHAUSTED and surfaces IMAGE_PULL to the
// customer, and those must not depend on an agent's error wording.
func errorFromResult(result *agentpb.CommandResult) error {
	message := result.ErrorMessage
	if message == "" {
		message = "agent reported failure"
	}

	switch result.ErrorCode {
	case agentpb.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED:
		return fmt.Errorf("%w: %s", ErrResourceExhausted, message)
	case agentpb.ErrorCode_ERROR_CODE_NOT_FOUND:
		return ErrNotFound
	case agentpb.ErrorCode_ERROR_CODE_IMAGE_PULL:
		return fmt.Errorf("%w: %s", ErrImagePull, message)
	case agentpb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:
		return errors.New(message)
	default:
		return errors.New(message)
	}
}
