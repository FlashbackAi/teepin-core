// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"bytes"
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
	f.session = NewAgentSession("provider-1", "us-east", "test", "", func(msg *agentpb.ControlMessage) error {
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

// A create carrying a ProviderID is dispatched to THAT provider's session,
// not an arbitrary one. This is the fix for the registry.Any() bug: with two
// providers connected, a create for provider-2's node must reach provider-2.
func TestAgentClient_CreateRoutesToOwningProvider(t *testing.T) {
	reply := &agentpb.CommandResult{Success: true}

	recv := func(id string) (*AgentSession, *[]string) {
		var got []string
		var s *AgentSession
		s = NewAgentSession(id, "us-east", "test", "", func(msg *agentpb.ControlMessage) error {
			got = append(got, msg.RequestId)
			go s.deliverResult(msg.RequestId, reply)
			return nil
		})
		return s, &got
	}
	sessA, gotA := recv("provider-1")
	sessB, gotB := recv("provider-2")

	r := NewRegistry()
	r.Add(sessA)
	r.Add(sessB)
	c := NewAgentClient(r)

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-b", ProviderID: "provider-2",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if len(*gotB) != 1 {
		t.Errorf("provider-2 received %d creates, want 1", len(*gotB))
	}
	if len(*gotA) != 0 {
		t.Errorf("provider-1 wrongly received %d creates (should be 0)", len(*gotA))
	}
}

// A create for a provider that is NOT connected is unavailable, rather than
// silently landing on some other provider.
func TestAgentClient_CreateUnknownProviderUnavailable(t *testing.T) {
	sessA := NewAgentSession("provider-1", "us-east", "test", "", func(*agentpb.ControlMessage) error { return nil })
	r := NewRegistry()
	r.Add(sessA)
	c := NewAgentClient(r)

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-x", ProviderID: "provider-missing",
	})
	if !errors.Is(err, ErrClusterUnavailable) {
		t.Errorf("create for absent provider = %v, want ErrClusterUnavailable", err)
	}
}

