// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package gpu

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// --- test helpers shared by inventory_test.go and allocator_test.go ---

func newGPUNode(name string, labels map[string]string, allocatable map[string]string) *corev1.Node {
	alloc := corev1.ResourceList{}
	for k, v := range allocatable {
		alloc[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status:     corev1.NodeStatus{Allocatable: alloc},
	}
}

func h100Labels() map[string]string {
	return map[string]string{
		LabelGPUPresent: "true",
		LabelGPUProduct: "NVIDIA-H100-80GB-HBM3",
		LabelGPUMemory:  "81920", // MiB
		LabelGPUCount:   "1",
		LabelMIGCapable: "true",
	}
}

func rtx4090Labels() map[string]string {
	return map[string]string{
		LabelGPUPresent: "true",
		LabelGPUProduct: "NVIDIA-GeForce-RTX-4090",
		LabelGPUMemory:  "24576", // MiB
		LabelGPUCount:   "1",
	}
}

func newPod(name, node string, phase corev1.PodPhase, annotations, limits map[string]string) *corev1.Pod {
	lims := corev1.ResourceList{}
	for k, v := range limits {
		lims[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Annotations: annotations,
			Labels:      map[string]string{LabelManaged: "true"},
		},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name:      "app",
				Resources: corev1.ResourceRequirements{Limits: lims},
			}},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// --- tests ---

func TestSnapshot_DiscoversGPUNodeFromLabels(t *testing.T) {
	node := newGPUNode("gpu-node-1", h100Labels(), map[string]string{
		"nvidia.com/gpu":         "1",
		"nvidia.com/mig-1g.10gb": "7",
		"nvidia.com/mig-2g.20gb": "3",
	})
	client := fake.NewSimpleClientset(node)

	infos, err := NewInventory(client, false).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("expected 1 GPU node, got %d", len(infos))
	}

	info := infos[0]
	if info.NodeName != "gpu-node-1" {
		t.Errorf("NodeName = %q, want gpu-node-1", info.NodeName)
	}
	if info.Model != "h100" {
		t.Errorf("Model = %q, want h100", info.Model)
	}
	if info.MemoryGBPerGPU != 80 {
		t.Errorf("MemoryGBPerGPU = %d, want 80", info.MemoryGBPerGPU)
	}
	if !info.MIGCapable {
		t.Error("MIGCapable = false, want true")
	}
	if info.SharedCapacity != 1 {
		t.Errorf("SharedCapacity = %d, want 1", info.SharedCapacity)
	}
	mig := info.FindMIGResource(20)
	if mig == nil {
		t.Fatal("expected a 20GB MIG resource")
	}
	if mig.ResourceName != "nvidia.com/mig-2g.20gb" || mig.Capacity != 3 {
		t.Errorf("MIG resource = %+v, want nvidia.com/mig-2g.20gb capacity 3", mig)
	}
	if info.FreeVRAMGB() != 80 {
		t.Errorf("FreeVRAMGB = %d, want 80", info.FreeVRAMGB())
	}
}

func TestSnapshot_IgnoresNonGPUNodes(t *testing.T) {
	client := fake.NewSimpleClientset(
		newGPUNode("cpu-node", map[string]string{"workload-type": "cpu"}, nil),
	)

	infos, err := NewInventory(client, false).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("expected 0 GPU nodes, got %d", len(infos))
	}
}

func TestSnapshot_AccountsPodUsage(t *testing.T) {
	node := newGPUNode("gpu-node-1", h100Labels(), map[string]string{
		"nvidia.com/gpu":         "1",
		"nvidia.com/mig-1g.10gb": "7",
	})
	// TEEPIN pod with exact VRAM annotation (25GB shared).
	teepinPod := newPod("teepin-inst", "gpu-node-1", corev1.PodRunning,
		map[string]string{AnnotationVRAMGB: "25"},
		map[string]string{"nvidia.com/gpu": "1"})
	// Foreign pod holding a 10GB MIG device (no annotation → full device charged).
	foreignPod := newPod("other", "gpu-node-1", corev1.PodRunning,
		nil, map[string]string{"nvidia.com/mig-1g.10gb": "1"})
	// Completed pod must not count.
	donePod := newPod("done", "gpu-node-1", corev1.PodSucceeded,
		map[string]string{AnnotationVRAMGB: "40"}, nil)

	client := fake.NewSimpleClientset(node, teepinPod, foreignPod, donePod)

	infos, err := NewInventory(client, false).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	info := infos[0]

	if info.UsedVRAMGB != 35 { // 25 (annotated) + 10 (foreign MIG)
		t.Errorf("UsedVRAMGB = %d, want 35", info.UsedVRAMGB)
	}
	if info.FreeVRAMGB() != 45 {
		t.Errorf("FreeVRAMGB = %d, want 45", info.FreeVRAMGB())
	}
	if info.SharedUsed != 1 {
		t.Errorf("SharedUsed = %d, want 1", info.SharedUsed)
	}
	if mig := info.FindMIGResource(10); mig.Used != 1 || mig.Free() != 6 {
		t.Errorf("MIG 10GB used/free = %d/%d, want 1/6", mig.Used, mig.Free())
	}
}

func TestSnapshot_SimulatedWithoutClient(t *testing.T) {
	infos, err := NewInventory(nil, true).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	if len(infos) != 1 || !infos[0].Simulated || infos[0].Model != "h100" {
		t.Fatalf("expected one simulated h100 node, got %+v", infos)
	}
}

func TestSnapshot_SimulatedUsesLabeledNodes(t *testing.T) {
	node := newGPUNode("teepin-local-worker2",
		map[string]string{"gpu-type": "h100-simulated"}, nil)
	client := fake.NewSimpleClientset(node)

	infos, err := NewInventory(client, true).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("expected 1 node, got %d", len(infos))
	}
	if infos[0].NodeName != "teepin-local-worker2" || !infos[0].Simulated {
		t.Errorf("got %+v, want simulated node teepin-local-worker2", infos[0])
	}
}

func TestSnapshot_NoClientNoSimulationFails(t *testing.T) {
	_, err := NewInventory(nil, false).Snapshot(context.Background())
	if err == nil {
		t.Fatal("expected error without client and without simulation")
	}
}

func TestShortModelName(t *testing.T) {
	cases := map[string]string{
		"NVIDIA-H100-80GB-HBM3":   "h100",
		"NVIDIA-A100-SXM4-80GB":   "a100",
		"NVIDIA-GeForce-RTX-4090": "rtx4090",
		"NVIDIA-L40S":             "l40s",
		"Tesla-T4":                "t4",
		"NVIDIA-H200":             "h200",
		"":                        "unknown",
		"Some Future GPU":         "some-future-gpu",
	}
	for product, want := range cases {
		if got := shortModelName(product); got != want {
			t.Errorf("shortModelName(%q) = %q, want %q", product, got, want)
		}
	}
}
