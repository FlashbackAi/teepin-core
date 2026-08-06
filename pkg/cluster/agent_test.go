// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// fakeAgent is a session whose sent commands are captured, with an
// optional auto-responder so dispatch() completes.
type fakeAgent struct {
	session *AgentSession

	mu   sync.Mutex
	sent []*agentpb.ControlMessage
}

// newFakeAgent wires a session that answers every command with reply.
// A nil reply leaves commands unanswered, for timeout behaviour.
func newFakeAgent(reply *agentpb.CommandResult) *fakeAgent {
	f := &fakeAgent{}
	f.session = NewAgentSession("provider-1", "us-east", "test", func(msg *agentpb.ControlMessage) error {
		f.mu.Lock()
		f.sent = append(f.sent, msg)
		f.mu.Unlock()

		if reply != nil {
			// Answer asynchronously, as a real agent would.
			go f.session.deliverResult(msg.RequestId, reply)
		}
		return nil
	})
	return f
}

func (f *fakeAgent) lastSent() *agentpb.ControlMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return nil
	}
	return f.sent[len(f.sent)-1]
}

func registryWith(session *AgentSession) *Registry {
	r := NewRegistry()
	r.Add(session)
	return r
}

func TestAgentClient_NoAgentIsUnavailable(t *testing.T) {
	c := NewAgentClient(NewRegistry())

	_, err := c.CreateInstance(context.Background(), InstanceSpec{InstanceID: "inst-x"})
	if !errors.Is(err, ErrClusterUnavailable) {
		t.Errorf("CreateInstance with no agent = %v, want ErrClusterUnavailable", err)
	}
}

// TestAgentClient_ListFailsWhenNoAgent guards the same hazard as the
// reconciler test: an empty list would be read as "every instance is
// gone" and stop billing platform-wide.
func TestAgentClient_ListFailsWhenNoAgent(t *testing.T) {
	c := NewAgentClient(NewRegistry())
	c.RecordStatus(InstanceStatus{InstanceID: "inst-live", Status: "running"})

	_, err := c.ListInstanceStatuses(context.Background(), AllTenants())
	if !errors.Is(err, ErrClusterUnavailable) {
		t.Fatalf("List with no agent = %v, want ErrClusterUnavailable (an empty list would stop all billing)", err)
	}
}

func TestAgentClient_CreateSendsCommandAndCachesStatus(t *testing.T) {
	fake := newFakeAgent(&agentpb.CommandResult{Success: true, PodName: "inst-abc-pod"})
	c := NewAgentClient(registryWith(fake.session))

	result, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID:  "inst-abc123",
		ProjectID:   "project-alice",
		Image:       "nginx:latest",
		GPUResource: "nvidia.com/mig-2g.20gb",
		GPUQuantity: 1,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if result.PodName != "inst-abc-pod" {
		t.Errorf("PodName = %q, want inst-abc-pod", result.PodName)
	}

	// The request ID must be the instance ID: that is the idempotency
	// key preventing a redelivered command creating a second pod.
	sent := fake.lastSent()
	if sent.RequestId != "inst-abc123" {
		t.Errorf("RequestId = %q, want the instance ID for idempotency", sent.RequestId)
	}

	cmd := sent.GetCreateInstance()
	if cmd == nil {
		t.Fatal("expected a CreateInstanceCommand")
	}
	if cmd.GpuResource != "nvidia.com/mig-2g.20gb" {
		t.Errorf("GpuResource = %q, want the allocated resource", cmd.GpuResource)
	}

	// Read-after-create must not report the instance missing while the
	// first status push is still in flight.
	st, err := c.GetInstanceStatus(context.Background(), ProjectScope("project-alice"), "inst-abc123")
	if err != nil {
		t.Fatalf("read-after-create failed: %v", err)
	}
	if st.Status != "pending" {
		t.Errorf("Status = %q, want pending", st.Status)
	}
}

func TestAgentClient_ResourceExhaustedIsTyped(t *testing.T) {
	fake := newFakeAgent(&agentpb.CommandResult{
		Success:      false,
		ErrorCode:    agentpb.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED,
		ErrorMessage: "mig slice taken",
	})
	c := NewAgentClient(registryWith(fake.session))

	_, err := c.CreateInstance(context.Background(), InstanceSpec{InstanceID: "inst-x"})

	// The control plane reallocates on this rather than failing the
	// customer's request, so the code must survive the wire as a
	// sentinel and not become an opaque string.
	if !errors.Is(err, ErrResourceExhausted) {
		t.Errorf("error = %v, want ErrResourceExhausted", err)
	}
}

func TestAgentClient_ImagePullIsTyped(t *testing.T) {
	fake := newFakeAgent(&agentpb.CommandResult{
		Success:      false,
		ErrorCode:    agentpb.ErrorCode_ERROR_CODE_IMAGE_PULL,
		ErrorMessage: "manifest unknown",
	})
	c := NewAgentClient(registryWith(fake.session))

	_, err := c.CreateInstance(context.Background(), InstanceSpec{InstanceID: "inst-x"})
	if !errors.Is(err, ErrImagePull) {
		t.Errorf("error = %v, want ErrImagePull", err)
	}
}

