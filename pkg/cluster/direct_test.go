// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"bytes"
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// newTestClient builds a DirectClient over a fake cluster.
//
// networking and inventory are nil: these tests cover pod construction
// and status mapping, which is where the production failures were.
func newTestClient() *DirectClient {
	return NewDirectClient(fake.NewSimpleClientset(), nil, nil, "nvidia")
}

func TestCreateInstance_GPUPodGetsRuntimeClass(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID:  "inst-abc123",
		Image:       "nvcr.io/nvidia/pytorch:24.01",
		CPUUnits:    8,
		MemoryGB:    32,
		GPUResource: "nvidia.com/mig-2g.20gb",
		GPUQuantity: 1,
		GPUVRAMGB:   20,
		NodeName:    "gpu-node-1",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, err := c.k8s.CoreV1().Pods(workloadNamespace).List(
		context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(pods.Items))
	}
	pod := pods.Items[0]

	// Regression: a GPU pod without the NVIDIA runtime class runs under
	// plain runc. Kubernetes still accounts for the GPU and the customer
	// is still billed, but no device nodes or driver libraries are
	// injected — the GPU is unusable. This shipped once and was found
	// only by exec'ing into a running container.
	if pod.Spec.RuntimeClassName == nil {
		t.Fatal("GPU pod has no RuntimeClassName: the GPU would be billed but unusable")
	}
	if *pod.Spec.RuntimeClassName != "nvidia" {
		t.Errorf("RuntimeClassName = %q, want \"nvidia\"", *pod.Spec.RuntimeClassName)
	}

	// Pinned to the node the allocator accounted capacity against.
	if got := pod.Spec.NodeSelector["kubernetes.io/hostname"]; got != "gpu-node-1" {
		t.Errorf("node pin = %q, want \"gpu-node-1\"", got)
	}

	// The GPU resource must appear in Limits or the device plugin never
	// assigns a device.
	if _, ok := pod.Spec.Containers[0].Resources.Limits["nvidia.com/mig-2g.20gb"]; !ok {
		t.Error("GPU resource missing from container limits")
	}
}

func TestCreateInstance_CPUPodHasNoRuntimeClass(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-cpu001",
		Image:      "nginx:latest",
		CPUUnits:   2,
		MemoryGB:   4,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(
		context.Background(), metav1.ListOptions{})
	pod := pods.Items[0]

	// Forcing the NVIDIA runtime on CPU-only workloads would make them
	// unschedulable on nodes without it.
	if pod.Spec.RuntimeClassName != nil {
		t.Errorf("CPU pod should have no RuntimeClassName, got %q", *pod.Spec.RuntimeClassName)
	}
}

func TestCreateInstance_NoServiceAccountToken(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-tenant1",
		Image:      "customer/workload:v1",
		CPUUnits:   1,
		MemoryGB:   2,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(
		context.Background(), metav1.ListOptions{})
	pod := pods.Items[0]

	// Tenant isolation: a customer container with a service account token
	// can talk to the Kubernetes API. Network policy alone does not close
	// this, so the token must be absent.
	if pod.Spec.AutomountServiceAccountToken == nil {
		t.Fatal("AutomountServiceAccountToken is nil: customer pod would receive an API token")
	}
	if *pod.Spec.AutomountServiceAccountToken {
		t.Error("customer pod must not automount a service account token")
	}
}

func TestCreateInstance_CommandAndArgsPreserved(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-cmd001",
		Image:      "python:3.12",
		Command:    []string{"python"},
		Args:       []string{"-m", "http.server", "8080"},
		CPUUnits:   1,
		MemoryGB:   2,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(
		context.Background(), metav1.ListOptions{})
	container := pods.Items[0].Spec.Containers[0]

	// Regression: these were accepted by the API and silently dropped,
	// leaving base images crash-looping with no explanation.
	if len(container.Command) != 1 || container.Command[0] != "python" {
		t.Errorf("Command = %v, want [python]", container.Command)
	}
	if len(container.Args) != 3 {
		t.Errorf("Args = %v, want 3 elements", container.Args)
	}
}

