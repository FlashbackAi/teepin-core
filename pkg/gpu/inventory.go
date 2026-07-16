// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package gpu provides GPU discovery, allocation, and pricing for the
// TEEPIN compute service. Hardware capability is discovered at runtime
// from Kubernetes node labels published by the NVIDIA GPU Operator
// (GPU Feature Discovery), so the platform runs on any NVIDIA GPU:
// MIG-capable datacenter GPUs (A100, A30, H100, H200, B200) use
// hardware-isolated MIG slices for standard sizes, while all other
// GPUs (L4, L40S, T4, RTX 4090, ...) use shared-GPU allocation.
package gpu

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Node labels published by NVIDIA GPU Feature Discovery (GPU Operator).
const (
	LabelGPUPresent = "nvidia.com/gpu.present" // "true" on GPU nodes
	LabelGPUProduct = "nvidia.com/gpu.product" // e.g. "NVIDIA-H100-80GB-HBM3"
	LabelGPUMemory  = "nvidia.com/gpu.memory"  // memory per GPU in MiB, e.g. "81920"
	LabelGPUCount   = "nvidia.com/gpu.count"   // physical GPUs on the node
	LabelMIGCapable = "nvidia.com/mig.capable" // "true" on MIG-capable GPUs
)

// Annotations TEEPIN sets on managed pods, used for capacity accounting
// and (later) billing reconciliation.
const (
	AnnotationVRAMGB      = "teepin.io/vram-gb"
	AnnotationGPUResource = "teepin.io/gpu-resource"
)

// LabelManaged identifies TEEPIN-managed pods.
const LabelManaged = "app.teepin.cloud/managed"

// SharedGPUResource is the extended resource exposed by the NVIDIA
// device plugin for whole/shared GPU scheduling.
const SharedGPUResource = "nvidia.com/gpu"

// Label used by the local Kind cluster to mark simulated GPU nodes.
const labelSimulatedGPU = "gpu-type"

// migResourceRe matches MIG extended resource names such as
// "nvidia.com/mig-2g.20gb" and captures compute slices and memory GB.
var migResourceRe = regexp.MustCompile(`^nvidia\.com/mig-(\d+)g\.(\d+)gb$`)

// MIGResource describes one MIG profile exposed by a node.
type MIGResource struct {
	ResourceName string // "nvidia.com/mig-2g.20gb"
	Profile      string // "2g.20gb"
	Slices       int    // compute slices (the "2g" part)
	MemoryGB     int    // memory (the "20gb" part)
	Capacity     int    // allocatable count on the node
	Used         int    // count currently requested by pods
}

// Free returns the number of unallocated MIG devices for this profile.
func (m *MIGResource) Free() int {
	free := m.Capacity - m.Used
	if free < 0 {
		return 0
	}
	return free
}

// NodeGPUInfo is a point-in-time view of one GPU node's capacity.
type NodeGPUInfo struct {
	NodeName       string
	Product        string // raw product label, e.g. "NVIDIA-H100-80GB-HBM3"
	Model          string // normalized short name, e.g. "h100"
	MemoryGBPerGPU int    // VRAM of a single physical GPU
	GPUCount       int    // physical GPUs on the node
	MIGCapable     bool
	MIGResources   map[string]*MIGResource // keyed by extended resource name
	SharedCapacity int                     // allocatable "nvidia.com/gpu"
	SharedUsed     int                     // "nvidia.com/gpu" requested by pods
	UsedVRAMGB     int                     // VRAM consumed by existing pods
	Simulated      bool                    // true for local-dev simulated nodes
}

// TotalVRAMGB returns the node's total GPU memory.
func (n *NodeGPUInfo) TotalVRAMGB() int {
	return n.MemoryGBPerGPU * n.GPUCount
}

// FreeVRAMGB returns the node's unallocated GPU memory.
func (n *NodeGPUInfo) FreeVRAMGB() int {
	free := n.TotalVRAMGB() - n.UsedVRAMGB
	if free < 0 {
		return 0
	}
	return free
}

// FindMIGResource returns the node's MIG resource matching the exact
// memory size, or nil if the node does not expose one.
func (n *NodeGPUInfo) FindMIGResource(memoryGB int) *MIGResource {
	for _, mig := range n.MIGResources {
		if mig.MemoryGB == memoryGB {
			return mig
		}
	}
	return nil
}

