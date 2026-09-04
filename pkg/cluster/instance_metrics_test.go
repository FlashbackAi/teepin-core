// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
)

// podMetricsResource is the metrics.k8s.io/v1beta1 PodMetrics REST
// resource — "pods", not the pluralized-from-Kind "podmetricses" one
// might expect. That mismatch matters here: the fake clientset's generic
// Tracker().Add(obj) guesses a resource name by pluralizing the Kind
// ("PodMetrics" -> "podmetricses"), which is WRONG for this API and
// silently stores seeded objects where the real List() call — which
// correctly targets "pods" — never finds them (verified directly: an
// object seeded via plain NewSimpleClientset(obj) results in zero items
// on every subsequent List, with no error at any step). Seeding via
// Tracker().Create() with this EXPLICIT resource name is the necessary
// workaround, not a preference.
var podMetricsResource = schema.GroupVersionResource{
	Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods",
}

// newMetricsClientWithPods builds a fake metrics.k8s.io client seeded
// with the given PodMetrics objects — see podMetricsResource's own
// comment for why this cannot simply be
// metricsfake.NewSimpleClientset(objects...).
func newMetricsClientWithPods(t *testing.T, pods ...*metricsv1beta1.PodMetrics) metricsclientset.Interface {
	t.Helper()
	mc := metricsfake.NewSimpleClientset()
	for _, p := range pods {
		if err := mc.Tracker().Create(podMetricsResource, p, p.Namespace); err != nil {
			t.Fatalf("seed PodMetrics %s/%s: %v", p.Namespace, p.Name, err)
		}
	}
	return mc
}

// ---------------------------------------------------------------------
// instanceNetRates — pure function, same guard shape as
// pkg/agentrunner/hostmetrics.go's netRatesFromSamples.
// ---------------------------------------------------------------------

func TestInstanceNetRates(t *testing.T) {
	prev := instanceIOSample{rxBytes: 0, txBytes: 0, at: time.Now().Add(-2 * time.Second)}
	cur := netCounters{rxBytes: 10 * 1024 * 1024, txBytes: 2 * 1024 * 1024}
	rx, tx, ok := instanceNetRates(prev, cur, time.Now())
	if !ok {
		t.Fatal("expected ok=true")
	}
	if rx < 4.9 || rx > 5.1 {
		t.Errorf("rxMBps = %v, want ~5", rx)
	}
	if tx < 0.9 || tx > 1.1 {
		t.Errorf("txMBps = %v, want ~1", tx)
	}
}

func TestInstanceNetRates_CounterReset(t *testing.T) {
	prev := instanceIOSample{rxBytes: 1000, txBytes: 1000, at: time.Now().Add(-time.Second)}
	cur := netCounters{rxBytes: 10, txBytes: 10}
	if _, _, ok := instanceNetRates(prev, cur, time.Now()); ok {
		t.Error("expected ok=false when a counter goes backwards (pod restart)")
	}
}

func TestInstanceNetRates_ZeroElapsed(t *testing.T) {
	now := time.Now()
	prev := instanceIOSample{rxBytes: 100, txBytes: 100, at: now}
	cur := netCounters{rxBytes: 200, txBytes: 200}
	if _, _, ok := instanceNetRates(prev, cur, now); ok {
		t.Error("expected ok=false when elapsed is zero")
	}
}

// ---------------------------------------------------------------------
// statsSummary JSON shape — proves the hand-declared struct actually
// matches the kubelet's real /stats/summary field names/nesting, since
// nothing here is generated from the upstream type.
// ---------------------------------------------------------------------

