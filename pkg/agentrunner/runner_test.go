// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package agentrunner

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// stubStream is a controllable bidirectional stream.
type stubStream struct {
	mu   sync.Mutex
	sent []*agentpb.AgentMessage

	// sendErr, when set, fails every Send.
	sendErr error

	// blockSend makes Send hang forever, simulating a peer that vanished
	// without closing the connection — the Fargate-drain case.
	blockSend bool

	// incoming delivers control messages; closing it ends Recv with EOF.
	incoming chan *agentpb.ControlMessage
}

func newStubStream() *stubStream {
	return &stubStream{incoming: make(chan *agentpb.ControlMessage)}
}

func (s *stubStream) Send(msg *agentpb.AgentMessage) error {
	s.mu.Lock()
	blocked, err := s.blockSend, s.sendErr
	if !blocked && err == nil {
		s.sent = append(s.sent, msg)
	}
	s.mu.Unlock()

	if blocked {
		select {} // never returns, like a write to a black-holed socket
	}
	return err
}

func (s *stubStream) Recv() (*agentpb.ControlMessage, error) {
	msg, ok := <-s.incoming
	if !ok {
		return nil, io.EOF
	}
	return msg, nil
}

func (s *stubStream) sentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

// nullCluster satisfies cluster.Client with no behaviour, so tests
// exercise the runner's connection handling rather than Kubernetes.
type nullCluster struct{}

func (nullCluster) CreateInstance(context.Context, cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	return &cluster.InstanceResult{}, nil
}
func (nullCluster) DeleteInstance(context.Context, cluster.Scope, string) error { return nil }
func (nullCluster) GetInstanceStatus(context.Context, cluster.Scope, string) (*cluster.InstanceStatus, error) {
	return nil, cluster.ErrNotFound
}
func (nullCluster) ListInstanceStatuses(context.Context, cluster.Scope) ([]cluster.InstanceStatus, error) {
	return nil, nil
}
func (nullCluster) StreamLogs(context.Context, cluster.Scope, string, cluster.LogOptions, io.Writer) error {
	return nil
}
func (nullCluster) Inventory(context.Context) ([]cluster.NodeInventory, error) { return nil, nil }
func (nullCluster) Healthy(context.Context) bool                               { return true }
func (nullCluster) ResolveInstanceAddress(context.Context, string, int32) (string, error) {
	return "", cluster.ErrNotFound
}

// healthCluster wraps nullCluster with a configurable Healthy result, so
// tests can simulate a home node's k3s going from reachable to unreachable
// without a real Kubernetes API.
type healthCluster struct {
	nullCluster
	healthy bool
}

func (h healthCluster) Healthy(context.Context) bool { return h.healthy }

func newTestRunner() *Runner {
	return New(Config{
		ProviderID: "test-provider",
		Region:     "us-east",
		Version:    "test",
		Cluster:    nullCluster{},
	})
}

// TestRun_ReturnsWhenSendFails is the regression test for the failure
// observed in production on 2026-08-07.
//
// The control plane's Fargate tasks were replaced during a deploy. The
// old task called GracefulStop, the ALB dropped the connection, and the
// agent — blocked in Recv() with nothing arriving to error on — held a
// dead connection for TEN MINUTES without attempting to reconnect, while
// the control plane logged "cluster unreachable" every 30 seconds.
//
// Run must return when a write fails, so the caller's backoff loop
// reconnects.
func TestRun_ReturnsWhenSendFails(t *testing.T) {
	s := newStubStream()
	s.sendErr = errors.New("connection reset by peer")

	r := newTestRunner()

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), s) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil on a failed connection; the caller would not reconnect")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after a send failure: the agent would hold a dead connection")
	}
}

// TestRun_ReturnsWhenSendHangs covers the harder case: the peer is gone
// but the socket was never closed, so writes block rather than erroring.
// This is what actually happens behind a load balancer, and it is why a
// send timeout exists rather than relying on Send returning an error.
func TestRun_ReturnsWhenSendHangs(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the send timeout")
	}

	s := newStubStream()
	s.blockSend = true

	r := newTestRunner()

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), s) }()

	// Register (the first write) hangs, so the timeout must fire.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil despite a hung connection")
		}
	case <-time.After(sendTimeout + 10*time.Second):
		t.Fatalf("Run did not return within %s of a hung send", sendTimeout)
	}
}

// TestHeartbeat_WritesWhenNothingElseDoes verifies the liveness signal
// exists at all.
//
// Status sweeps only send on a CHANGE, so a steady-state agent can go
// minutes without writing. Without a heartbeat there is nothing to fail,
// and a dead connection stays undetected however good the send-failure
// handling is.
func TestHeartbeat_WritesWhenNothingElseDoes(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for a heartbeat tick")
	}

	s := newStubStream()
	r := newTestRunner()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = r.Run(ctx, s) }()

	// Registration plus the initial inventory/status reports land
	// immediately; wait past one heartbeat and confirm writes continue
	// with no instances and no inventory changes.
	time.Sleep(2 * time.Second)
	baseline := s.sentCount()

	time.Sleep(heartbeatInterval + 3*time.Second)

	if s.sentCount() <= baseline {
		t.Errorf("no writes in %s of idle time: a dead connection would go unnoticed",
			heartbeatInterval+3*time.Second)
	}
}

func TestRun_RegistersFirst(t *testing.T) {
	s := newStubStream()
	r := newTestRunner()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = r.Run(ctx, s) }()
	time.Sleep(500 * time.Millisecond)

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.sent) == 0 {
		t.Fatal("nothing sent")
	}
	// The control plane cannot route anything until it knows which
	// provider is speaking, so this must be the first message.
	if s.sent[0].GetRegister() == nil {
		t.Errorf("first message was %T, want RegisterRequest", s.sent[0].Payload)
	}
	if s.sent[0].GetRegister().ProviderId != "test-provider" {
		t.Errorf("ProviderId = %q", s.sent[0].GetRegister().ProviderId)
	}
}

