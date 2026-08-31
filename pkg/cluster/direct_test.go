// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/FlashbackAi/teepin-core/pkg/networking"
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

// A bare Pod defaults to RestartPolicy: Always when left unset — correct
// for a customer's persistent compute instance (this test's baseline
// case), catastrophic for a one-shot workload like Kumbha's agent (see
// TestCreateInstance_NeverRestartSetsPodRestartPolicyNever). Found live
// 2026-08-24: an agent pod silently restarted mid-build and re-ran the
// entire build from scratch against the same original prompt.
func TestCreateInstance_DefaultsToRestartPolicyAlways(t *testing.T) {
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

	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("RestartPolicy = %q, want Always for a normal compute instance", pod.Spec.RestartPolicy)
	}
}

func TestCreateInstance_NeverRestartSetsPodRestartPolicyNever(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID:   "kumbha-agent-abc123",
		Image:        "teepin/kumbha-agent:latest",
		CPUUnits:     2,
		MemoryGB:     4,
		NeverRestart: true,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(
		context.Background(), metav1.ListOptions{})
	pod := pods.Items[0]

	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never — a restarted agent pod silently re-runs the whole build", pod.Spec.RestartPolicy)
	}
}

// TestCreateInstance_AllowFilesystemOwnershipChangesGrantsCapabilities
// and its sibling below are the regression tests for two real 2026-08-26
// incidents on the SAME "drop ALL capabilities" SecurityContext (see
// InstanceSpec.AllowFilesystemOwnershipChanges' own doc comment): first,
// every Kaniko build failed unpacking even the most ordinary base image
// (nginx:alpine) with "chown /etc/shadow: operation not permitted";
// then, one build later, a deployed nginx instance's own worker
// processes failed with "setgid(101): Operation not permitted" doing
// the completely standard root-to-less-privileged-user startup drop
// most daemon images use. Both capability groups are asserted together
// since both are granted by the same InstanceSpec field.
func TestCreateInstance_AllowFilesystemOwnershipChangesGrantsCapabilities(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID:                      "kaniko-build-abc123",
		Image:                           "gcr.io/kaniko-project/executor:v1.23.2-debug",
		CPUUnits:                        2,
		MemoryGB:                        4,
		AllowFilesystemOwnershipChanges: true,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(context.Background(), metav1.ListOptions{})
	caps := pods.Items[0].Spec.Containers[0].SecurityContext.Capabilities.Add

	for _, want := range []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETFCAP", "SETGID", "SETUID"} {
		found := false
		for _, got := range caps {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("capability %q not granted; got %v", want, caps)
		}
	}
}

