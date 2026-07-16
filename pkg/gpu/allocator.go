// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package gpu

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
)

// PricePerGBHour is the platform-wide GPU price: linear, exact-allocation
// pricing at $0.10 per GB of VRAM per hour. This is the single source of
// truth for GPU pricing.
const PricePerGBHour = 0.10

// Allocation types.
const (
	AllocationMIG       = "mig"       // hardware-isolated MIG slice
	AllocationShared    = "shared"    // shared GPU with VRAM accounting
	AllocationSimulated = "simulated" // local development without GPUs
)

// Allocator selects GPU capacity for instance requests based on the
// live cluster inventory. It is hardware-agnostic: MIG slices are used
// when a node exposes a profile matching the requested size exactly,
// otherwise the workload is placed on a shared GPU with capacity
// accounting.
type Allocator struct {
	inventory *Inventory
}

// Allocation is the result of a successful GPU allocation.
//
// AllocatedVRAM >= RequestedVRAM: standard sizes match a MIG profile
// exactly; custom sizes are rounded UP to the smallest device that can
// hold them (MIG slice or whole GPU) and the customer is billed for
// the allocated size, shown transparently before deployment. Exact
// custom-size allocation returns with software VRAM partitioning
// (HAMi milestone).
type Allocation struct {
	NodeName       string // node the pod must be scheduled on ("" = any, simulation only)
	GPUModel       string // normalized model, e.g. "h100"
	AllocationType string // AllocationMIG, AllocationShared, or AllocationSimulated
	ResourceName   string // extended resource to request, e.g. "nvidia.com/mig-2g.20gb"
	Quantity       int    // quantity of ResourceName to request
	AllocatedVRAM  int    // GB actually reserved and billed
	RequestedVRAM  int    // GB the customer asked for
	InstanceType   string // e.g. "gpu.h100.2g.20gb" or "gpu.h100.80gb"
	Simulated      bool   // true when running against simulated capacity
}

// NewAllocator creates a GPU allocator backed by the given inventory.
func NewAllocator(inventory *Inventory) *Allocator {
	return &Allocator{inventory: inventory}
}

// AllocateByVRAM allocates GPU capacity for the requested VRAM.
// Preference order (launch policy — MIG is the only isolation
// mechanism until the HAMi milestone):
//  1. A node exposing a MIG profile whose memory matches exactly
//     (hardware isolation, no waste).
//  2. The smallest available MIG profile that can hold the request
//     (rounded up; the customer is billed for the slice, shown
//     transparently in the response).
//  3. A whole dedicated GPU (non-MIG hardware or no fitting profile);
//     billed as the full GPU.
//
// Among eligible nodes the one with the least free VRAM is chosen
// (bin-packing: keep large contiguous capacity available).
func (a *Allocator) AllocateByVRAM(ctx context.Context, requestedVRAM int) (*Allocation, error) {
	if requestedVRAM <= 0 {
		return nil, fmt.Errorf("invalid VRAM request: %d GB", requestedVRAM)
	}

	nodes, err := a.inventory.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("GPU discovery failed: %w", err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no GPU nodes available in the cluster")
	}

	// Sort by free VRAM ascending for bin-packing.
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].FreeVRAMGB() < nodes[j].FreeVRAMGB()
	})

	// Pass 1: exact MIG profile match.
	for _, node := range nodes {
		mig := node.FindMIGResource(requestedVRAM)
		if mig == nil || mig.Free() <= 0 || node.FreeVRAMGB() < requestedVRAM {
			continue
		}
		return a.newAllocation(node, AllocationMIG, mig.ResourceName, requestedVRAM, mig.MemoryGB), nil
	}

	// Pass 2: round up to the smallest MIG profile that fits.
	var bestNode *NodeGPUInfo
	var bestMIG *MIGResource
	for _, node := range nodes {
		for _, mig := range node.MIGResources {
			if mig.MemoryGB < requestedVRAM || mig.Free() <= 0 || node.FreeVRAMGB() < mig.MemoryGB {
				continue
			}
			if bestMIG == nil || mig.MemoryGB < bestMIG.MemoryGB {
				bestNode, bestMIG = node, mig
			}
		}
	}
	if bestMIG != nil {
		return a.newAllocation(bestNode, AllocationMIG, bestMIG.ResourceName, requestedVRAM, bestMIG.MemoryGB), nil
	}

	// Pass 3: whole dedicated GPU.
	largestGPU := 0
	for _, node := range nodes {
		if node.MemoryGBPerGPU > largestGPU {
			largestGPU = node.MemoryGBPerGPU
		}
		if node.MemoryGBPerGPU < requestedVRAM {
			continue
		}
		if node.SharedUsed >= node.SharedCapacity || node.FreeVRAMGB() < node.MemoryGBPerGPU {
			continue
		}
		return a.newAllocation(node, AllocationShared, SharedGPUResource, requestedVRAM, node.MemoryGBPerGPU), nil
	}

	if requestedVRAM > largestGPU {
		return nil, fmt.Errorf(
			"requested VRAM (%d GB) exceeds the largest available GPU (%d GB)",
			requestedVRAM, largestGPU)
	}
	return nil, fmt.Errorf(
		"insufficient GPU capacity: no node can hold %d GB right now", requestedVRAM)
}