func TestStatsSummary_UnmarshalsRealShape(t *testing.T) {
	raw := `{
		"pods": [
			{
				"podRef": {"name": "inst-abc123-xy9z1", "namespace": "default"},
				"network": {"rxBytes": 1048576, "txBytes": 524288},
				"ephemeral-storage": {"usedBytes": 2147483648}
			},
			{
				"podRef": {"name": "some-other-pod", "namespace": "kube-system"}
			}
		]
	}`
	var summary statsSummary
	if err := json.Unmarshal([]byte(raw), &summary); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(summary.Pods) != 2 {
		t.Fatalf("got %d pods, want 2", len(summary.Pods))
	}
	p := summary.Pods[0]
	if p.PodRef.Name != "inst-abc123-xy9z1" || p.PodRef.Namespace != "default" {
		t.Errorf("podRef = %+v", p.PodRef)
	}
	if p.Network == nil || *p.Network.RxBytes != 1048576 || *p.Network.TxBytes != 524288 {
		t.Errorf("network = %+v", p.Network)
	}
	if p.EphemeralStorage == nil || *p.EphemeralStorage.UsedBytes != 2147483648 {
		t.Errorf("ephemeral-storage = %+v", p.EphemeralStorage)
	}
	// The second pod has neither field present — a real kubelet omits
	// them for a pod it has no cAdvisor stats for yet, not a zero value.
	if summary.Pods[1].Network != nil || summary.Pods[1].EphemeralStorage != nil {
		t.Error("pod with no network/storage stats should decode as nil pointers, not zero structs")
	}
}

// ---------------------------------------------------------------------
// DirectClient.InstanceMetrics
// ---------------------------------------------------------------------

// managedPod builds a pod matching what DirectClient itself creates for
// a customer instance: managed+instance-id labels, a NodeName, and a
// fixed CPU/memory request==limit (this platform's allocation model).
func managedPod(instanceID, podName, nodeName string, cpuMilli int64, memBytes int64) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: workloadNamespace,
			Labels: map[string]string{
				labelManaged:    "true",
				labelInstanceID: instanceID,
			},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name: "main",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(memBytes, resource.BinarySI),
					},
				},
			}},
		},
	}
}

func podMetrics(podName string, cpuMilli int64, memBytes int64) *metricsv1beta1.PodMetrics {
	return &metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: workloadNamespace},
		Containers: []metricsv1beta1.ContainerMetrics{{
			Name: "main",
			Usage: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(memBytes, resource.BinarySI),
			},
		}},
	}
}

// Without a REST config, the feature is off entirely — no error, no
// data, same "genuinely absent, not a failure" posture as CPUOnly's own
// Inventory.
func TestInstanceMetrics_NoRESTConfig(t *testing.T) {
	c := NewDirectClient(fake.NewSimpleClientset(), nil, nil, "nvidia")
	got, err := c.InstanceMetrics(context.Background())
	if err != nil {
		t.Fatalf("InstanceMetrics: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil (no REST config configured)", got)
	}
}

// No managed pods at all: an empty, successful result, not an error.
func TestInstanceMetrics_NoPods(t *testing.T) {
	c := NewDirectClient(fake.NewSimpleClientset(), nil, nil, "nvidia")
	c.metricsClient = metricsfake.NewSimpleClientset()

	got, err := c.InstanceMetrics(context.Background())
	if err != nil {
		t.Fatalf("InstanceMetrics: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil (no pods)", got)
	}
}

// The core CPU%-relative-to-the-instance's-own-limit computation: 500m
// used against a 1000m limit is 50%, not some fraction of host capacity.
// The fake k8s clientset's REST client has no stats/summary route, so
// network/storage are unavailable this sweep — proving CPU/memory alone
// is enough to produce a result (the "best-effort per field" contract).
func TestInstanceMetrics_CPUPercentRelativeToInstanceLimit(t *testing.T) {
	pod := managedPod("inst-abc123", "inst-abc123-xy9z1", "node-1", 1000, 2*1024*1024*1024)
	c := NewDirectClient(fake.NewSimpleClientset(pod), nil, nil, "nvidia")
	c.metricsClient = newMetricsClientWithPods(t,
		podMetrics("inst-abc123-xy9z1", 500, 1*1024*1024*1024),
	)

	got, err := c.InstanceMetrics(context.Background())
	if err != nil {
		t.Fatalf("InstanceMetrics: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d metrics, want 1", len(got))
	}
	m := got[0]
	if m.InstanceID != "inst-abc123" {
		t.Errorf("InstanceID = %q, want inst-abc123", m.InstanceID)
	}
	if m.CPUUsedPercent < 49.9 || m.CPUUsedPercent > 50.1 {
		t.Errorf("CPUUsedPercent = %v, want ~50 (500m used / 1000m limit)", m.CPUUsedPercent)
	}
	if m.MemoryUsedGB < 0.99 || m.MemoryUsedGB > 1.01 {
		t.Errorf("MemoryUsedGB = %v, want ~1", m.MemoryUsedGB)
	}
}