func TestDeleteInstance_MissingIsNotAnError(t *testing.T) {
	c := newTestClient()

	// Commands may be redelivered after an agent reconnects, so deleting
	// something already gone must succeed rather than fail a retry loop.
	if err := c.DeleteInstance(context.Background(), AllTenants(), "inst-neverexisted"); err != nil {
		t.Errorf("deleting a missing instance should be nil, got %v", err)
	}
}

func TestGetInstanceStatus_NotFound(t *testing.T) {
	c := newTestClient()

	_, err := c.GetInstanceStatus(context.Background(), AllTenants(), "inst-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestPodStatus_ImagePullIsFailedNotPending(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{labelInstanceID: "inst-badimage"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason:  "ImagePullBackOff",
						Message: "manifest unknown",
					},
				},
			}},
		},
	}

	// Kubernetes calls this Pending forever. Billing a customer for a
	// container that will never start is wrong, so TEEPIN calls it failed.
	st := podStatus(pod)
	if st.Status != "failed" {
		t.Errorf("ImagePullBackOff mapped to %q, want \"failed\"", st.Status)
	}
	if st.Message == "" {
		t.Error("failed status should carry a message the customer can act on")
	}
}

func TestPodStatus_RunningIsRunning(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{labelInstanceID: "inst-ok"},
		},
		Spec:   corev1.PodSpec{NodeName: "gpu-node-1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	st := podStatus(pod)
	if st.Status != "running" {
		t.Errorf("Status = %q, want \"running\"", st.Status)
	}
	if st.NodeName != "gpu-node-1" {
		t.Errorf("NodeName = %q, want \"gpu-node-1\"", st.NodeName)
	}
}

func TestListInstanceStatuses_OnlyManagedPods(t *testing.T) {
	c := NewDirectClient(fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "teepin-workload",
				Namespace: workloadNamespace,
				Labels: map[string]string{
					labelManaged:    "true",
					labelInstanceID: "inst-mine",
				},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		// A pod TEEPIN does not manage. The reconciler stops billing for
		// instances missing from this list, so including a foreign pod
		// would be harmless, but excluding managed ones would not — and
		// treating someone else's pod as an instance is a tenancy leak.
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kube-dns",
				Namespace: workloadNamespace,
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
	), nil, nil, "nvidia")

	statuses, err := c.ListInstanceStatuses(context.Background(), AllTenants())
	if err != nil {
		t.Fatalf("ListInstanceStatuses: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("expected 1 managed instance, got %d", len(statuses))
	}
	if statuses[0].InstanceID != "inst-mine" {
		t.Errorf("InstanceID = %q, want \"inst-mine\"", statuses[0].InstanceID)
	}
}