func (a *Allocator) newAllocation(node *NodeGPUInfo, allocType, resourceName string, requestedVRAM, allocatedVRAM int) *Allocation {
	var instanceType string
	switch {
	case allocType == AllocationMIG:
		if mig, ok := node.MIGResources[resourceName]; ok {
			instanceType = fmt.Sprintf("gpu.%s.%s", node.Model, mig.Profile)
		}
	default:
		// Whole dedicated GPU.
		instanceType = fmt.Sprintf("gpu.%s.%dgb", node.Model, allocatedVRAM)
	}

	if node.Simulated {
		allocType = AllocationSimulated
	}

	return &Allocation{
		NodeName:       node.NodeName,
		GPUModel:       node.Model,
		AllocationType: allocType,
		ResourceName:   resourceName,
		Quantity:       1,
		AllocatedVRAM:  allocatedVRAM,
		RequestedVRAM:  requestedVRAM,
		InstanceType:   instanceType,
		Simulated:      node.Simulated,
	}
}

// InstanceTypeInfo describes an offerable GPU configuration, derived
// from the live inventory.
type InstanceTypeInfo struct {
	Name         string  // "gpu.h100.2g.20gb"
	GPUModel     string  // "h100"
	MemoryGB     int     // VRAM
	Isolation    string  // "mig" or "shared"
	PricePerHour float64 // linear: MemoryGB * PricePerGBHour
}

// AvailableInstanceTypes derives the offerable instance types from the
// cluster's current GPU inventory: every MIG profile exposed by a node,
// plus a full-GPU type per model. Custom sizes are always available via
// gpu_vram and are not enumerated.
func (a *Allocator) AvailableInstanceTypes(ctx context.Context) ([]InstanceTypeInfo, error) {
	nodes, err := a.inventory.Snapshot(ctx)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var types []InstanceTypeInfo

	for _, node := range nodes {
		for _, mig := range node.MIGResources {
			name := fmt.Sprintf("gpu.%s.%s", node.Model, mig.Profile)
			if seen[name] {
				continue
			}
			seen[name] = true
			types = append(types, InstanceTypeInfo{
				Name:         name,
				GPUModel:     node.Model,
				MemoryGB:     mig.MemoryGB,
				Isolation:    AllocationMIG,
				PricePerHour: GetPriceForVRAM(mig.MemoryGB),
			})
		}

		if node.SharedCapacity > 0 && node.MemoryGBPerGPU > 0 {
			name := fmt.Sprintf("gpu.%s.%dgb", node.Model, node.MemoryGBPerGPU)
			if !seen[name] {
				seen[name] = true
				types = append(types, InstanceTypeInfo{
					Name:         name,
					GPUModel:     node.Model,
					MemoryGB:     node.MemoryGBPerGPU,
					Isolation:    AllocationShared,
					PricePerHour: GetPriceForVRAM(node.MemoryGBPerGPU),
				})
			}
		}
	}

	sort.Slice(types, func(i, j int) bool {
		if types[i].GPUModel != types[j].GPUModel {
			return types[i].GPUModel < types[j].GPUModel
		}
		return types[i].MemoryGB < types[j].MemoryGB
	})

	return types, nil
}

// GetPriceForVRAM returns the hourly price for a VRAM allocation.
// Pricing is linear and exact: $0.10 per GB-hour.
func GetPriceForVRAM(vramGB int) float64 {
	return float64(vramGB) * PricePerGBHour
}

// vramRe matches VRAM strings such as "25GB", "25 GB", or "40000MB".
var vramRe = regexp.MustCompile(`^(\d+)\s*([GgMm][Bb])$`)

// ParseVRAM parses a VRAM string (e.g. "25GB", "40000MB") to whole GB.
// Megabyte values are rounded up to the next GB, since allocation and
// billing are in whole GB.
func ParseVRAM(vramStr string) (int, error) {
	matches := vramRe.FindStringSubmatch(vramStr)
	if len(matches) != 3 {
		return 0, fmt.Errorf("invalid VRAM format: %q (expected format: 25GB or 40000MB)", vramStr)
	}

	value, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, fmt.Errorf("invalid VRAM value: %s", matches[1])
	}

	if matches[2] == "MB" || matches[2] == "mb" || matches[2] == "Mb" || matches[2] == "mB" {
		// Round up: customers must not receive less than they requested.
		value = (value + 1023) / 1024
	}

	return value, nil
}
