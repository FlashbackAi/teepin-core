// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// TestSnapshotter_ServesAgentReportedInventory pins the bug that made
// the first real agent deployment fail: the allocator held a
// Kubernetes-backed inventory, so on AWS — where there is no Kubernetes
// client — every allocation failed with "no kubernetes client available
// for GPU discovery" before the request ever reached the agent.
//
// Capacity must come from whatever the cluster client can reach.
func TestSnapshotter_ServesAgentReportedInventory(t *testing.T) {
	session := NewAgentSession("p1", "us-east", "test", "", func(*agentpb.ControlMessage) error { return nil })
	session.setInventory([]NodeInventory{{
		NodeName:       "gpu-node-1",
		GPUProduct:     "NVIDIA-A6000",
		GPUModel:       "a6000",
		MemoryGBPerGPU: 48,
		GPUCount:       1,
		SharedCapacity: 1,
		Ready:          true,
		MIGResources: []MIGResource{{
			ResourceName: "nvidia.com/mig-1g.10gb",
			Profile:      "1g.10gb",
			MemoryGB:     10,
			Capacity:     4,
		}},
	}})

	client := NewAgentClient(registryWith(session))
	s := NewSnapshotter(client)

	nodes, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot through an agent must work without any Kubernetes client: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}

	node := nodes[0]
	if node.NodeName != "gpu-node-1" || node.MemoryGBPerGPU != 48 {
		t.Errorf("node data lost in translation: %+v", node)
	}
	// MIGResources is a map keyed by resource name on the gpu side; a
	// mistranslation here would leave the allocator unable to find any
	// MIG profile and silently fall back to whole-GPU placement.
	if _, ok := node.MIGResources["nvidia.com/mig-1g.10gb"]; !ok {
		t.Errorf("MIG resources not keyed by resource name: %+v", node.MIGResources)
	}
}

func TestSnapshotter_DropsUnreadyNodes(t *testing.T) {
	session := NewAgentSession("p1", "us-east", "test", "", func(*agentpb.ControlMessage) error { return nil })
	session.setInventory([]NodeInventory{
		{NodeName: "healthy", GPUCount: 1, Ready: true},
		{NodeName: "cordoned", GPUCount: 1, Ready: false},
	})

	s := NewSnapshotter(NewAgentClient(registryWith(session)))

	nodes, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Capacity on an unschedulable node is not capacity: placing there
	// produces a pod that never runs but is still billed.
	if len(nodes) != 1 || nodes[0].NodeName != "healthy" {
		t.Errorf("unready node was offered as capacity: %+v", nodes)
	}
}

// TestSnapshotter_UnreachableIsAnErrorNotEmpty distinguishes "the
// platform is full" from "capacity cannot be reached".
//
// An empty inventory makes the allocator tell the customer there is no
// GPU available, which is a different — and wrong — problem when the
// truth is that the agent is briefly disconnected.
func TestSnapshotter_UnreachableIsAnErrorNotEmpty(t *testing.T) {
	s := NewSnapshotter(NewAgentClient(NewRegistry())) // no agents

	nodes, err := s.Snapshot(context.Background())

	if err == nil {
		t.Fatalf("unreachable capacity returned %d nodes and no error", len(nodes))
	}
	if !errors.Is(err, ErrClusterUnavailable) {
		t.Errorf("error = %v, want ErrClusterUnavailable", err)
	}
}

func TestSnapshotter_StaleInventoryIsNotOffered(t *testing.T) {
	session := NewAgentSession("p1", "us-east", "test", "", func(*agentpb.ControlMessage) error { return nil })
	session.setInventory([]NodeInventory{{NodeName: "gpu-1", GPUCount: 1, Ready: true}})

	session.mu.Lock()
	session.inventoryAt = time.Now().Add(-10 * time.Minute)
	session.mu.Unlock()

	s := NewSnapshotter(NewAgentClient(registryWith(session)))

	nodes, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("stale capacity was offered for placement: %+v", nodes)
	}
}