func TestStreamLogs_NotFound(t *testing.T) {
	c := newTestClient()

	var buf bytes.Buffer
	err := c.StreamLogs(context.Background(), AllTenants(), "inst-missing", LogOptions{}, &buf)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// twoTenantCluster returns a cluster holding one instance for each of
// two projects.
func twoTenantCluster() *DirectClient {
	mk := func(instanceID, projectID string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instanceID,
				Namespace: workloadNamespace,
				Labels: map[string]string{
					labelManaged:    "true",
					labelInstanceID: instanceID,
					labelProjectID:  projectID,
				},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	return NewDirectClient(fake.NewSimpleClientset(
		mk("inst-alice01", "project-alice"),
		mk("inst-bob0001", "project-bob"),
	), nil, nil, "nvidia")
}

// The tenancy tests below are the reason Scope is a required argument.
// An earlier version of the API let any authenticated caller read
// another customer's project by ID; these pin the equivalent boundary
// for compute so it cannot regress silently.

func TestGetInstanceStatus_OtherTenantIsNotFound(t *testing.T) {
	c := twoTenantCluster()

	// Alice knows Bob's instance ID — IDs appear in logs, URLs and
	// support tickets, so this is not a hypothetical.
	_, err := c.GetInstanceStatus(
		context.Background(), ProjectScope("project-alice"), "inst-bob0001")

	// Not merely denied: indistinguishable from absent, so Alice cannot
	// confirm the instance exists at all.
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("reading another tenant's instance returned %v, want ErrNotFound", err)
	}
}

func TestGetInstanceStatus_OwnInstanceIsVisible(t *testing.T) {
	c := twoTenantCluster()

	st, err := c.GetInstanceStatus(
		context.Background(), ProjectScope("project-alice"), "inst-alice01")
	if err != nil {
		t.Fatalf("reading own instance failed: %v", err)
	}
	if st.InstanceID != "inst-alice01" {
		t.Errorf("InstanceID = %q, want \"inst-alice01\"", st.InstanceID)
	}
}

func TestDeleteInstance_CannotDeleteOtherTenant(t *testing.T) {
	c := twoTenantCluster()

	// Delete is idempotent, so this returns nil either way. What matters
	// is whether Bob's pod survived.
	if err := c.DeleteInstance(
		context.Background(), ProjectScope("project-alice"), "inst-bob0001"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}

	_, err := c.k8s.CoreV1().Pods(workloadNamespace).Get(
		context.Background(), "inst-bob0001", metav1.GetOptions{})
	if err != nil {
		t.Fatal("another tenant's pod was deleted: scope was not enforced on delete")
	}
}

func TestListInstanceStatuses_ScopedToProject(t *testing.T) {
	c := twoTenantCluster()

	statuses, err := c.ListInstanceStatuses(
		context.Background(), ProjectScope("project-alice"))
	if err != nil {
		t.Fatalf("ListInstanceStatuses: %v", err)
	}

	if len(statuses) != 1 {
		t.Fatalf("expected 1 instance in Alice's scope, got %d", len(statuses))
	}
	if statuses[0].InstanceID != "inst-alice01" {
		t.Errorf("listed %q, want \"inst-alice01\"", statuses[0].InstanceID)
	}
}

func TestListInstanceStatuses_AllTenantsSeesEverything(t *testing.T) {
	c := twoTenantCluster()

	// The reconciler must see every tenant's instances: an instance it
	// cannot see is one it keeps billing after the pod disappears.
	statuses, err := c.ListInstanceStatuses(context.Background(), AllTenants())
	if err != nil {
		t.Fatalf("ListInstanceStatuses: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("AllTenants returned %d instances, want 2", len(statuses))
	}
}

func TestStreamLogs_OtherTenantIsNotFound(t *testing.T) {
	c := twoTenantCluster()

	// Logs routinely contain credentials, prompts and customer data, so
	// this boundary matters as much as the status one.
	var buf bytes.Buffer
	err := c.StreamLogs(
		context.Background(), ProjectScope("project-alice"), "inst-bob0001",
		LogOptions{}, &buf)

	if !errors.Is(err, ErrNotFound) {
		t.Errorf("reading another tenant's logs returned %v, want ErrNotFound", err)
	}
	if buf.Len() > 0 {
		t.Error("log bytes were written for another tenant's instance")
	}
}

func TestCreateInstance_TenancyLabelsCannotBeOverridden(t *testing.T) {
	c := newTestClient()

	// A customer-supplied label must not be able to reassign the pod to
	// another project, which would make it readable by that tenant and
	// invisible to its owner.
	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-evil001",
		Image:      "attacker/image:v1",
		CPUUnits:   1,
		MemoryGB:   1,
		ProjectID:  "project-alice",
		Labels: map[string]string{
			labelProjectID: "project-victim",
		},
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(
		context.Background(), metav1.ListOptions{})
	got := pods.Items[0].Labels[labelProjectID]

	if got != "project-alice" {
		t.Errorf("project label = %q, want \"project-alice\": a customer label overrode tenancy", got)
	}
}

func TestNodeReadiness_CordonedNodeIsNotReady(t *testing.T) {
	c := NewDirectClient(fake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "healthy"},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			}},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "cordoned"},
			Spec:       corev1.NodeSpec{Unschedulable: true},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			}},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "notready"},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			}},
		},
	), nil, nil, "nvidia")

	ready := c.nodeReadiness(context.Background())

	if !ready["healthy"] {
		t.Error("healthy node should be ready")
	}
	// A cordoned node still advertises allocatable GPU resources, so
	// capacity alone would place work somewhere it cannot run.
	if ready["cordoned"] {
		t.Error("cordoned node must not be reported ready")
	}
	if ready["notready"] {
		t.Error("NotReady node must not be reported ready")
	}
}
