// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// fakeNodeAuth resolves exactly one credential to one identity.
type fakeNodeAuth struct {
	credential string
	identity   *NodeIdentity
}

func (f *fakeNodeAuth) AuthenticateNode(_ context.Context, cred string) (*NodeIdentity, error) {
	if cred == f.credential {
		return f.identity, nil
	}
	return nil, errors.New("invalid")
}

// The shared datacenter token authenticates and yields NO node identity —
// the existing path, unchanged, regardless of whether a node authenticator
// is present.
func TestResolveCredential_SharedTokenUnchanged(t *testing.T) {
	for _, withNodeAuth := range []bool{false, true} {
		s := NewAgentServer(nil, nil, "shared-secret")
		if withNodeAuth {
			s = s.WithNodeAuthenticator(&fakeNodeAuth{credential: "tnc_x", identity: &NodeIdentity{}})
		}
		id, err := s.resolveCredential(context.Background(), "shared-secret")
		if err != nil {
			t.Fatalf("withNodeAuth=%v: shared token rejected: %v", withNodeAuth, err)
		}
		if id != nil {
			t.Errorf("withNodeAuth=%v: shared token yielded an identity; want nil (datacenter path)", withNodeAuth)
		}
	}
}

// A per-node credential resolves to its identity — including the class, which
// the caller then trusts over the RegisterRequest.
func TestResolveCredential_PerNodeIdentity(t *testing.T) {
	want := &NodeIdentity{NodeName: "mac-mini", ProviderID: "home-sreek", Class: "home"}
	s := NewAgentServer(nil, nil, "shared-secret").
		WithNodeAuthenticator(&fakeNodeAuth{credential: "tnc_valid", identity: want})

	id, err := s.resolveCredential(context.Background(), "tnc_valid")
	if err != nil {
		t.Fatalf("valid per-node credential rejected: %v", err)
	}
	if id == nil || id.Class != "home" || id.NodeName != "mac-mini" {
		t.Fatalf("resolved identity = %+v, want %+v", id, want)
	}
}

// A credential that is neither the shared token nor a known node is rejected.
func TestResolveCredential_UnknownRejected(t *testing.T) {
	s := NewAgentServer(nil, nil, "shared-secret").
		WithNodeAuthenticator(&fakeNodeAuth{credential: "tnc_valid", identity: &NodeIdentity{}})

	if _, err := s.resolveCredential(context.Background(), "tnc_bogus"); err == nil {
		t.Fatal("unknown credential accepted")
	}
}

// With no shared token and no node authenticator, everything is refused —
// the channel never runs open.
func TestResolveCredential_NoAuthConfiguredRefuses(t *testing.T) {
	s := NewAgentServer(nil, nil, "")
	if _, err := s.resolveCredential(context.Background(), "anything"); err == nil {
		t.Fatal("credential accepted with no auth configured")
	}
}

// fakeNodeReporter records every ReportSeen call, so a test can assert what
// handleMessage passed through without a database.
type fakeNodeReporter struct {
	seen []NodeSeen
}

func (f *fakeNodeReporter) ReportSeen(seen NodeSeen) {
	f.seen = append(f.seen, seen)
}

// A home session reports zero GPUNodes (CPU-only) — handleMessage's Inventory
// case must still thread the report's cluster_ready through to the single
// "recorded under its own identity" ReportSeen call.
func TestHandleMessage_Inventory_HomeSession_ThreadsClusterReady(t *testing.T) {
	reporter := &fakeNodeReporter{}
	s := NewAgentServer(nil, nil, "shared-secret").WithNodeReporter(reporter)
	session := NewAgentSession("home-sreek", "home", "v1", "home", func(*agentpb.ControlMessage) error { return nil })

	s.handleMessage(session, &agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Inventory{Inventory: &agentpb.GPUInventory{
			Nodes:        nil, // CPU-only: no GPU nodes to enumerate
			ObservedAt:   timestamppb.Now(),
			ClusterReady: false, // k3s unreachable
		}},
	})

	if len(reporter.seen) != 1 {
		t.Fatalf("got %d ReportSeen calls, want 1", len(reporter.seen))
	}
	if reporter.seen[0].K8sReady {
		t.Error("K8sReady = true, want false (ClusterReady=false on the report)")
	}
	if reporter.seen[0].NodeName != "home-sreek" {
		t.Errorf("NodeName = %q, want the session's own provider id", reporter.seen[0].NodeName)
	}
}