// Inventory discovers GPU capacity from the Kubernetes cluster.
type Inventory struct {
	client    kubernetes.Interface
	simulated bool
}

// NewInventory creates a GPU inventory. When simulated is true (local
// development without real GPUs), nodes labeled "gpu-type" are treated
// as H100-class 80GB GPUs; if none exist a synthetic node is returned.
func NewInventory(client kubernetes.Interface, simulated bool) *Inventory {
	return &Inventory{client: client, simulated: simulated}
}

// Simulated reports whether the inventory runs in simulation mode.
func (inv *Inventory) Simulated() bool {
	return inv.simulated
}

// Snapshot returns the current GPU capacity of every GPU node,
// including usage by existing pods.
func (inv *Inventory) Snapshot(ctx context.Context) ([]*NodeGPUInfo, error) {
	if inv.client == nil {
		if inv.simulated {
			return []*NodeGPUInfo{syntheticH100Node()}, nil
		}
		return nil, fmt.Errorf("no kubernetes client available for GPU discovery")
	}

	nodes, err := inv.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var infos []*NodeGPUInfo
	for i := range nodes.Items {
		node := &nodes.Items[i]
		info := inv.nodeGPUInfo(node)
		if info == nil {
			continue
		}
		if err := inv.applyPodUsage(ctx, info); err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}

	if len(infos) == 0 && inv.simulated {
		// No labeled nodes in the cluster; fall back to a synthetic node
		// so the API remains testable without any cluster setup.
		infos = append(infos, syntheticH100Node())
	}

	return infos, nil
}

// nodeGPUInfo builds capacity info for a node, or returns nil when the
// node has no GPU capability TEEPIN can use.
func (inv *Inventory) nodeGPUInfo(node *corev1.Node) *NodeGPUInfo {
	labels := node.Labels

	// Local-dev simulated GPU node.
	if inv.simulated {
		if _, ok := labels[labelSimulatedGPU]; ok {
			info := syntheticH100Node()
			info.NodeName = node.Name
			return info
		}
	}

	info := &NodeGPUInfo{
		NodeName:     node.Name,
		Product:      labels[LabelGPUProduct],
		MIGResources: map[string]*MIGResource{},
	}
	info.Model = shortModelName(info.Product)

	if memMiB, err := strconv.Atoi(labels[LabelGPUMemory]); err == nil && memMiB > 0 {
		info.MemoryGBPerGPU = memMiB / 1024
	}
	if count, err := strconv.Atoi(labels[LabelGPUCount]); err == nil && count > 0 {
		info.GPUCount = count
	}
	info.MIGCapable = labels[LabelMIGCapable] == "true"

	// Extended resources exposed by the device plugin.
	for resName, qty := range node.Status.Allocatable {
		name := string(resName)
		if name == SharedGPUResource {
			info.SharedCapacity = int(qty.Value())
			continue
		}
		if m := migResourceRe.FindStringSubmatch(name); m != nil {
			slices, _ := strconv.Atoi(m[1])
			memGB, _ := strconv.Atoi(m[2])
			info.MIGResources[name] = &MIGResource{
				ResourceName: name,
				Profile:      fmt.Sprintf("%dg.%dgb", slices, memGB),
				Slices:       slices,
				MemoryGB:     memGB,
				Capacity:     int(qty.Value()),
			}
			info.MIGCapable = true
		}
	}

	// Not a GPU node.
	if labels[LabelGPUPresent] != "true" &&
		info.SharedCapacity == 0 && len(info.MIGResources) == 0 {
		return nil
	}

	// GPU count fallback when GFD labels are absent but the device
	// plugin exposes whole GPUs.
	if info.GPUCount == 0 && info.SharedCapacity > 0 {
		info.GPUCount = info.SharedCapacity
	}
	if info.GPUCount == 0 && len(info.MIGResources) > 0 {
		info.GPUCount = 1
	}

	return info
}