// instanceCluster reports a fixed set of running instances.
type instanceCluster struct {
	nullCluster
	instances []cluster.InstanceStatus
}

func (c instanceCluster) ListInstanceStatuses(context.Context, cluster.Scope) ([]cluster.InstanceStatus, error) {
	return c.instances, nil
}

// capturingCluster records the InstanceSpec CreateInstance was actually
// called with, so a test can assert on how a command was decoded rather
// than only on whether the call succeeded.
type capturingCluster struct {
	nullCluster
	captured cluster.InstanceSpec
}

func (c *capturingCluster) CreateInstance(_ context.Context, spec cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	c.captured = spec
	return &cluster.InstanceResult{PodName: spec.InstanceID}, nil
}

// TestHandleCreate_DecodesAllowFilesystemOwnershipChanges is a
// regression test for a real 2026-08-26 incident: a field added to
// InstanceSpec/PodSecurityContext alone does not reach a home-class
// provider — this codebase runs in agent mode in production, and every
// InstanceSpec field needs an explicit round trip through the proto and
// both translation sites (pkg/cluster/agent.go's encode side, and this
// package's decode side) or it silently never reaches the local
// cluster.Client. Confirms the decode half specifically.
func TestHandleCreate_DecodesAllowFilesystemOwnershipChanges(t *testing.T) {
	fc := &capturingCluster{}
	r := New(Config{ProviderID: "test-provider", Cluster: fc})
	s := newStubStream()

	r.handleCreate(context.Background(), s, "req-1", &agentpb.CreateInstanceCommand{
		InstanceId:                      "kaniko-build-x",
		Image:                           "gcr.io/kaniko-project/executor:v1.23.2-debug",
		AllowFilesystemOwnershipChanges: true,
	})

	if !fc.captured.AllowFilesystemOwnershipChanges {
		t.Error("AllowFilesystemOwnershipChanges was not decoded onto the local InstanceSpec")
	}
}

// TestReconnect_ReportsEveryInstanceAgain is the regression test for a
// bug that made running, billed instances invisible to their owner.
//
// The control plane holds instance statuses in memory, so a restarted
// task or a fresh connection starts from nothing. The agent's status
// sweep only sends CHANGES — and `lastReported` was created once at
// startup and never reset. After a reconnect the agent therefore
// considered every unchanged instance "already reported" and stayed
// silent, so the control plane never learned they existed.
//
// Observed 2026-08-07: two pods running in the cluster, each visible
// under only one of two API keys, while both were being billed.
func TestReconnect_ReportsEveryInstanceAgain(t *testing.T) {
	live := []cluster.InstanceStatus{
		{InstanceID: "inst-aaaa1111", Status: "running", ProjectID: "p1"},
		{InstanceID: "inst-bbbb2222", Status: "running", ProjectID: "p1"},
	}

	r := New(Config{
		ProviderID: "test-provider",
		Cluster:    instanceCluster{instances: live},
	})

	countStatuses := func(s *stubStream) int {
		s.mu.Lock()
		defer s.mu.Unlock()
		n := 0
		for _, msg := range s.sent {
			if msg.GetInstanceStatus() != nil {
				n++
			}
		}
		return n
	}

	// First connection: both instances reported.
	first := newStubStream()
	ctx1, cancel1 := context.WithCancel(context.Background())
	go func() { _ = r.Run(ctx1, first) }()
	time.Sleep(700 * time.Millisecond)
	cancel1()

	if got := countStatuses(first); got < 2 {
		t.Fatalf("first connection reported %d statuses, want at least 2", got)
	}

	// Second connection, same Runner, nothing changed in the cluster.
	// Every instance must be reported again: the control plane on the
	// other end has no memory of the first connection.
	second := newStubStream()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { _ = r.Run(ctx2, second) }()
	time.Sleep(700 * time.Millisecond)

	if got := countStatuses(second); got < 2 {
		t.Errorf(
			"after reconnect only %d statuses were sent, want 2: unchanged instances "+
				"stay invisible to the control plane and are billed but unlistable",
			got)
	}
}

// reportInventory sends the outgoing GPUInventory.cluster_ready field as the
// fresh Cluster.Healthy(ctx) result — checked EVERY report, not cached from
// startup, so a home node's k3s crashing mid-session is reflected within one
// inventoryInterval rather than frozen at whatever it was when the agent
// connected.
func TestReportInventory_ReportsClusterReady(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		r := New(Config{
			ProviderID: "home-sreek",
			Region:     "home",
			Version:    "test",
			Cluster:    healthCluster{healthy: healthy},
		})
		s := newStubStream()

		r.reportInventory(context.Background(), s)

		if s.sentCount() != 1 {
			t.Fatalf("healthy=%v: got %d sent messages, want 1", healthy, s.sentCount())
		}
		inv := s.sent[0].GetInventory()
		if inv == nil {
			t.Fatalf("healthy=%v: first message was %T, want GPUInventory", healthy, s.sent[0].Payload)
		}
		if inv.ClusterReady != healthy {
			t.Errorf("healthy=%v: ClusterReady = %v, want %v", healthy, inv.ClusterReady, healthy)
		}
	}
}

func TestRun_ReturnsOnStreamEOF(t *testing.T) {
	s := newStubStream()
	r := newTestRunner()

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), s) }()

	time.Sleep(300 * time.Millisecond)
	close(s.incoming) // control plane closed the stream cleanly

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("clean EOF returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on EOF")
	}
}