// A pod with no CPU limit set at all (should not happen in this
// platform's fixed-allocation model, but defensively) must not divide
// by zero — and with no other metric source available in this test, it
// simply does not appear in the result.
func TestInstanceMetrics_ZeroCPULimit_Excluded(t *testing.T) {
	pod := managedPod("inst-nolimit", "inst-nolimit-ab12c", "node-1", 0, 0)
	c := NewDirectClient(fake.NewSimpleClientset(pod), nil, nil, "nvidia")
	c.metricsClient = newMetricsClientWithPods(t,
		podMetrics("inst-nolimit-ab12c", 200, 500*1024*1024),
	)

	got, err := c.InstanceMetrics(context.Background())
	if err != nil {
		t.Fatalf("InstanceMetrics: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d metrics, want 0 (zero CPU limit, nothing else measurable)", len(got))
	}
}

// A metrics.k8s.io failure (metrics-server not ready) must not fail the
// whole call — it degrades to an empty result here since this test gives
// it no other data source, mirroring the per-field best-effort contract.
func TestInstanceMetrics_MetricsServerUnavailable_NotFatal(t *testing.T) {
	pod := managedPod("inst-abc123", "inst-abc123-xy9z1", "node-1", 1000, 2*1024*1024*1024)
	c := NewDirectClient(fake.NewSimpleClientset(pod), nil, nil, "nvidia")
	c.metricsClient = metricsfake.NewSimpleClientset() // no PodMetrics seeded -> List returns empty, not an error

	got, err := c.InstanceMetrics(context.Background())
	if err != nil {
		t.Fatalf("InstanceMetrics must not fail when metrics-server has nothing yet: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d metrics, want 0 (no usage data, no network/storage data)", len(got))
	}
}

// Two calls with an intervening usage change prove the per-instance
// state (cpuLimit lookup, result set) is recomputed fresh each call —
// not an exhaustive delta test (network rates are covered by
// TestInstanceNetRates above), just confirming the method itself
// reflects updated metrics-server data on a second call.
func TestInstanceMetrics_ReflectsUpdatedUsageOnNextCall(t *testing.T) {
	pod := managedPod("inst-abc123", "inst-abc123-xy9z1", "node-1", 1000, 2*1024*1024*1024)
	c := NewDirectClient(fake.NewSimpleClientset(pod), nil, nil, "nvidia")
	c.metricsClient = newMetricsClientWithPods(t,
		podMetrics("inst-abc123-xy9z1", 100, 200*1024*1024),
	)

	got, err := c.InstanceMetrics(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("first call: got %d metrics, err %v", len(got), err)
	}
	if got[0].CPUUsedPercent < 9.9 || got[0].CPUUsedPercent > 10.1 {
		t.Errorf("first call: CPUUsedPercent = %v, want ~10", got[0].CPUUsedPercent)
	}

	// PodMetricsInterface is read-only (Get/List/Watch — metrics-server is
	// never written to by a client), so simulating "the next scrape
	// produced new numbers" means swapping in a freshly-seeded fake
	// rather than mutating the existing one.
	c.metricsClient = newMetricsClientWithPods(t,
		podMetrics("inst-abc123-xy9z1", 800, 1800*1024*1024),
	)

	got, err = c.InstanceMetrics(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("second call: got %d metrics, err %v", len(got), err)
	}
	if got[0].CPUUsedPercent < 79.9 || got[0].CPUUsedPercent > 80.1 {
		t.Errorf("second call: CPUUsedPercent = %v, want ~80", got[0].CPUUsedPercent)
	}
}
