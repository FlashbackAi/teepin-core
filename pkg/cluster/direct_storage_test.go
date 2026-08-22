// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestCreateInstance_EphemeralStorageLimitSet is the regression test for a
// real bug found live 2026-08-21: buildPod set no ephemeral-storage limit
// at all, so a workload's writable layer was backed directly by the
// host's full disk (1007G on Srialla for a "2 vCPU / 4GB" instance).
func TestCreateInstance_EphemeralStorageLimitSet(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID:         "inst-eph00001",
		Image:              "nginx:latest",
		CPUUnits:           2,
		MemoryGB:           4,
		EphemeralStorageGB: 10,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(context.Background(), metav1.ListOptions{})
	pod := pods.Items[0]

	limit, ok := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceEphemeralStorage]
	if !ok {
		t.Fatal("no ephemeral-storage limit set — a single instance could exhaust a shared node's disk")
	}
	if got := limit.String(); got != "10Gi" {
		t.Errorf("ephemeral-storage limit = %q, want 10Gi", got)
	}
	if _, ok := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceEphemeralStorage]; !ok {
		t.Error("ephemeral-storage request not set alongside the limit")
	}
}

// TestCreateInstance_NoEphemeralStorageMeansNoLimit guards the other
// direction: a spec that genuinely sets no cap (EphemeralStorageGB == 0)
// must not have a zero-quantity limit parsed in, which Kubernetes would
// treat as "may use no disk at all" and evict the pod immediately.
func TestCreateInstance_NoEphemeralStorageMeansNoLimit(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-eph00002",
		Image:      "nginx:latest",
		CPUUnits:   2,
		MemoryGB:   4,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(context.Background(), metav1.ListOptions{})
	pod := pods.Items[0]

	if _, ok := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceEphemeralStorage]; ok {
		t.Error("ephemeral-storage limit set despite EphemeralStorageGB == 0")
	}
}

// TestCreateInstance_MountsVolumeOnlyWhenStorageRequested covers both
// directions of the /data mount.
func TestCreateInstance_MountsVolumeOnlyWhenStorageRequested(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-vol00001",
		Image:      "nginx:latest",
		CPUUnits:   2,
		MemoryGB:   4,
		StorageGB:  5,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(context.Background(), metav1.ListOptions{})
	pod := pods.Items[0]

	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Fatalf("expected one PVC volume, got %+v", pod.Spec.Volumes)
	}
	if want := pvcName("inst-vol00001"); pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != want {
		t.Errorf("claim name = %q, want %q", pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName, want)
	}
	mounts := pod.Spec.Containers[0].VolumeMounts
	if len(mounts) != 1 || mounts[0].MountPath != "/data" {
		t.Errorf("volume mounts = %+v, want exactly one at /data", mounts)
	}

	// The PVC itself must have actually been created, sized correctly.
	pvc, err := c.k8s.CoreV1().PersistentVolumeClaims(workloadNamespace).Get(
		context.Background(), pvcName("inst-vol00001"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pvc: %v", err)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "5Gi" {
		t.Errorf("pvc size = %q, want 5Gi", got.String())
	}

	// No storage requested — no volume, no mount, no PVC.
	_, err = c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-novol001",
		Image:      "nginx:latest",
		CPUUnits:   2,
		MemoryGB:   4,
	})
	if err != nil {
		t.Fatalf("CreateInstance (no storage): %v", err)
	}
	pods2, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(context.Background(),
		metav1.ListOptions{LabelSelector: labelInstanceShort + "=inst-novol001"})
	if len(pods2.Items) != 1 {
		t.Fatalf("expected 1 pod for inst-novol001, got %d", len(pods2.Items))
	}
	if len(pods2.Items[0].Spec.Volumes) != 0 {
		t.Errorf("expected no volumes when StorageGB == 0, got %+v", pods2.Items[0].Spec.Volumes)
	}
	if _, err := c.k8s.CoreV1().PersistentVolumeClaims(workloadNamespace).Get(
		context.Background(), pvcName("inst-novol001"), metav1.GetOptions{}); err == nil {
		t.Error("a PVC was created despite StorageGB == 0")
	}
}