// TestAgentClient_StreamLogsRoutesToOwningProvider is the regression test
// for a latent bug found live 2026-08-22: StreamLogs used registry.Any()
// while CreateInstance already correctly routed by provider — with two
// sessions connected, a log fetch could land on the agent that never ran
// the pod, which would 404 rather than return the real logs.
func TestAgentClient_StreamLogsRoutesToOwningProvider(t *testing.T) {
	createReply := &agentpb.CommandResult{Success: true}

	recv := func(id string) (*AgentSession, *[]string) {
		var got []string
		var s *AgentSession
		s = NewAgentSession(id, "us-east", "test", "", func(msg *agentpb.ControlMessage) error {
			got = append(got, msg.RequestId)
			switch msg.Payload.(type) {
			case *agentpb.ControlMessage_CreateInstance:
				go s.deliverResult(msg.RequestId, createReply)
			case *agentpb.ControlMessage_FetchLogs:
				go s.deliverLogChunk(msg.RequestId, &agentpb.LogChunk{
					InstanceId: "inst-b", Data: []byte("hello"), Eof: true,
				})
			}
			return nil
		})
		return s, &got
	}
	sessA, gotA := recv("provider-1")
	sessB, gotB := recv("provider-2")

	r := NewRegistry()
	r.Add(sessA)
	r.Add(sessB)
	c := NewAgentClient(r)

	if _, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-b", ProviderID: "provider-2",
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	var buf bytes.Buffer
	if err := c.StreamLogs(context.Background(), AllTenants(), "inst-b", LogOptions{}, &buf); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if buf.String() != "hello" {
		t.Errorf("logs = %q, want %q", buf.String(), "hello")
	}

	fetchesOnB := 0
	for _, id := range *gotB {
		if id != "inst-b" { // the create's own RequestId is the instance id
			fetchesOnB++
		}
	}
	if fetchesOnB != 1 {
		t.Errorf("provider-2 (the owning provider) received %d FetchLogs, want 1", fetchesOnB)
	}
	for _, id := range *gotA {
		if id != "inst-b" && id != "" {
			t.Errorf("provider-1 (not the owner) wrongly received a command: %q", id)
		}
	}
	if len(*gotA) != 0 {
		t.Errorf("provider-1 received %d messages, want 0 (it never created inst-b)", len(*gotA))
	}
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

// TestAgentClient_CreateSendsAllowFilesystemOwnershipChanges is a
// regression test for a real 2026-08-26 incident: a field added to
// InstanceSpec (client.go) or PodSecurityContext (direct.go) alone does
// NOT reach a home-class provider — this codebase runs in agent mode in
// production, and every InstanceSpec field needs an explicit round trip
// through the proto (proto/agent/v1/agent.proto) and both translation
// sites (this file's CreateInstance, and pkg/agentrunner/runner.go's
// decode side) or it silently never crosses the wire. Confirms the
// encode half for AllowFilesystemOwnershipChanges specifically — the
// flag Kaniko needs to unpack a base image's layers.
func TestAgentClient_CreateSendsAllowFilesystemOwnershipChanges(t *testing.T) {
	fake := newFakeAgent(&agentpb.CommandResult{Success: true, PodName: "kaniko-build-pod"})
	c := NewAgentClient(registryWith(fake.session))

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID:                      "kaniko-build-x",
		ProjectID:                       "project-alice",
		Image:                           "gcr.io/kaniko-project/executor:v1.23.2-debug",
		AllowFilesystemOwnershipChanges: true,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	cmd := fake.lastSent().GetCreateInstance()
	if cmd == nil {
		t.Fatal("expected a CreateInstanceCommand")
	}
	if !cmd.AllowFilesystemOwnershipChanges {
		t.Error("AllowFilesystemOwnershipChanges did not reach the wire command")
	}
}

// TestAgentClient_UpdateInstanceSetsReplaceExisting confirms the encode
// side of the swap: UpdateInstance's ONLY difference from an ordinary
// CreateInstance call, on the wire, is replace_existing — everything else
// (the field mapping, which oneof case is used) is identical, since
// pkg/agentrunner's handleCreate is what actually branches on the flag.
func TestAgentClient_UpdateInstanceSetsReplaceExisting(t *testing.T) {
	fake := newFakeAgent(&agentpb.CommandResult{Success: true, PodName: "inst-swap0001-pod"})
	c := NewAgentClient(registryWith(fake.session))

	_, err := c.UpdateInstance(context.Background(), Scope{}, InstanceSpec{
		InstanceID: "inst-swap0001",
		ProjectID:  "project-alice",
		Image:      "myapp:v2",
	})
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}

	cmd := fake.lastSent().GetCreateInstance()
	if cmd == nil {
		t.Fatal("expected a CreateInstanceCommand (replace reuses the same wire message)")
	}
	if !cmd.ReplaceExisting {
		t.Error("UpdateInstance must set replace_existing on the wire command")
	}
	if cmd.Image != "myapp:v2" {
		t.Errorf("cmd.Image = %q, want myapp:v2", cmd.Image)
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

// TestAgentClient_EndpointFieldsSurviveAPartialStatusReport is the
// regression test for a real production bug found live (2026-08-18): an
// agent build that predates the endpoint fields — or a home node's
// periodic status sweep, which never reports them at all — sends
// InstanceStatus with every endpoint field at its zero value. Before this
// fix, RecordStatus's plain map assignment let that blank report erase a
// correctly-synthesized endpoint from the cache, which the reconciler
// then persisted as an erasure to the database — a customer's working
// https://inst-xxxx.teepin.com URL going 404/empty on the very next
// ~15-30s status sweep after create, for no reason visible in the create
// response itself.
func TestAgentClient_EndpointFieldsSurviveAPartialStatusReport(t *testing.T) {
	c := NewAgentClient(NewRegistry())

	// The synthesized endpoint from create (see AgentClient.CreateInstance,
	// Stage 3 plan B8) — or an agent's own correctly-populated report.
	c.RecordStatus(InstanceStatus{
		InstanceID:  "inst-endpoint1",
		Status:      "running",
		EndpointURL: "https://inst-endpoint1.teepin.com",
		DNSName:     "inst-endpoint1.teepin.com",
		TLSEnabled:  true,
		TLSReady:    true,
	})

	// A later periodic report that carries no endpoint fields at all —
	// exactly what an old/home agent sends.
	c.RecordStatus(InstanceStatus{InstanceID: "inst-endpoint1", Status: "running"})

	c.mu.RLock()
	got := c.statuses["inst-endpoint1"]
	c.mu.RUnlock()

	if got.EndpointURL != "https://inst-endpoint1.teepin.com" || got.DNSName != "inst-endpoint1.teepin.com" ||
		!got.TLSEnabled || !got.TLSReady {
		t.Errorf("endpoint fields were erased by a partial status report: %+v", got)
	}
}

// TestAgentClient_EndpointFieldsUpdateWhenReported confirms the fix above
// does not make endpoint fields permanently sticky: a report that DOES
// carry new endpoint values (e.g. the TLS-ready flip, Stage 3 plan A6)
// must still overwrite the cache normally.
func TestAgentClient_EndpointFieldsUpdateWhenReported(t *testing.T) {
	c := NewAgentClient(NewRegistry())

	c.RecordStatus(InstanceStatus{
		InstanceID:  "inst-endpoint2",
		Status:      "running",
		EndpointURL: "http://inst-endpoint2.teepin.com", // HTTP: TLS not ready yet
		DNSName:     "inst-endpoint2.teepin.com",
		TLSEnabled:  true,
		TLSReady:    false,
	})

	// cert-manager finishes issuing; the next sweep reports the flip.
	c.RecordStatus(InstanceStatus{
		InstanceID:  "inst-endpoint2",
		Status:      "running",
		EndpointURL: "https://inst-endpoint2.teepin.com",
		DNSName:     "inst-endpoint2.teepin.com",
		TLSEnabled:  true,
		TLSReady:    true,
	})

	c.mu.RLock()
	got := c.statuses["inst-endpoint2"]
	c.mu.RUnlock()

	if got.EndpointURL != "https://inst-endpoint2.teepin.com" || !got.TLSReady {
		t.Errorf("a real endpoint update was not applied: %+v", got)
	}
}

func TestAgentSession_CloseFailsPendingCommands(t *testing.T) {
	// A command in flight when the agent disconnects must fail promptly.
	// Without this it blocks until the two-minute timeout, holding an
	// HTTP request open for no reason.
	session := NewAgentSession("p1", "us-east", "test", "", func(*agentpb.ControlMessage) error {
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

	first := NewAgentSession("provider-1", "us-east", "v1", "", func(*agentpb.ControlMessage) error { return nil })
	second := NewAgentSession("provider-1", "us-east", "v2", "", func(*agentpb.ControlMessage) error { return nil })

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

	first := NewAgentSession("provider-1", "us-east", "v1", "", func(*agentpb.ControlMessage) error { return nil })
	second := NewAgentSession("provider-1", "us-east", "v2", "", func(*agentpb.ControlMessage) error { return nil })

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
	session := NewAgentSession("p1", "us-east", "test", "", func(*agentpb.ControlMessage) error { return nil })
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

	registry.Add(NewAgentSession("p1", "us-east", "test", "", func(*agentpb.ControlMessage) error { return nil }))

	if !c.Healthy(context.Background()) {
		t.Error("a connected agent should report healthy")
	}
}

// TestAgentClient_CreateRoundTripsEndpointFields covers Stage 3 defect 1
// on the agent-mode path specifically: a datacenter agent's CommandResult
// carries DnsName/TlsEnabled/TlsReady over gRPC, and AgentClient must
// deserialize every one of them onto InstanceResult, not just
// EndpointUrl/PublicIp (the two fields that existed before Stage 3).
func TestAgentClient_CreateRoundTripsEndpointFields(t *testing.T) {
	fake := newFakeAgent(&agentpb.CommandResult{
		Success:     true,
		PodName:     "inst-ep-pod",
		EndpointUrl: "https://inst-ep12345.teepin.com",
		PublicIp:    "203.0.113.9",
		DnsName:     "inst-ep12345.teepin.com",
		TlsEnabled:  true,
		TlsReady:    false,
	})
	c := NewAgentClient(registryWith(fake.session))

	result, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-ep12345",
		Ports:      []PortMapping{{Container: 80, Protocol: "tcp"}},
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	if result.DNSName != "inst-ep12345.teepin.com" {
		t.Errorf("DNSName = %q, want inst-ep12345.teepin.com", result.DNSName)
	}
	if result.PublicIP != "203.0.113.9" {
		t.Errorf("PublicIP = %q, want 203.0.113.9", result.PublicIP)
	}
	if !result.TLSEnabled {
		t.Error("TLSEnabled=false, want true (round-tripped from CommandResult)")
	}
	if result.TLSReady {
		t.Error("TLSReady=true, want false (round-tripped from CommandResult)")
	}

	// The cache seeded on create (for immediate read-after-create) must
	// carry the same fields — GetInstanceStatus is what the console
	// actually reads.
	st, err := c.GetInstanceStatus(context.Background(), AllTenants(), "inst-ep12345")
	if err != nil {
		t.Fatalf("GetInstanceStatus: %v", err)
	}
	if st.DNSName != "inst-ep12345.teepin.com" || !st.TLSEnabled {
		t.Errorf("cached status missing endpoint fields: %+v", st)
	}
}

// TestAgentClient_HomeNodeSynthesizesEndpoint covers Stage 3 plan B8: a
// home-class instance never gets endpoint fields from CommandResult (the
// home agent has no local Ingress/cert-manager — see
// cmd/teepin-agent/main.go's homeClusterClient, which always passes nil
// networking). AgentClient must synthesize the URL itself from
// spec.EndpointDomain, and report TLS ready immediately: the wildcard ACM
// cert (Stage 3 plan B7) is already issued, there is no per-instance
// cert-manager wait to track for a home instance.
func TestAgentClient_HomeNodeSynthesizesEndpoint(t *testing.T) {
	// The fake agent's reply carries NO endpoint fields at all — exactly
	// what a real home agent sends today.
	fake := newFakeAgent(&agentpb.CommandResult{Success: true, PodName: "inst-home1-pod"})
	c := NewAgentClient(registryWith(fake.session))

	result, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID:     "inst-home12345",
		ProviderID:     "provider-1",
		NodeClass:      "home",
		Ports:          []PortMapping{{Container: 8080, Protocol: "tcp"}},
		EndpointDomain: "teepin.com",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	wantDNS := "inst-home12345.teepin.com"
	if result.DNSName != wantDNS {
		t.Errorf("DNSName = %q, want synthesized %q", result.DNSName, wantDNS)
	}
	if result.EndpointURL != "https://"+wantDNS {
		t.Errorf("EndpointURL = %q, want https://%s", result.EndpointURL, wantDNS)
	}
	if !result.TLSEnabled || !result.TLSReady {
		t.Errorf("home instance must report TLS enabled AND ready immediately (ACM wildcard, no cert-manager wait): TLSEnabled=%v TLSReady=%v", result.TLSEnabled, result.TLSReady)
	}
}

// TestAgentClient_HomeNodeNoSynthesisWithoutPorts: an instance with no
// requested ports gets no endpoint, home node or not — nothing is
// listening, so a synthesized URL would just be a broken link.
func TestAgentClient_HomeNodeNoSynthesisWithoutPorts(t *testing.T) {
	fake := newFakeAgent(&agentpb.CommandResult{Success: true, PodName: "inst-home2-pod"})
	c := NewAgentClient(registryWith(fake.session))

	result, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID:     "inst-home2xxxx",
		ProviderID:     "provider-1",
		NodeClass:      "home",
		EndpointDomain: "teepin.com",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if result.DNSName != "" || result.EndpointURL != "" {
		t.Errorf("expected no synthesized endpoint with no ports, got DNSName=%q EndpointURL=%q", result.DNSName, result.EndpointURL)
	}
}

// TestAgentClient_DatacenterPathNeverSynthesizes: only NodeClass=="home"
// triggers synthesis. A datacenter instance's endpoint fields must come
// through untouched from CommandResult, even when empty (e.g. TLS not yet
// enabled — Stage 3 task A5) — synthesizing a fake URL for a datacenter
// instance would be actively wrong, since nothing there is listening on
// the ACM-wildcard-fronted edge until Phase B's cutover.
func TestAgentClient_DatacenterPathNeverSynthesizes(t *testing.T) {
	fake := newFakeAgent(&agentpb.CommandResult{Success: true, PodName: "inst-dc1-pod"})
	c := NewAgentClient(registryWith(fake.session))

	result, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID:     "inst-dc1xxxxxx",
		Ports:          []PortMapping{{Container: 80, Protocol: "tcp"}},
		EndpointDomain: "teepin.com",
		// NodeClass left empty: the datacenter path.
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if result.DNSName != "" || result.EndpointURL != "" || result.TLSEnabled || result.TLSReady {
		t.Errorf("datacenter path must not synthesize an endpoint, got %+v", result)
	}
}

// TestAgentClient_CreateInstance_HidesLabeledPodFromListImmediately is the
// regression test for a real live bug (found 2026-08-29): a customer's
// Compute list showed a transient, nameless "kaniko-build-xyz" entry for
// the whole duration of every build. CreateInstance's own immediate
// cache-seed (RecordStatus, so a read-after-create never reports the
// instance missing while waiting for the first status push) ran
// unconditionally, with no check for the "hide from Compute list" label —
// unlike DirectClient, which excludes such pods at the k8s API level via
// managedSelector before a status object even exists, AgentClient's cache
// has no labels to filter on the same way, so this seed is the one place
// that must set InstanceStatus.Hidden itself.
func TestAgentClient_CreateInstance_HidesLabeledPodFromListImmediately(t *testing.T) {
	fake := newFakeAgent(&agentpb.CommandResult{Success: true, PodName: "kaniko-build-pod"})
	c := NewAgentClient(registryWith(fake.session))

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "kaniko-build-x",
		ProjectID:  "project-alice",
		Image:      "gcr.io/kaniko-project/executor:v1.23.2-debug",
		Labels:     map[string]string{"teepin.io/kumbha-agent": "true"},
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	statuses, err := c.ListInstanceStatuses(context.Background(), AllTenants())
	if err != nil {
		t.Fatalf("ListInstanceStatuses: %v", err)
	}
	for _, st := range statuses {
		if st.InstanceID == "kaniko-build-x" {
			t.Fatalf("kaniko-build-x appeared in ListInstanceStatuses (the customer's own Compute list reads this) despite carrying the hide-from-Compute-list label: %+v", st)
		}
	}

	// A targeted lookup by its exact ID (what build.Service's own poll
	// loop does) must still find it — only the LISTING path hides it,
	// same asymmetry DirectClient's instanceSelector/managedSelector
	// already has.
	got, err := c.GetInstanceStatus(context.Background(), AllTenants(), "kaniko-build-x")
	if err != nil {
		t.Fatalf("GetInstanceStatus: %v", err)
	}
	if got.InstanceID != "kaniko-build-x" {
		t.Errorf("GetInstanceStatus did not find the hidden instance by its exact ID")
	}
}

// TestAgentClient_ListInstanceStatuses_HidesByIDPrefixEvenWithoutTheFlag
// is the regression test for a live 2026-08-31 follow-up: preserving
// Hidden across updates (see the test below) only stops FUTURE
// corruption — a pod whose cache entry was ALREADY wrong before that fix
// shipped (a 5-day-old agent pod, in the live incident) kept showing in
// the customer's Compute list even after the fix was deployed, since
// there was nothing to retroactively correct it. Filtering by the
// instance ID's own well-known prefix has no such gap: it is recomputed
// fresh on every read, with no stored state to have ever gone wrong in
// the first place. This proves the fallback alone, with Hidden left at
// its Go zero value (false), the exact state the live incident found.
func TestAgentClient_ListInstanceStatuses_HidesByIDPrefixEvenWithoutTheFlag(t *testing.T) {
	c := NewAgentClient(registryWith(newFakeAgent(&agentpb.CommandResult{Success: true}).session))

	for _, id := range []string{"kumbha-agent-4ab155f0", "kaniko-build-43368ae2", "kumbha-shot-43368ae2"} {
		c.RecordStatus(InstanceStatus{InstanceID: id, Status: "running", ProjectID: "project-alice"})
	}
	c.RecordStatus(InstanceStatus{InstanceID: "inst-real0001", Status: "running", ProjectID: "project-alice"})

	statuses, err := c.ListInstanceStatuses(context.Background(), AllTenants())
	if err != nil {
		t.Fatalf("ListInstanceStatuses: %v", err)
	}
	if len(statuses) != 1 || statuses[0].InstanceID != "inst-real0001" {
		t.Fatalf("got %+v, want only inst-real0001 — the three internal-tooling pods should all be hidden by their ID prefix alone", statuses)
	}
}

// TestAgentClient_RecordStatus_PreservesHiddenAcrossUpdate is the
// regression test for a live 2026-08-31 incident that directly followed
// the fix above: making the home-node's own reportStatuses sweep finally
// include Kaniko/Kumbha-agent pods (so their status could ever reach this
// cache at all — see Scope.IncludeHidden) meant RecordStatus started
// being called for them via a real status push for the first time. The
// wire message that arrives on that path (agentpb.InstanceStatus, via
// grpcserver.go's handleMessage) has no Hidden field, so a naive
// overwrite reset Hidden to false on the very first push after creation
// — both pod types started appearing in the customer's own Compute list
// within seconds. Hidden must survive an update exactly like the
// endpoint fields already do, since it is a fixed fact about which pod
// this is (set once, at CreateInstance), not runtime state a later
// report could legitimately change.
func TestAgentClient_RecordStatus_PreservesHiddenAcrossUpdate(t *testing.T) {
	fake := newFakeAgent(&agentpb.CommandResult{Success: true, PodName: "kaniko-build-pod"})
	c := NewAgentClient(registryWith(fake.session))

	if _, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "kaniko-build-y",
		ProjectID:  "project-alice",
		Image:      "gcr.io/kaniko-project/executor:v1.23.2-debug",
		Labels:     map[string]string{"teepin.io/kumbha-agent": "true"},
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	// Simulate the home-node's own periodic status push — exactly what
	// grpcserver.go's handleMessage does on an AgentMessage_InstanceStatus,
	// with no Hidden field on the wire to carry forward.
	c.RecordStatus(InstanceStatus{
		InstanceID: "kaniko-build-y",
		Status:     "terminated",
		ProjectID:  "project-alice",
	})

	statuses, err := c.ListInstanceStatuses(context.Background(), AllTenants())
	if err != nil {
		t.Fatalf("ListInstanceStatuses: %v", err)
	}
	for _, st := range statuses {
		if st.InstanceID == "kaniko-build-y" {
			t.Fatalf("kaniko-build-y appeared in ListInstanceStatuses after a status update reset Hidden to false: %+v", st)
		}
	}
}

// TestAgentClient_CreateInstance_OrdinaryInstanceStillListed is the
// non-regression companion: an instance with no hide-from-Compute-list
// label must appear in ListInstanceStatuses exactly as before — the
// Hidden field must default false and never suppress a real customer
// instance.
func TestAgentClient_CreateInstance_OrdinaryInstanceStillListed(t *testing.T) {
	fake := newFakeAgent(&agentpb.CommandResult{Success: true, PodName: "inst-real0001-pod"})
	c := NewAgentClient(registryWith(fake.session))

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-real0001",
		ProjectID:  "project-alice",
		Image:      "nginx:alpine",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	statuses, err := c.ListInstanceStatuses(context.Background(), AllTenants())
	if err != nil {
		t.Fatalf("ListInstanceStatuses: %v", err)
	}
	for _, st := range statuses {
		if st.InstanceID == "inst-real0001" {
			return
		}
	}
	t.Fatal("inst-real0001 did not appear in ListInstanceStatuses")
}