// TestCreateInstance_FilesystemOwnershipCapabilitiesNotGrantedByDefault
// covers the buildPod mechanism itself, at the cluster layer: given a
// spec that does NOT ask for AllowFilesystemOwnershipChanges, none of
// these capabilities are added — independent of whatever policy a
// specific caller (pkg/api.instanceSpec now grants this to every
// customer instance; pkg/build.Service to every Kaniko build) happens to
// choose. The knob itself must still correctly withhold the grant when
// unset, or turning it off for some future workload would not actually
// do anything.
func TestCreateInstance_FilesystemOwnershipCapabilitiesNotGrantedByDefault(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-ordinary",
		Image:      "nginx:latest",
		CPUUnits:   1,
		MemoryGB:   1,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(context.Background(), metav1.ListOptions{})
	caps := pods.Items[0].Spec.Containers[0].SecurityContext.Capabilities.Add

	for _, forbidden := range []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETFCAP", "SETGID", "SETUID"} {
		for _, got := range caps {
			if got == forbidden {
				t.Errorf("AllowFilesystemOwnershipChanges=false still granted %q — the knob does not work", forbidden)
			}
		}
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

// TestDeleteInstance_PreservesKumbhaAgentPVC is the regression test for a
// live 2026-08-31 incident: a relaunched Kumbha agent found its own
// workspace completely empty — hours of prior work gone — because
// DeleteInstance (called by StopAgent's hard-kill, or LaunchAgent's own
// launch-failed cleanup) wiped the PVC unconditionally, contradicting
// StopAgent's own documented promise that the workspace survives a kill.
// A pod carrying the teepin.io/kumbha-agent label (LaunchAgent stamps
// this on every agent pod it creates) must keep its PVC through delete.
func TestDeleteInstance_PreservesKumbhaAgentPVC(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if _, err := c.CreateInstance(ctx, InstanceSpec{
		InstanceID: "kumbha-agent-abcd1234",
		Image:      "teepin/kumbha-agent:latest",
		CPUUnits:   2,
		MemoryGB:   4,
		StorageGB:  10,
		Labels:     map[string]string{labelKumbhaAgent: "true"},
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	if err := c.DeleteInstance(ctx, AllTenants(), "kumbha-agent-abcd1234"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}

	if _, err := c.k8s.CoreV1().PersistentVolumeClaims(workloadNamespace).
		Get(ctx, pvcName("kumbha-agent-abcd1234"), metav1.GetOptions{}); err != nil {
		t.Errorf("PVC was deleted along with a Kumbha agent pod, want it preserved: %v", err)
	}
}

// TestDeleteInstance_DeletesOrdinaryInstancePVC locks in the UNCHANGED
// behaviour for everything else: a customer deleting their own compute
// instance must still wipe its storage — only a Kumbha agent pod's own
// workspace gets the exception above.
func TestDeleteInstance_DeletesOrdinaryInstancePVC(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if _, err := c.CreateInstance(ctx, InstanceSpec{
		InstanceID: "inst-ordinary01",
		Image:      "nginx:latest",
		CPUUnits:   2,
		MemoryGB:   4,
		StorageGB:  10,
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	if err := c.DeleteInstance(ctx, AllTenants(), "inst-ordinary01"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}

	if _, err := c.k8s.CoreV1().PersistentVolumeClaims(workloadNamespace).
		Get(ctx, pvcName("inst-ordinary01"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("ordinary instance's PVC should be gone after delete, got err=%v", err)
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

// TestPodStatus_TerminatedContainerCarriesExitReason covers a real
// 2026-08-26 incident: a Kaniko build container that actually STARTED
// and then exited non-zero (by far the common build-failure shape) has
// State.Terminated set, not State.Waiting — the only branch podStatus
// checked before this fix. pod.Status.Reason (the other source this
// function reads) is also typically empty for a plain container-exit
// failure (it's really for pod-level eviction/admission failures), so
// Message stayed "" end to end, and the caller (pkg/build.Service.Build)
// fell back to the useless "build failed: failed" — a real customer saw
// exactly that, with no way to diagnose their own broken Dockerfile.
func TestPodStatus_TerminatedContainerCarriesExitReason(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{labelInstanceID: "inst-badbuild"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1,
						Reason:   "Error",
						Message:  "wget: can't open '/tmp/workspace.zip': No such file or directory",
					},
				},
			}},
		},
	}

	st := podStatus(pod)
	if st.Status != "failed" {
		t.Errorf("got status %q, want \"failed\"", st.Status)
	}
	if st.Message == "" {
		t.Fatal("failed status should carry a message the customer can act on, got empty")
	}
	if !strings.Contains(st.Message, "exit code 1") || !strings.Contains(st.Message, "workspace.zip") {
		t.Errorf("got message %q, want it to name the exit code and the container's own failure detail", st.Message)
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

// TestListInstanceStatuses_IncludeHiddenSeesKumbhaAgentAndBuildPods is
// the regression test for a live 2026-08-31 incident: a Kaniko build pod
// completed and pushed its image successfully in 11 seconds, but the
// control plane never learned it finished — waitForCompletion polled
// AgentClient.GetInstanceStatus (a pure cache read) forever, and the
// ONLY thing that ever refreshes that cache is the home-node agent's own
// reportStatuses sweep, which discovers instances via
// ListInstanceStatuses(AllTenants()) — a selector that (correctly, for a
// customer-facing list) excludes every teepin.io/kumbha-agent pod,
// Kaniko builds included. The cached status stayed frozen at its initial
// "pending" seed forever, and every deploy eventually reported "context
// deadline exceeded" for a build that had, in fact, already succeeded.
// AllTenantsIncludingHidden is the fix: the ONE caller (reportStatuses)
// that needs these pods to keep the cache honest, without touching what
// a customer's own Compute list shows.
func TestListInstanceStatuses_IncludeHiddenSeesKumbhaAgentAndBuildPods(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if _, err := c.CreateInstance(ctx, InstanceSpec{
		InstanceID: "kaniko-build-abcd1234",
		Image:      "gcr.io/kaniko-project/executor:latest",
		CPUUnits:   2,
		MemoryGB:   4,
		Labels:     map[string]string{labelKumbhaAgent: "true"},
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	// Default scope (what a customer-facing list uses): still excluded.
	statuses, err := c.ListInstanceStatuses(ctx, AllTenants())
	if err != nil {
		t.Fatalf("ListInstanceStatuses(AllTenants): %v", err)
	}
	for _, st := range statuses {
		if st.InstanceID == "kaniko-build-abcd1234" {
			t.Error("AllTenants() found the Kaniko build pod — it must stay hidden from a customer-facing list")
		}
	}

	// IncludeHidden (what reportStatuses uses): found, so its status can
	// actually reach the control plane's cache.
	statuses, err = c.ListInstanceStatuses(ctx, AllTenantsIncludingHidden())
	if err != nil {
		t.Fatalf("ListInstanceStatuses(AllTenantsIncludingHidden): %v", err)
	}
	found := false
	for _, st := range statuses {
		if st.InstanceID == "kaniko-build-abcd1234" {
			found = true
		}
	}
	if !found {
		t.Error("AllTenantsIncludingHidden() did not find the Kaniko build pod — its status can never reach the control plane's cache this way")
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

// fakeProvisioner is a minimal EndpointProvisioner that returns a fixed
// EndpointInfo, so CreateInstance's endpoint-field population can be
// tested without a fake Kubernetes Ingress/Service round trip (that part
// is already covered by pkg/networking's own tests).
type fakeProvisioner struct {
	info *networking.EndpointInfo
	err  error

	lastPort int32
	lastOpts networking.EndpointOptions
}

func (f *fakeProvisioner) ProvisionEndpoint(_ context.Context, _ uuid.UUID, _ string, port int32, opts networking.EndpointOptions) (*networking.EndpointInfo, error) {
	f.lastPort = port
	f.lastOpts = opts
	if f.err != nil {
		return nil, f.err
	}
	return f.info, nil
}

func (f *fakeProvisioner) RevokeEndpoint(context.Context, uuid.UUID) error { return nil }

func (f *fakeProvisioner) GetEndpointInfo(context.Context, uuid.UUID, string, networking.EndpointOptions) (*networking.EndpointInfo, error) {
	return f.info, f.err
}

// TestCreateInstance_PopulatesEndpointFieldsFromProvisioner is Stage 3
// defect 1's origin, closed: CreateInstance must carry every field the
// EndpointProvisioner returns onto InstanceResult, not just EndpointURL —
// this is what lets a caller with no Kubernetes access of its own (the
// control plane, in production) persist and serve the full endpoint
// picture from the result alone (see client.go's InstanceResult doc).
func TestCreateInstance_PopulatesEndpointFieldsFromProvisioner(t *testing.T) {
	provisioner := &fakeProvisioner{info: &networking.EndpointInfo{
		HTTPSURL:   "https://inst-endpoint1.teepin.com",
		HTTPURL:    "http://inst-endpoint1.teepin.com",
		PublicIP:   "203.0.113.5",
		DNSName:    "inst-endpoint1.teepin.com",
		TLSEnabled: true,
		TLSReady:   false, // cert-manager has not issued yet
	}}
	c := NewDirectClient(fake.NewSimpleClientset(), provisioner, nil, "nvidia")

	instanceUUID := uuid.New()
	result, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-endpoint1",
		Image:      "nginx:latest",
		CPUUnits:   1,
		MemoryGB:   2,
		Ports:      []PortMapping{{Container: 8080, Protocol: "tcp"}},
		Labels:     map[string]string{labelInstanceUUID: instanceUUID.String()},
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	if result.DNSName != "inst-endpoint1.teepin.com" {
		t.Errorf("DNSName = %q, want inst-endpoint1.teepin.com", result.DNSName)
	}
	if result.PublicIP != "203.0.113.5" {
		t.Errorf("PublicIP = %q, want 203.0.113.5", result.PublicIP)
	}
	if !result.TLSEnabled {
		t.Error("TLSEnabled=false, want true (from provisioner)")
	}
	if result.TLSReady {
		t.Error("TLSReady=true immediately after create — cert-manager issues asynchronously")
	}
	// TLS is enabled but not yet ready: the customer-visible URL must be
	// the HTTP one, not a broken HTTPS link waiting on a cert that has not
	// issued yet.
	if result.EndpointURL != "http://inst-endpoint1.teepin.com" {
		t.Errorf("EndpointURL = %q, want the HTTP URL while TLS is not ready", result.EndpointURL)
	}

	if provisioner.lastPort != 8080 {
		t.Errorf("provisioner received port %d, want 8080 (the customer's container port)", provisioner.lastPort)
	}
}

// TestCreateInstance_PrefersHTTPSWhenTLSReady covers the other half of the
// scheme-selection logic: once TLS IS ready, the HTTPS URL wins.
func TestCreateInstance_PrefersHTTPSWhenTLSReady(t *testing.T) {
	provisioner := &fakeProvisioner{info: &networking.EndpointInfo{
		HTTPSURL:   "https://inst-endpoint2.teepin.com",
		HTTPURL:    "http://inst-endpoint2.teepin.com",
		DNSName:    "inst-endpoint2.teepin.com",
		TLSEnabled: true,
		TLSReady:   true,
	}}
	c := NewDirectClient(fake.NewSimpleClientset(), provisioner, nil, "nvidia")

	result, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-endpoint2",
		Image:      "nginx:latest",
		CPUUnits:   1,
		MemoryGB:   2,
		Ports:      []PortMapping{{Container: 80, Protocol: "tcp"}},
		Labels:     map[string]string{labelInstanceUUID: uuid.New().String()},
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if result.EndpointURL != "https://inst-endpoint2.teepin.com" {
		t.Errorf("EndpointURL = %q, want the HTTPS URL once TLS is ready", result.EndpointURL)
	}
}

// TestCreateInstance_EndpointOptionsPassedThrough covers Stage 3 A9: a
// spec's EndpointDomain/EnableTLS/TLSIssuer must reach the provisioner as
// an override, not be silently dropped (the original defect 6).
func TestCreateInstance_EndpointOptionsPassedThrough(t *testing.T) {
	provisioner := &fakeProvisioner{info: &networking.EndpointInfo{DNSName: "inst-optthru01.custom.io"}}
	c := NewDirectClient(fake.NewSimpleClientset(), provisioner, nil, "nvidia")

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID:     "inst-optthru01",
		Image:          "nginx:latest",
		CPUUnits:       1,
		MemoryGB:       2,
		Ports:          []PortMapping{{Container: 80, Protocol: "tcp"}},
		Labels:         map[string]string{labelInstanceUUID: uuid.New().String()},
		EndpointDomain: "custom.io",
		EnableTLS:      true,
		TLSIssuer:      "letsencrypt-staging",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if provisioner.lastOpts.Domain != "custom.io" {
		t.Errorf("provisioner received Domain=%q, want custom.io", provisioner.lastOpts.Domain)
	}
	if provisioner.lastOpts.UseTLS == nil || !*provisioner.lastOpts.UseTLS {
		t.Error("provisioner did not receive a UseTLS=true override")
	}
	if provisioner.lastOpts.TLSIssuer != "letsencrypt-staging" {
		t.Errorf("provisioner received TLSIssuer=%q, want letsencrypt-staging", provisioner.lastOpts.TLSIssuer)
	}
}

// TestCreateInstance_NoPortsSkipsProvisioning: an instance with no
// requested ports gets no endpoint at all — provisioning an Ingress for a
// workload with nothing listening would be pointless and would leak an
// orphaned resource on delete paths that assume "ports means endpoint".
func TestCreateInstance_NoPortsSkipsProvisioning(t *testing.T) {
	provisioner := &fakeProvisioner{info: &networking.EndpointInfo{DNSName: "should-not-be-used"}}
	c := NewDirectClient(fake.NewSimpleClientset(), provisioner, nil, "nvidia")

	result, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-noports01",
		Image:      "nginx:latest",
		CPUUnits:   1,
		MemoryGB:   2,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if result.DNSName != "" || result.EndpointURL != "" {
		t.Errorf("expected no endpoint fields set with no ports requested, got DNSName=%q EndpointURL=%q", result.DNSName, result.EndpointURL)
	}
}

// TestCreateInstance_ProvisioningFailureIsNonFatal: endpoint provisioning
// is best-effort. A provisioner error must not fail the whole create — a
// running instance the customer can reach some other way beats one
// deleted because DNS provisioning had a transient failure.
func TestCreateInstance_ProvisioningFailureIsNonFatal(t *testing.T) {
	provisioner := &fakeProvisioner{err: errors.New("ingress controller unavailable")}
	c := NewDirectClient(fake.NewSimpleClientset(), provisioner, nil, "nvidia")

	result, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-provfail1",
		Image:      "nginx:latest",
		CPUUnits:   1,
		MemoryGB:   2,
		Ports:      []PortMapping{{Container: 80, Protocol: "tcp"}},
		Labels:     map[string]string{labelInstanceUUID: uuid.New().String()},
	})
	if err != nil {
		t.Fatalf("CreateInstance should not fail when endpoint provisioning fails, got: %v", err)
	}
	if result.PodName == "" {
		t.Error("pod should still have been created despite the provisioning failure")
	}
	if result.EndpointURL != "" {
		t.Error("EndpointURL should be empty when provisioning failed")
	}
}

// TestUpdateInstance_ReplacesPodKeepsEndpoint is the core guarantee
// UpdateInstance exists for (a Kumbha redeploy): the pod is swapped for a
// new one running the new image, but the Service/Ingress the customer's
// hostname resolves to is never recreated — same DNSName, same endpoint,
// because CreateInstance's own Service/Ingress creation is
// IsAlreadyExists-tolerant and the new pod carries the identical
// app.teepin.cloud/instance-id label the existing Service already
// selects on.
func TestUpdateInstance_ReplacesPodKeepsEndpoint(t *testing.T) {
	provisioner := &fakeProvisioner{info: &networking.EndpointInfo{
		HTTPSURL:   "https://inst-swap0001.teepin.com",
		DNSName:    "inst-swap0001.teepin.com",
		TLSEnabled: true,
		TLSReady:   true,
	}}
	c := NewDirectClient(fake.NewSimpleClientset(), provisioner, nil, "nvidia")
	instanceUUID := uuid.New()

	spec := InstanceSpec{
		InstanceID: "inst-swap0001",
		Image:      "myapp:v1",
		CPUUnits:   1,
		MemoryGB:   1,
		Ports:      []PortMapping{{Container: 80, Protocol: "tcp"}},
		Labels:     map[string]string{labelInstanceUUID: instanceUUID.String()},
	}
	if _, err := c.CreateInstance(context.Background(), spec); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	before, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(context.Background(), metav1.ListOptions{})
	if len(before.Items) != 1 {
		t.Fatalf("expected exactly 1 pod after create, got %d", len(before.Items))
	}
	oldPodName := before.Items[0].Name

	spec.Image = "myapp:v2"
	result, err := c.UpdateInstance(context.Background(), Scope{}, spec)
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	if result.DNSName != "inst-swap0001.teepin.com" {
		t.Errorf("DNSName = %q, want the SAME hostname the original create got", result.DNSName)
	}

	after, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(context.Background(), metav1.ListOptions{})
	if len(after.Items) != 1 {
		t.Fatalf("expected exactly 1 pod after update (old one replaced, not left behind), got %d", len(after.Items))
	}
	if after.Items[0].Name == oldPodName {
		t.Error("pod name did not change — the old pod was never actually replaced")
	}
	if after.Items[0].Spec.Containers[0].Image != "myapp:v2" {
		t.Errorf("new pod image = %q, want myapp:v2", after.Items[0].Spec.Containers[0].Image)
	}
	if after.Items[0].Labels[labelInstanceID] != "inst-swap0001" {
		t.Error("replacement pod must carry the same instance-id label the Service already selects on")
	}
}

// TestUpdateInstance_NoExistingInstanceIsNotFound: an update targeting an
// instance ID nothing created yet must not silently provision a fresh,
// orphaned instance — that would defeat the whole "same ID in, same ID
// out" contract this method exists for.
func TestUpdateInstance_NoExistingInstanceIsNotFound(t *testing.T) {
	c := newTestClient()

	_, err := c.UpdateInstance(context.Background(), Scope{}, InstanceSpec{
		InstanceID: "inst-neverexisted",
		Image:      "myapp:v1",
		CPUUnits:   1,
		MemoryGB:   1,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateInstance on a non-existent instance: err = %v, want ErrNotFound", err)
	}
}

// TestUpdateInstance_ScopedToProjectLikeAnyOtherMutation: an update must
// not be able to reach or replace another tenant's instance merely by
// guessing its ID — the same tenancy predicate DeleteInstance/GetInstance
// already enforce.
func TestUpdateInstance_ScopedToProjectLikeAnyOtherMutation(t *testing.T) {
	c := newTestClient()
	if _, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-tenantA01",
		Image:      "myapp:v1",
		CPUUnits:   1,
		MemoryGB:   1,
		ProjectID:  "project-alice",
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	_, err := c.UpdateInstance(context.Background(), ProjectScope("project-bob"), InstanceSpec{
		InstanceID: "inst-tenantA01",
		Image:      "myapp:v2",
		CPUUnits:   1,
		MemoryGB:   1,
		ProjectID:  "project-bob",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant UpdateInstance: err = %v, want ErrNotFound", err)
	}
}