// TestCreateInstance_PVCCreateIsIdempotent: a redelivered create command
// (the control plane's own retry/reconnect story) must not fail on a PVC
// that already exists from the first attempt.
func TestCreateInstance_PVCCreateIsIdempotent(t *testing.T) {
	c := newTestClient()
	spec := InstanceSpec{
		InstanceID: "inst-idem0001",
		Image:      "nginx:latest",
		CPUUnits:   2,
		MemoryGB:   4,
		StorageGB:  5,
	}

	if _, err := c.CreateInstance(context.Background(), spec); err != nil {
		t.Fatalf("first CreateInstance: %v", err)
	}
	// Second call reuses the same instance ID -> same PVC name. Create
	// against the fake clientset will hit IsAlreadyExists for the PVC and
	// the NetworkPolicy; both must be treated as success, not an error
	// that aborts pod creation.
	if _, err := c.CreateInstance(context.Background(), spec); err != nil {
		t.Fatalf("redelivered CreateInstance must be idempotent, got: %v", err)
	}
}

// TestDeleteInstance_RemovesPVCAndNetworkPolicy covers teardown: both
// objects named deterministically from the instance ID must be gone
// after delete, and a second delete (nothing left to remove) must still
// succeed — commands may be redelivered after an agent reconnects.
func TestDeleteInstance_RemovesPVCAndNetworkPolicy(t *testing.T) {
	c := newTestClient()
	spec := InstanceSpec{
		InstanceID: "inst-del00001",
		Image:      "nginx:latest",
		CPUUnits:   2,
		MemoryGB:   4,
		StorageGB:  5,
	}
	if _, err := c.CreateInstance(context.Background(), spec); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	if err := c.DeleteInstance(context.Background(), AllTenants(), "inst-del00001"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}

	if _, err := c.k8s.CoreV1().PersistentVolumeClaims(workloadNamespace).Get(
		context.Background(), pvcName("inst-del00001"), metav1.GetOptions{}); err == nil {
		t.Error("PVC still exists after delete")
	}
	if _, err := c.k8s.NetworkingV1().NetworkPolicies(workloadNamespace).Get(
		context.Background(), networkPolicyName("inst-del00001"), metav1.GetOptions{}); err == nil {
		t.Error("NetworkPolicy still exists after delete")
	}

	// Redelivered delete: nothing left to remove, must still succeed.
	if err := c.DeleteInstance(context.Background(), AllTenants(), "inst-del00001"); err != nil {
		t.Fatalf("redelivered DeleteInstance must be idempotent, got: %v", err)
	}
}

// TestCreateInstance_SecurityContextHardening covers the pod- and
// container-level SecurityContext added alongside NetworkPolicy —
// deliberately not RunAsNonRoot (would break nginx, which runs as root
// and binds port 80) but with every capability dropped except the one
// that same nginx needs back.
func TestCreateInstance_SecurityContextHardening(t *testing.T) {
	c := newTestClient()

	_, err := c.CreateInstance(context.Background(), InstanceSpec{
		InstanceID: "inst-sec00001",
		Image:      "nginx:latest",
		CPUUnits:   2,
		MemoryGB:   4,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	pods, _ := c.k8s.CoreV1().Pods(workloadNamespace).List(context.Background(), metav1.ListOptions{})
	pod := pods.Items[0]

	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.SeccompProfile == nil ||
		pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("pod SecurityContext missing RuntimeDefault seccomp profile")
	}

	sc := pod.Spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("container has no SecurityContext at all")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be explicitly false")
	}
	if sc.RunAsNonRoot != nil && *sc.RunAsNonRoot {
		t.Error("RunAsNonRoot must NOT be forced — nginx (and most base images) run as root")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("expected exactly Drop:[ALL], got %+v", sc.Capabilities)
	}
	found := false
	for _, cap := range sc.Capabilities.Add {
		if cap == "NET_BIND_SERVICE" {
			found = true
		}
	}
	if !found {
		t.Error("NET_BIND_SERVICE not re-added — nginx binding port 80 as root would fail")
	}
}