// applyPodUsage accounts VRAM and device usage of existing pods on the
// node. TEEPIN-managed pods carry an exact VRAM annotation; foreign
// pods are charged the full memory of each GPU/MIG device they request.
func (inv *Inventory) applyPodUsage(ctx context.Context, info *NodeGPUInfo) error {
	pods, err := inv.client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s,status.phase!=%s,status.phase!=%s",
			info.NodeName, corev1.PodSucceeded, corev1.PodFailed),
	})
	if err != nil {
		return fmt.Errorf("failed to list pods on node %s: %w", info.NodeName, err)
	}

	for i := range pods.Items {
		pod := &pods.Items[i]

		// The API server filters by field selector; keep an explicit
		// guard for clients that do not (e.g. fakes in tests).
		if pod.Spec.NodeName != info.NodeName ||
			pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		annotatedVRAM := 0
		if v, err := strconv.Atoi(pod.Annotations[AnnotationVRAMGB]); err == nil && v > 0 {
			annotatedVRAM = v
		}

		deviceVRAM := 0
		for _, container := range pod.Spec.Containers {
			for resName, qty := range container.Resources.Limits {
				name := string(resName)
				count := int(qty.Value())
				if count <= 0 {
					continue
				}
				if name == SharedGPUResource {
					info.SharedUsed += count
					deviceVRAM += count * info.MemoryGBPerGPU
				} else if mig, ok := info.MIGResources[name]; ok {
					mig.Used += count
					deviceVRAM += count * mig.MemoryGB
				}
			}
		}

		// TEEPIN pods: exact annotated VRAM. Foreign pods: full device memory.
		if annotatedVRAM > 0 {
			info.UsedVRAMGB += annotatedVRAM
		} else {
			info.UsedVRAMGB += deviceVRAM
		}
	}

	return nil
}

// syntheticH100Node returns a simulated H100 80GB node for local
// development without GPU hardware.
func syntheticH100Node() *NodeGPUInfo {
	return &NodeGPUInfo{
		NodeName:       "",
		Product:        "NVIDIA-H100-80GB-HBM3 (simulated)",
		Model:          "h100",
		MemoryGBPerGPU: 80,
		GPUCount:       1,
		MIGCapable:     true,
		MIGResources: map[string]*MIGResource{
			"nvidia.com/mig-1g.10gb": {ResourceName: "nvidia.com/mig-1g.10gb", Profile: "1g.10gb", Slices: 1, MemoryGB: 10, Capacity: 7},
			"nvidia.com/mig-2g.20gb": {ResourceName: "nvidia.com/mig-2g.20gb", Profile: "2g.20gb", Slices: 2, MemoryGB: 20, Capacity: 3},
			"nvidia.com/mig-4g.40gb": {ResourceName: "nvidia.com/mig-4g.40gb", Profile: "4g.40gb", Slices: 4, MemoryGB: 40, Capacity: 1},
			"nvidia.com/mig-7g.80gb": {ResourceName: "nvidia.com/mig-7g.80gb", Profile: "7g.80gb", Slices: 7, MemoryGB: 80, Capacity: 1},
		},
		SharedCapacity: 1,
		Simulated:      true,
	}
}

// knownModels maps product-name substrings to normalized model names.
// Ordered so that more specific names match first (e.g. H200 before H20).
var knownModels = []struct {
	substr string
	model  string
}{
	{"H200", "h200"}, {"H100", "h100"}, {"GB200", "gb200"}, {"B200", "b200"},
	{"A100", "a100"}, {"A30", "a30"}, {"A10G", "a10g"}, {"A10", "a10"},
	{"L40S", "l40s"}, {"L40", "l40"}, {"L4", "l4"}, {"T4", "t4"},
	{"V100", "v100"}, {"RTX 6000", "rtx6000"}, {"RTX A6000", "rtxa6000"},
	{"4090", "rtx4090"}, {"3090", "rtx3090"},
}

// shortModelName normalizes a GPU product label to a short model name
// used in instance type names (e.g. "gpu.h100.2g.20gb").
func shortModelName(product string) string {
	if product == "" {
		return "unknown"
	}
	upper := strings.ToUpper(strings.ReplaceAll(product, "-", " "))
	for _, km := range knownModels {
		if strings.Contains(upper, km.substr) {
			return km.model
		}
	}
	// Fallback: sanitized lowercase product name.
	s := strings.ToLower(product)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}
