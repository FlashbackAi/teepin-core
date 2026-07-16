// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package gpu

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestAllocator(objects ...runtime.Object) *Allocator {
	client := fake.NewSimpleClientset(objects...)
	return NewAllocator(NewInventory(client, false))
}

func TestParseVRAM(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"25GB", 25, false},
		{"25gb", 25, false},
		{"25 GB", 25, false},
		{"1024MB", 1, false},
		{"40000MB", 40, false}, // 39.06 GB rounds UP: never allocate less than requested
		{"80GB", 80, false},
		{"25TB", 0, true},
		{"GB", 0, true},
		{"", 0, true},
		{"25", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseVRAM(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseVRAM(%q): expected error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVRAM(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseVRAM(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestAllocate_MIGExactMatch(t *testing.T) {
	alloc := newTestAllocator(newGPUNode("gpu-1", h100Labels(), map[string]string{
		"nvidia.com/gpu":         "1",
		"nvidia.com/mig-2g.20gb": "3",
	}))

	a, err := alloc.AllocateByVRAM(context.Background(), 20)
	if err != nil {
		t.Fatalf("AllocateByVRAM failed: %v", err)
	}
	if a.AllocationType != AllocationMIG {
		t.Errorf("AllocationType = %q, want mig", a.AllocationType)
	}
	if a.ResourceName != "nvidia.com/mig-2g.20gb" {
		t.Errorf("ResourceName = %q, want nvidia.com/mig-2g.20gb", a.ResourceName)
	}
	if a.NodeName != "gpu-1" {
		t.Errorf("NodeName = %q, want gpu-1", a.NodeName)
	}
	if a.AllocatedVRAM != 20 || a.RequestedVRAM != 20 {
		t.Errorf("VRAM = %d/%d, want exact 20/20", a.AllocatedVRAM, a.RequestedVRAM)
	}
	if a.InstanceType != "gpu.h100.2g.20gb" {
		t.Errorf("InstanceType = %q, want gpu.h100.2g.20gb", a.InstanceType)
	}
}

func TestAllocate_CustomSizeRoundsUpToMIG(t *testing.T) {
	// Launch policy: 25GB has no exact MIG profile, so it lands in the
	// smallest slice that holds it (40GB), billed transparently.
	alloc := newTestAllocator(newGPUNode("gpu-1", h100Labels(), map[string]string{
		"nvidia.com/gpu":         "1",
		"nvidia.com/mig-2g.20gb": "3",
		"nvidia.com/mig-4g.40gb": "1",
	}))

	a, err := alloc.AllocateByVRAM(context.Background(), 25)
	if err != nil {
		t.Fatalf("AllocateByVRAM failed: %v", err)
	}
	if a.AllocationType != AllocationMIG {
		t.Errorf("AllocationType = %q, want mig", a.AllocationType)
	}
	if a.ResourceName != "nvidia.com/mig-4g.40gb" {
		t.Errorf("ResourceName = %q, want nvidia.com/mig-4g.40gb", a.ResourceName)
	}
	if a.RequestedVRAM != 25 || a.AllocatedVRAM != 40 {
		t.Errorf("VRAM = %d/%d, want requested 25 allocated 40", a.RequestedVRAM, a.AllocatedVRAM)
	}
	if a.InstanceType != "gpu.h100.4g.40gb" {
		t.Errorf("InstanceType = %q, want gpu.h100.4g.40gb", a.InstanceType)
	}
}

func TestAllocate_CustomSizeFallsBackToWholeGPU(t *testing.T) {
	// No MIG profile >= 25GB available: the request gets a whole
	// dedicated GPU, billed as the full 80GB.
	alloc := newTestAllocator(newGPUNode("gpu-1", h100Labels(), map[string]string{
		"nvidia.com/gpu":         "1",
		"nvidia.com/mig-2g.20gb": "3",
	}))

	a, err := alloc.AllocateByVRAM(context.Background(), 25)
	if err != nil {
		t.Fatalf("AllocateByVRAM failed: %v", err)
	}
	if a.AllocationType != AllocationShared || a.ResourceName != SharedGPUResource {
		t.Errorf("got %q/%q, want shared whole GPU", a.AllocationType, a.ResourceName)
	}
	if a.RequestedVRAM != 25 || a.AllocatedVRAM != 80 {
		t.Errorf("VRAM = %d/%d, want requested 25 allocated 80", a.RequestedVRAM, a.AllocatedVRAM)
	}
	if a.InstanceType != "gpu.h100.80gb" {
		t.Errorf("InstanceType = %q, want gpu.h100.80gb", a.InstanceType)
	}
}

func TestAllocate_NonMIGConsumerGPU(t *testing.T) {
	// An RTX 4090 has no MIG support: everything goes through the
	// shared path. This is the "runs on whatever GPU" guarantee.
	alloc := newTestAllocator(newGPUNode("gpu-1", rtx4090Labels(), map[string]string{
		"nvidia.com/gpu": "1",
	}))

	a, err := alloc.AllocateByVRAM(context.Background(), 20)
	if err != nil {
		t.Fatalf("AllocateByVRAM failed: %v", err)
	}
	if a.AllocationType != AllocationShared || a.GPUModel != "rtx4090" {
		t.Errorf("got %q on %q, want shared on rtx4090", a.AllocationType, a.GPUModel)
	}
	if a.AllocatedVRAM != 24 { // whole card is dedicated and billed
		t.Errorf("AllocatedVRAM = %d, want 24 (whole GPU)", a.AllocatedVRAM)
	}

	// 40GB does not fit on a 24GB card.
	_, err = alloc.AllocateByVRAM(context.Background(), 40)
	if err == nil || !strings.Contains(err.Error(), "largest available GPU") {
		t.Errorf("expected 'largest available GPU' error, got %v", err)
	}
}

func TestAllocate_RejectsWhenCapacityExhausted(t *testing.T) {
	node := newGPUNode("gpu-1", h100Labels(), map[string]string{
		"nvidia.com/gpu":         "1",
		"nvidia.com/mig-2g.20gb": "3",
	})
	// Existing TEEPIN workload already holds 70 of 80 GB.
	existing := newPod("busy", "gpu-1", corev1.PodRunning,
		map[string]string{AnnotationVRAMGB: "70"},
		map[string]string{"nvidia.com/gpu": "1"})

	alloc := newTestAllocator(node, existing)

	_, err := alloc.AllocateByVRAM(context.Background(), 20)
	if err == nil || !strings.Contains(err.Error(), "insufficient GPU capacity") {
		t.Errorf("expected insufficient capacity error, got %v", err)
	}
}

func TestAllocate_MIGExhaustedRoundsUpToLargerProfile(t *testing.T) {
	node := newGPUNode("gpu-1", h100Labels(), map[string]string{
		"nvidia.com/gpu":         "1",
		"nvidia.com/mig-1g.10gb": "1",
		"nvidia.com/mig-2g.20gb": "3",
	})
	// The only 10GB MIG device is taken by a foreign pod: a 10GB
	// request must land in the next profile up (20GB).
	existing := newPod("other", "gpu-1", corev1.PodRunning,
		nil, map[string]string{"nvidia.com/mig-1g.10gb": "1"})

	alloc := newTestAllocator(node, existing)

	a, err := alloc.AllocateByVRAM(context.Background(), 10)
	if err != nil {
		t.Fatalf("AllocateByVRAM failed: %v", err)
	}
	if a.AllocationType != AllocationMIG || a.ResourceName != "nvidia.com/mig-2g.20gb" {
		t.Errorf("got %q/%q, want mig round-up to 20GB", a.AllocationType, a.ResourceName)
	}
	if a.RequestedVRAM != 10 || a.AllocatedVRAM != 20 {
		t.Errorf("VRAM = %d/%d, want requested 10 allocated 20", a.RequestedVRAM, a.AllocatedVRAM)
	}
}

func TestAllocate_BinPacking(t *testing.T) {
	migResources := map[string]string{
		"nvidia.com/gpu":         "1",
		"nvidia.com/mig-2g.20gb": "3",
	}
	nodeA := newGPUNode("gpu-a", h100Labels(), migResources)
	nodeB := newGPUNode("gpu-b", h100Labels(), migResources)
	// gpu-a already has 40GB in use → 40GB free; gpu-b is empty (80GB free).
	existing := newPod("busy", "gpu-a", corev1.PodRunning,
		map[string]string{AnnotationVRAMGB: "40"}, nil)

	alloc := newTestAllocator(nodeA, nodeB, existing)

	// A 20GB slice fits on both nodes: bin-packing must prefer the
	// fuller node (gpu-a) to keep contiguous capacity on gpu-b.
	a, err := alloc.AllocateByVRAM(context.Background(), 20)
	if err != nil {
		t.Fatalf("AllocateByVRAM failed: %v", err)
	}
	if a.NodeName != "gpu-a" {
		t.Errorf("NodeName = %q, want gpu-a (least free VRAM that fits)", a.NodeName)
	}

	// 60GB fits no MIG profile → whole GPU. gpu-a (40GB free) cannot
	// dedicate a full GPU; gpu-b can.
	b, err := alloc.AllocateByVRAM(context.Background(), 60)
	if err != nil {
		t.Fatalf("AllocateByVRAM failed: %v", err)
	}
	if b.NodeName != "gpu-b" || b.AllocatedVRAM != 80 {
		t.Errorf("got node %q allocated %d, want gpu-b with whole 80GB GPU", b.NodeName, b.AllocatedVRAM)
	}
}

func TestAllocate_NoGPUNodes(t *testing.T) {
	alloc := newTestAllocator(
		newGPUNode("cpu-only", map[string]string{"workload-type": "cpu"}, nil))

	_, err := alloc.AllocateByVRAM(context.Background(), 10)
	if err == nil {
		t.Fatal("expected error when cluster has no GPU nodes")
	}
}

func TestAllocate_InvalidRequests(t *testing.T) {
	alloc := newTestAllocator(newGPUNode("gpu-1", h100Labels(),
		map[string]string{"nvidia.com/gpu": "1"}))

	for _, vram := range []int{0, -5} {
		if _, err := alloc.AllocateByVRAM(context.Background(), vram); err == nil {
			t.Errorf("AllocateByVRAM(%d): expected error", vram)
		}
	}
}

func TestAllocate_SimulatedMode(t *testing.T) {
	client := fake.NewSimpleClientset(newGPUNode("teepin-local-worker2",
		map[string]string{"gpu-type": "h100-simulated"}, nil))
	alloc := NewAllocator(NewInventory(client, true))

	a, err := alloc.AllocateByVRAM(context.Background(), 25)
	if err != nil {
		t.Fatalf("AllocateByVRAM failed: %v", err)
	}
	if a.AllocationType != AllocationSimulated || !a.Simulated {
		t.Errorf("got type %q simulated=%v, want simulated", a.AllocationType, a.Simulated)
	}
	if a.NodeName != "teepin-local-worker2" {
		t.Errorf("NodeName = %q, want teepin-local-worker2", a.NodeName)
	}
	// Simulated H100 exposes a 40GB profile: 25GB rounds up to it.
	if a.AllocatedVRAM != 40 {
		t.Errorf("AllocatedVRAM = %d, want 40 (round-up policy)", a.AllocatedVRAM)
	}
}

func TestAvailableInstanceTypes(t *testing.T) {
	alloc := newTestAllocator(newGPUNode("gpu-1", h100Labels(), map[string]string{
		"nvidia.com/gpu":         "1",
		"nvidia.com/mig-1g.10gb": "7",
		"nvidia.com/mig-2g.20gb": "3",
	}))

	types, err := alloc.AvailableInstanceTypes(context.Background())
	if err != nil {
		t.Fatalf("AvailableInstanceTypes failed: %v", err)
	}

	byName := map[string]InstanceTypeInfo{}
	for _, ty := range types {
		byName[ty.Name] = ty
	}

	mig20, ok := byName["gpu.h100.2g.20gb"]
	if !ok {
		t.Fatalf("missing gpu.h100.2g.20gb in %v", types)
	}
	if mig20.PricePerHour != 2.00 { // linear: 20GB * $0.10
		t.Errorf("20GB price = %.2f, want 2.00", mig20.PricePerHour)
	}
	if _, ok := byName["gpu.h100.80gb"]; !ok {
		t.Errorf("missing full-GPU type gpu.h100.80gb in %v", types)
	}
}

func TestGetPriceForVRAM(t *testing.T) {
	cases := map[int]float64{10: 1.00, 20: 2.00, 25: 2.50, 80: 8.00}
	for vram, want := range cases {
		if got := GetPriceForVRAM(vram); got != want {
			t.Errorf("GetPriceForVRAM(%d) = %.2f, want %.2f", vram, got, want)
		}
	}
}