// A datacenter session reports one or more GPU nodes — every per-node
// ReportSeen call must carry the SAME session-level cluster_ready value
// (all nodes share one agent process and one cluster client).
func TestHandleMessage_Inventory_DatacenterSession_ThreadsClusterReady(t *testing.T) {
	reporter := &fakeNodeReporter{}
	s := NewAgentServer(nil, nil, "shared-secret").WithNodeReporter(reporter)
	session := NewAgentSession("dc-provider", "us-east", "v1", "datacenter", func(*agentpb.ControlMessage) error { return nil })

	s.handleMessage(session, &agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Inventory{Inventory: &agentpb.GPUInventory{
			Nodes: []*agentpb.GPUNode{
				{NodeName: "gpu-node-1"},
				{NodeName: "gpu-node-2"},
			},
			ObservedAt:   timestamppb.Now(),
			ClusterReady: true,
		}},
	})

	if len(reporter.seen) != 2 {
		t.Fatalf("got %d ReportSeen calls, want 2", len(reporter.seen))
	}
	for _, seen := range reporter.seen {
		if !seen.K8sReady {
			t.Errorf("node %q: K8sReady = false, want true (ClusterReady=true on the report)", seen.NodeName)
		}
	}
}

// TestHandleMessage_Inventory_ThreadsUtilization is the regression test
// for a gap found live 2026-09-04 while auditing the newly-built node
// telemetry pipeline for solidity: CPU%/memory-GB were added to the
// wire and threaded through correctly, but nothing proved it with a
// test — exactly the kind of silent field-drop this codebase has caught
// elsewhere by NOT having this test. Also covers GPU VRAM usage
// (n.UsedVRAMGB), which was live-reported to the allocator already but
// never threaded to ReportSeen at all until this same pass, and — added
// the same day once network/storage rates joined CPU/mem/VRAM on the
// wire — network RX/TX and storage read/write MB/s.
func TestHandleMessage_Inventory_ThreadsUtilization(t *testing.T) {
	t.Run("home session (CPU-only, no GPU nodes)", func(t *testing.T) {
		reporter := &fakeNodeReporter{}
		s := NewAgentServer(nil, nil, "shared-secret").WithNodeReporter(reporter)
		session := NewAgentSession("home-sreek", "home", "v1", "home", func(*agentpb.ControlMessage) error { return nil })

		s.handleMessage(session, &agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_Inventory{Inventory: &agentpb.GPUInventory{
				Nodes:            nil,
				ObservedAt:       timestamppb.Now(),
				ClusterReady:     true,
				CpuUsedPercent:   33.5,
				MemoryUsedGb:     12.25,
				NetworkRxMbps:    5.5,
				NetworkTxMbps:    1.25,
				StorageReadMbps:  3.0,
				StorageWriteMbps: 0.75,
			}},
		})

		if len(reporter.seen) != 1 {
			t.Fatalf("got %d ReportSeen calls, want 1", len(reporter.seen))
		}
		got := reporter.seen[0]
		if got.CPUUsedPercent != 33.5 {
			t.Errorf("CPUUsedPercent = %v, want 33.5", got.CPUUsedPercent)
		}
		if got.MemoryUsedGB != 12.25 {
			t.Errorf("MemoryUsedGB = %v, want 12.25", got.MemoryUsedGB)
		}
		if got.GPUUsedVRAMGB != 0 {
			t.Errorf("GPUUsedVRAMGB = %v, want 0 (no GPU nodes reported)", got.GPUUsedVRAMGB)
		}
		if got.NetworkRxMbps != 5.5 || got.NetworkTxMbps != 1.25 {
			t.Errorf("network rates = (%v,%v), want (5.5,1.25)", got.NetworkRxMbps, got.NetworkTxMbps)
		}
		if got.StorageReadMbps != 3.0 || got.StorageWriteMbps != 0.75 {
			t.Errorf("storage rates = (%v,%v), want (3.0,0.75)", got.StorageReadMbps, got.StorageWriteMbps)
		}
	})

	t.Run("datacenter session (per-GPU-node utilization)", func(t *testing.T) {
		reporter := &fakeNodeReporter{}
		s := NewAgentServer(nil, nil, "shared-secret").WithNodeReporter(reporter)
		session := NewAgentSession("dc-provider", "us-east", "v1", "datacenter", func(*agentpb.ControlMessage) error { return nil })

		s.handleMessage(session, &agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_Inventory{Inventory: &agentpb.GPUInventory{
				Nodes: []*agentpb.GPUNode{
					{NodeName: "gpu-node-1", UsedVramGb: 18},
					{NodeName: "gpu-node-2", UsedVramGb: 40},
				},
				ObservedAt:       timestamppb.Now(),
				ClusterReady:     true,
				CpuUsedPercent:   61.0,
				MemoryUsedGb:     200.5,
				NetworkRxMbps:    12.0,
				NetworkTxMbps:    4.5,
				StorageReadMbps:  8.25,
				StorageWriteMbps: 2.0,
			}},
		})

		if len(reporter.seen) != 2 {
			t.Fatalf("got %d ReportSeen calls, want 2", len(reporter.seen))
		}
		wantVRAM := map[string]int{"gpu-node-1": 18, "gpu-node-2": 40}
		for _, seen := range reporter.seen {
			// Session-level utilization (host CPU/mem/network/storage) is
			// the SAME on every per-node report — one host, one process,
			// one set of readings.
			if seen.CPUUsedPercent != 61.0 {
				t.Errorf("node %q: CPUUsedPercent = %v, want 61.0", seen.NodeName, seen.CPUUsedPercent)
			}
			if seen.MemoryUsedGB != 200.5 {
				t.Errorf("node %q: MemoryUsedGB = %v, want 200.5", seen.NodeName, seen.MemoryUsedGB)
			}
			if seen.NetworkRxMbps != 12.0 || seen.NetworkTxMbps != 4.5 {
				t.Errorf("node %q: network rates = (%v,%v), want (12.0,4.5)", seen.NodeName, seen.NetworkRxMbps, seen.NetworkTxMbps)
			}
			if seen.StorageReadMbps != 8.25 || seen.StorageWriteMbps != 2.0 {
				t.Errorf("node %q: storage rates = (%v,%v), want (8.25,2.0)", seen.NodeName, seen.StorageReadMbps, seen.StorageWriteMbps)
			}
			// GPU VRAM usage is PER-NODE, unlike the readings above — each
			// device reports its own.
			if want := wantVRAM[seen.NodeName]; seen.GPUUsedVRAMGB != want {
				t.Errorf("node %q: GPUUsedVRAMGB = %v, want %d", seen.NodeName, seen.GPUUsedVRAMGB, want)
			}
		}
	})
}

// A nil NodeReporter (home compute / write-through disabled) means the
// Inventory case must not attempt to call it — handleMessage should simply
// skip persistence, not panic.
func TestHandleMessage_Inventory_NoReporterConfigured(t *testing.T) {
	s := NewAgentServer(nil, nil, "shared-secret") // no WithNodeReporter
	session := NewAgentSession("dc-provider", "us-east", "v1", "datacenter", func(*agentpb.ControlMessage) error { return nil })

	s.handleMessage(session, &agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Inventory{Inventory: &agentpb.GPUInventory{
			ObservedAt:   timestamppb.Now(),
			ClusterReady: true,
		}},
	})
	// No assertion beyond "did not panic" — the point is nil-safety.
}
