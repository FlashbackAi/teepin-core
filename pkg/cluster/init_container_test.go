// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestCreateInstance_InitContainerRequiresStorage proves buildPod refuses
// an InitContainer with no StorageGB rather than silently producing a pod
// spec that would fail to apply (an init container writing to /data with
// no /data volume to mount).
func TestCreateInstance_InitContainerRequiresStorage(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-init00001",
		Image:      "vllm/vllm-openai:latest",
		CPUUnits:   4,
		MemoryGB:   16,
		InitContainer: &InitContainerSpec{
			Image:   "curlimages/curl",
			Command: []string{"sh", "-c", "curl -L $URL -o /data/model"},
			Env:     map[string]string{"URL": "https://example.com/model.bin"},
		},
	})
	if err == nil {
		t.Fatal("expected an error for InitContainer with StorageGB == 0")
	}
}

// TestCreateInstance_InitContainerAddedToPod proves a valid InitContainer
// (StorageGB > 0) becomes a real Kubernetes init container sharing the
// same /data volume as the main container — the download-then-serve
// pattern Teepin Inference's model-mount reconciler depends on.
func TestCreateInstance_InitContainerAddedToPod(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-init00002",
		Image:      "vllm/vllm-openai:latest",
		CPUUnits:   4,
		MemoryGB:   16,
		StorageGB:  100,
		InitContainer: &InitContainerSpec{
			Image:   "curlimages/curl",
			Command: []string{"sh", "-c", "curl -L $URL -o /data/model"},
			Env:     map[string]string{"URL": "https://example.com/model.bin"},
		},
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(context.Background(), metav1.ListOptions{})
	pod := pods.Items[0]

	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("got %d init containers, want 1", len(pod.Spec.InitContainers))
	}
	init := pod.Spec.InitContainers[0]
	if init.Image != "curlimages/curl" {
		t.Errorf("init image = %q, want curlimages/curl", init.Image)
	}
	if len(init.VolumeMounts) != 1 || init.VolumeMounts[0].MountPath != "/data" {
		t.Errorf("init container does not mount /data — the same volume the main container writes to: %+v", init.VolumeMounts)
	}
	if len(init.Env) != 1 || init.Env[0].Name != "URL" {
		t.Errorf("init container env not carried through: %+v", init.Env)
	}

	// The main container must mount the SAME volume the init container
	// populated — otherwise the download would be invisible to the
	// engine that's supposed to serve it.
	if len(pod.Spec.Containers[0].VolumeMounts) != 1 || pod.Spec.Containers[0].VolumeMounts[0].MountPath != "/data" {
		t.Error("main container does not mount /data alongside the init container")
	}
}

// TestCreateInstance_NoInitContainerMeansNone guards the other direction:
// every existing caller (nil InitContainer) must see no behaviour change.
func TestCreateInstance_NoInitContainerMeansNone(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-init00003",
		Image:      "nginx:latest",
		CPUUnits:   2,
		MemoryGB:   4,
		StorageGB:  10,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(context.Background(), metav1.ListOptions{})
	pod := pods.Items[0]

	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("got %d init containers with InitContainer unset, want 0", len(pod.Spec.InitContainers))
	}
}
