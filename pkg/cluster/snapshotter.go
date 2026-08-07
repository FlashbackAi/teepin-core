// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"

	"github.com/FlashbackAi/teepin-core/pkg/gpu"
)

// Snapshotter adapts a cluster.Client to the inventory source the GPU
// allocator reasons over.
//
// This exists because allocation is a control-plane decision made where
// there may be no Kubernetes API to ask. In direct mode the allocator
// could query the cluster itself; in agent mode the only capacity data
// is what agents have reported over their streams. Routing both through
// the cluster client means the allocation policy sees the same shape
// either way and does not know which deployment it is running in.
type Snapshotter struct {
	client Client
}

// Compile-time check against the interface the allocator depends on.
var _ gpu.Snapshotter = (*Snapshotter)(nil)

func NewSnapshotter(client Client) *Snapshotter {
	return &Snapshotter{client: client}
}

// Snapshot returns current GPU capacity.
//
// Errors propagate rather than becoming an empty result: an empty
// inventory means "no capacity exists" and the allocator would reject
// the request with a message about the platform being full, when the
// truth is that capacity is merely unreachable. Those are different
// problems and the customer deserves the accurate one.
func (s *Snapshotter) Snapshot(ctx context.Context) ([]*gpu.NodeGPUInfo, error) {
	nodes, err := s.client.Inventory(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]*gpu.NodeGPUInfo, 0, len(nodes))
	for _, n := range nodes {
		// Unready nodes are dropped here rather than by the allocator:
		// capacity on a cordoned or NotReady node is not capacity, and
		// placing work there produces a pod that never schedules.
		if !n.Ready {
			continue
		}

		info := &gpu.NodeGPUInfo{
			NodeName:       n.NodeName,
			Product:        n.GPUProduct,
			Model:          n.GPUModel,
			MemoryGBPerGPU: n.MemoryGBPerGPU,
			GPUCount:       n.GPUCount,
			MIGCapable:     n.MIGCapable,
			SharedCapacity: n.SharedCapacity,
			SharedUsed:     n.SharedUsed,
			UsedVRAMGB:     n.UsedVRAMGB,
			MIGResources:   map[string]*gpu.MIGResource{},
		}

		for _, m := range n.MIGResources {
			info.MIGResources[m.ResourceName] = &gpu.MIGResource{
				ResourceName: m.ResourceName,
				Profile:      m.Profile,
				Slices:       m.Slices,
				MemoryGB:     m.MemoryGB,
				Capacity:     m.Capacity,
				Used:         m.Used,
			}
		}

		out = append(out, info)
	}

	return out, nil
}