func TestAgentClient_ScopeHidesOtherTenants(t *testing.T) {
	fake := newFakeAgent(&agentpb.CommandResult{Success: true})
	c := NewAgentClient(registryWith(fake.session))

	c.RecordStatus(InstanceStatus{
		InstanceID: "inst-bob0001",
		ProjectID:  "project-bob",
		Status:     "running",
	})

	// The cache has no label selector to lean on, so the scope check
	// here is the entire tenancy boundary for agent mode.
	_, err := c.GetInstanceStatus(context.Background(), ProjectScope("project-alice"), "inst-bob0001")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant read = %v, want ErrNotFound", err)
	}

	statuses, err := c.ListInstanceStatuses(context.Background(), ProjectScope("project-alice"))
	if err != nil {
		t.Fatalf("ListInstanceStatuses: %v", err)
	}
	if len(statuses) != 0 {
		t.Errorf("Alice sees %d of Bob's instances, want 0", len(statuses))
	}
}

func TestAgentClient_DeleteDoesNotTouchOtherTenants(t *testing.T) {
	fake := newFakeAgent(&agentpb.CommandResult{Success: true})
	c := NewAgentClient(registryWith(fake.session))

	c.RecordStatus(InstanceStatus{
		InstanceID: "inst-bob0001",
		ProjectID:  "project-bob",
		Status:     "running",
	})

	if err := c.DeleteInstance(context.Background(), ProjectScope("project-alice"), "inst-bob0001"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}

	// No delete command may reach the agent at all: the agent executes
	// blindly, so a command sent here would destroy Bob's workload.
	if sent := fake.lastSent(); sent != nil {
		t.Errorf("a delete command was sent for another tenant's instance: %+v", sent)
	}
}

func TestAgentClient_TerminatedIsFinal(t *testing.T) {
	c := NewAgentClient(NewRegistry())

	c.RecordStatus(InstanceStatus{InstanceID: "inst-a", Status: "terminated"})
	// A stale "running" arriving late, e.g. a status sent before the
	// delete but delivered after it.
	c.RecordStatus(InstanceStatus{InstanceID: "inst-a", Status: "running"})

	c.mu.RLock()
	got := c.statuses["inst-a"].Status
	c.mu.RUnlock()

	// Resurrecting a terminated instance restarts billing for a workload
	// that no longer exists.
	if got != "terminated" {
		t.Errorf("status = %q, want terminated to be final", got)
	}
}

func TestAgentSession_CloseFailsPendingCommands(t *testing.T) {
	// A command in flight when the agent disconnects must fail promptly.
	// Without this it blocks until the two-minute timeout, holding an
	// HTTP request open for no reason.
	session := NewAgentSession("p1", "us-east", "test", func(*agentpb.ControlMessage) error {
		return nil // never answers
	})

	done := make(chan error, 1)
	go func() {
		_, err := session.dispatch(context.Background(), &agentpb.ControlMessage{
			RequestId: "req-1",
			Payload:   &agentpb.ControlMessage_DeleteInstance{DeleteInstance: &agentpb.DeleteInstanceCommand{}},
		})
		done <- err
	}()

	// Let dispatch register before closing.
	time.Sleep(50 * time.Millisecond)
	session.Close()

	select {
	case <-done:
		// Either an error or a synthetic failure result is fine; what
		// matters is that it returned rather than hanging.
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not return after the session closed")
	}
}

func TestRegistry_ReconnectReplacesSession(t *testing.T) {
	r := NewRegistry()

	first := NewAgentSession("provider-1", "us-east", "v1", func(*agentpb.ControlMessage) error { return nil })
	second := NewAgentSession("provider-1", "us-east", "v2", func(*agentpb.ControlMessage) error { return nil })

	r.Add(first)
	r.Add(second)

	// An agent that lost its network leaves a stale session behind;
	// refusing the reconnect would strand that provider.
	if r.Count() != 1 {
		t.Errorf("Count = %d, want 1 after reconnect", r.Count())
	}

	current, ok := r.Get("provider-1")
	if !ok || current != second {
		t.Error("reconnect must replace the stale session")
	}
}

func TestRegistry_RemoveIgnoresSupersededSession(t *testing.T) {
	r := NewRegistry()

	first := NewAgentSession("provider-1", "us-east", "v1", func(*agentpb.ControlMessage) error { return nil })
	second := NewAgentSession("provider-1", "us-east", "v2", func(*agentpb.ControlMessage) error { return nil })

	r.Add(first)
	r.Add(second)

	// The old connection's cleanup arrives after the new one registered.
	// It must not evict the live session, or a reconnect would leave the
	// provider unreachable until the next retry.
	r.Remove(first)

	if r.Count() != 1 {
		t.Errorf("Count = %d, want the newer session to survive", r.Count())
	}
}

func TestAgentClient_StaleInventoryIsIgnored(t *testing.T) {
	session := NewAgentSession("p1", "us-east", "test", func(*agentpb.ControlMessage) error { return nil })
	session.setInventory([]NodeInventory{{NodeName: "gpu-1", GPUCount: 1}})

	// Backdate past the freshness window.
	session.mu.Lock()
	session.inventoryAt = time.Now().Add(-5 * time.Minute)
	session.mu.Unlock()

	c := NewAgentClient(registryWith(session))

	nodes, err := c.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}

	// Placing against minutes-old capacity produces allocations the
	// agent then rejects; the customer sees a failure instead of a
	// slightly slower success elsewhere.
	if len(nodes) != 0 {
		t.Errorf("stale inventory returned %d nodes, want 0", len(nodes))
	}
}

func TestAgentClient_HealthyTracksConnectedAgents(t *testing.T) {
	registry := NewRegistry()
	c := NewAgentClient(registry)

	if c.Healthy(context.Background()) {
		t.Error("no agents connected must not report healthy")
	}

	registry.Add(NewAgentSession("p1", "us-east", "test", func(*agentpb.ControlMessage) error { return nil }))

	if !c.Healthy(context.Background()) {
		t.Error("a connected agent should report healthy")
	}
}
