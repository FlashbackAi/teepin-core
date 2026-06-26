// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package gpu

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"k8s.io/client-go/kubernetes"
)

// Allocator handles GPU resource allocation
type Allocator struct {
	k8sClient *kubernetes.Clientset
	migProfiles map[int]MIGProfile
}

// MIGProfile represents a NVIDIA MIG configuration
type MIGProfile struct {
	Name      string  // e.g., "2g.20gb"
	MemoryGB  int     // 20
	GPUSlices int     // Number of GPU slices
	Price     float64 // Price per hour
}

// Allocation represents an allocated GPU resource
type Allocation struct {
	MIGProfile    string
	AllocatedVRAM int    // GB
	RequestedVRAM int    // GB
	DeviceID      string
	NodeName      string
}

// NewAllocator creates a new GPU allocator
func NewAllocator(k8sClient *kubernetes.Clientset) *Allocator {
	return &Allocator{
		k8sClient: k8sClient,
		migProfiles: map[int]MIGProfile{
			10: {Name: "1g.10gb", MemoryGB: 10, GPUSlices: 1, Price: 2.00},
			20: {Name: "2g.20gb", MemoryGB: 20, GPUSlices: 2, Price: 4.00},
			40: {Name: "4g.40gb", MemoryGB: 40, GPUSlices: 4, Price: 6.00},
			80: {Name: "7g.80gb", MemoryGB: 80, GPUSlices: 7, Price: 8.00},
		},
	}
}

// AllocateByVRAM allocates GPU based on VRAM requirement (EXACT allocation)
func (a *Allocator) AllocateByVRAM(ctx context.Context, requestedVRAM int) (*Allocation, error) {
	if requestedVRAM <= 0 {
		return nil, fmt.Errorf("invalid VRAM request: %d GB", requestedVRAM)
	}

	if requestedVRAM > 80 {
		return nil, fmt.Errorf("requested VRAM (%d GB) exceeds H100 capacity (80GB)", requestedVRAM)
	}

	// Check if request matches a standard MIG profile
	useMIG := a.isStandardMIGSize(requestedVRAM)

	var deviceID, allocationType string
	var profile MIGProfile

	if useMIG {
		// Use MIG for standard sizes (better hardware isolation)
		profile = a.migProfiles[requestedVRAM]
		deviceID = fmt.Sprintf("nvidia.com/mig-%s", profile.Name)
		allocationType = "MIG"
	} else {
		// Use time-slicing for custom sizes (exact VRAM allocation)
		deviceID = "nvidia.com/gpu"
		allocationType = "time-slicing"
	}

	// In production, this would:
	// 1. Query available GPU capacity
	// 2. For MIG: Create MIG slice or allocate existing
	// 3. For time-slicing: Configure VRAM limit
	// 4. Return actual device ID

	return &Allocation{
		MIGProfile:     allocationType,
		AllocatedVRAM:  requestedVRAM,  // EXACT allocation, no over-provisioning
		RequestedVRAM:  requestedVRAM,
		DeviceID:       deviceID,
		NodeName:       "teepin-local-worker2", // Simulated GPU node
	}, nil
}

// isStandardMIGSize checks if VRAM matches a standard MIG profile
func (a *Allocator) isStandardMIGSize(vramGB int) bool {
	standardSizes := []int{10, 20, 40, 80}
	for _, size := range standardSizes {
		if vramGB == size {
			return true
		}
	}
	return false
}

// ParseVRAM parses VRAM string (e.g., "25GB", "40000MB") to GB
func ParseVRAM(vramStr string) (int, error) {
	re := regexp.MustCompile(`^(\d+)\s*(GB|MB|gb|mb)$`)
	matches := re.FindStringSubmatch(vramStr)

	if len(matches) != 3 {
		return 0, fmt.Errorf("invalid VRAM format: %s (expected format: 25GB or 40000MB)", vramStr)
	}

	value, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, fmt.Errorf("invalid VRAM value: %s", matches[1])
	}

	unit := matches[2]
	if unit == "MB" || unit == "mb" {
		// Convert MB to GB
		value = value / 1024
	}

	return value, nil
}

// GetInstanceTypeFromVRAM returns instance type name based on VRAM
func (a *Allocator) GetInstanceTypeFromVRAM(vramGB int) string {
	if a.isStandardMIGSize(vramGB) {
		profile := a.migProfiles[vramGB]
		return fmt.Sprintf("gpu.h100.%s", profile.Name)
	}
	return fmt.Sprintf("gpu.h100.custom-%dgb", vramGB)
}

// GetPriceForVRAM returns hourly price for VRAM allocation
// Industry standard: $0.10 per GB-hour (exact allocation)
func (a *Allocator) GetPriceForVRAM(vramGB int) float64 {
	pricePerGB := 0.10
	return float64(vramGB) * pricePerGB
}

// GetAvailableProfiles returns all available MIG profiles
func (a *Allocator) GetAvailableProfiles() []MIGProfile {
	profiles := make([]MIGProfile, 0, len(a.migProfiles))
	for _, profile := range a.migProfiles {
		profiles = append(profiles, profile)
	}
	return profiles
}