// TestBuildNetworkPolicy_AllowsDNSBeforeBlockingPodCIDR is the regression
// test for the classic NetworkPolicy footgun: CoreDNS's own pod lives
// inside the pod CIDR, so an egress rule that blocks the pod CIDR without
// first explicitly allowing kube-dns breaks ALL name resolution, not just
// pod-to-pod traffic.
func TestBuildNetworkPolicy_AllowsDNSBeforeBlockingPodCIDR(t *testing.T) {
	np := buildNetworkPolicy(InstanceSpec{InstanceID: "inst-dns00001"}, "10.42.0.0/16")

	if len(np.Spec.Egress) < 2 {
		t.Fatalf("expected at least 2 egress rules (DNS, then general), got %d", len(np.Spec.Egress))
	}
	dnsRule := np.Spec.Egress[0]
	if len(dnsRule.To) != 1 || dnsRule.To[0].PodSelector == nil ||
		dnsRule.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Errorf("first egress rule must target kube-dns by pod selector, got %+v", dnsRule.To)
	}
	if len(dnsRule.Ports) != 2 {
		t.Errorf("DNS rule should allow exactly UDP+TCP 53, got %+v", dnsRule.Ports)
	}

	generalRule := np.Spec.Egress[1]
	if len(generalRule.To) != 1 || generalRule.To[0].IPBlock == nil {
		t.Fatalf("second egress rule must be an IPBlock rule, got %+v", generalRule.To)
	}
	except := generalRule.To[0].IPBlock.Except
	hasPodCIDR := false
	hasMetadata := false
	for _, e := range except {
		if e == "10.42.0.0/16" {
			hasPodCIDR = true
		}
		if e == "169.254.169.254/32" {
			hasMetadata = true
		}
	}
	if !hasPodCIDR {
		t.Error("general egress rule does not exclude the pod CIDR — pod-to-pod would not be blocked")
	}
	if !hasMetadata {
		t.Error("general egress rule does not exclude the cloud metadata address")
	}
}

// TestBuildNetworkPolicy_IngressBlocksPodCIDROnly confirms the ingress
// side permits the node/ingress/internet but excludes the pod CIDR —
// the actual pod-to-pod block.
func TestBuildNetworkPolicy_IngressBlocksPodCIDROnly(t *testing.T) {
	np := buildNetworkPolicy(InstanceSpec{InstanceID: "inst-ing00001"}, "10.42.0.0/16")

	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("expected exactly 1 ingress rule, got %d", len(np.Spec.Ingress))
	}
	rule := np.Spec.Ingress[0]
	if len(rule.From) != 1 || rule.From[0].IPBlock == nil {
		t.Fatalf("ingress rule must be an IPBlock rule, got %+v", rule.From)
	}
	if rule.From[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Errorf("ingress CIDR = %q, want 0.0.0.0/0", rule.From[0].IPBlock.CIDR)
	}
	if len(rule.From[0].IPBlock.Except) != 1 || rule.From[0].IPBlock.Except[0] != "10.42.0.0/16" {
		t.Errorf("ingress except = %+v, want exactly the pod CIDR", rule.From[0].IPBlock.Except)
	}
}

// TestEffectivePodCIDR_DefaultsWhenUnset — a NewDirectClient call site
// that never calls WithPodCIDR must still get a correct, working
// NetworkPolicy rather than one built against an empty CIDR (which would
// match nothing, silently making the "except" clause a no-op and
// blocking DNS and the node along with everything else).
func TestEffectivePodCIDR_DefaultsWhenUnset(t *testing.T) {
	c := NewDirectClient(fake.NewSimpleClientset(), nil, nil, "nvidia")
	if got := c.effectivePodCIDR(); got != defaultPodCIDR {
		t.Errorf("effectivePodCIDR() = %q, want the default %q", got, defaultPodCIDR)
	}

	c.WithPodCIDR("192.168.0.0/16")
	if got := c.effectivePodCIDR(); got != "192.168.0.0/16" {
		t.Errorf("effectivePodCIDR() = %q, want the overridden value", got)
	}
}
