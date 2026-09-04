// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// instanceIOSample is the previous sweep's cumulative network byte
// counters for one instance, plus when they were read — needed to turn
// /stats/summary's cumulative counters into a wall-clock RATE, same
// technique as pkg/agentrunner/hostmetrics.go uses for the host itself.
// Kept per-instance (not per-pod-UID) so a redeployed pod's first
// reading after replacement is treated as a fresh baseline rather than
// either an artificial reset-to-zero spike or a lookup that silently
// never matches again.
type instanceIOSample struct {
	rxBytes, txBytes uint64
	at               time.Time
}

// podUsage is one pod's summed CPU/memory usage across its containers,
// from metrics.k8s.io.
type podUsage struct {
	cpuMilli int64
	memBytes int64
}

// netCounters is one pod's cumulative network byte counters from
// /stats/summary, for one sweep.
type netCounters struct {
	rxBytes, txBytes uint64
}

// InstanceMetrics implements cluster.Client — see that interface method's
// own doc comment for the overall contract (partial results, no data
// rather than zero-filled entries).
//
// Three independent data sources feed this, and a failure in any ONE
// must not blank out the others: CPU/memory absence still lets network/
// storage report, and vice versa — the same "best-effort per field"
// posture pkg/agentrunner/hostmetrics.go established for host metrics.
func (c *DirectClient) InstanceMetrics(ctx context.Context) ([]InstanceMetric, error) {
	if c.metricsClient == nil {
		// No REST config was ever supplied (WithRESTConfig never called,
		// or it failed) — the feature is simply off, not an error: every
		// OTHER capability of this client works fine without it.
		return nil, nil
	}

	pods, err := c.k8s.CoreV1().Pods(workloadNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedSelector(AllTenants()),
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	type podInfo struct {
		nodeName      string
		podName       string
		cpuLimitMilli int64
	}
	infoByInstance := make(map[string]podInfo, len(pods.Items))
	nodeNames := make(map[string]bool)

	for i := range pods.Items {
		pod := &pods.Items[i]
		instanceID := pod.Labels[labelInstanceID]
		if instanceID == "" || pod.Spec.NodeName == "" {
			continue
		}
		var limitMilli int64
		for _, ctr := range pod.Spec.Containers {
			if q, ok := ctr.Resources.Limits[corev1.ResourceCPU]; ok {
				limitMilli += q.MilliValue()
			}
		}
		infoByInstance[instanceID] = podInfo{
			nodeName:      pod.Spec.NodeName,
			podName:       pod.Name,
			cpuLimitMilli: limitMilli,
		}
		nodeNames[pod.Spec.NodeName] = true
	}
	if len(infoByInstance) == 0 {
		return nil, nil
	}

	// CPU/memory: one List call covers every pod in the namespace.
	usageByPod := make(map[string]podUsage, len(infoByInstance))
	podMetricsList, err := c.metricsClient.MetricsV1beta1().PodMetricses(workloadNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("WARN: metrics.k8s.io unavailable, CPU/memory instance metrics skipped this sweep: %v", err)
	} else {
		for _, pm := range podMetricsList.Items {
			var u podUsage
			for _, ctr := range pm.Containers {
				if q, ok := ctr.Usage[corev1.ResourceCPU]; ok {
					u.cpuMilli += q.MilliValue()
				}
				if q, ok := ctr.Usage[corev1.ResourceMemory]; ok {
					u.memBytes += q.Value()
				}
			}
			usageByPod[pm.Name] = u
		}
	}

	// Network/storage: one /stats/summary call per DISTINCT node, not per
	// pod — a home node has exactly one, a datacenter node's pods share
	// theirs, so this never fans out faster than the node count.
	netByPod := make(map[string]netCounters, len(infoByInstance))
	storageGBByPod := make(map[string]float64, len(infoByInstance))
	for node := range nodeNames {
		summary, err := c.fetchNodeStatsSummary(ctx, node)
		if err != nil {
			log.Printf("WARN: kubelet stats/summary unavailable for node %s, network/storage instance metrics skipped this sweep: %v", node, err)
			continue
		}
		for _, p := range summary.Pods {
			if p.PodRef.Namespace != workloadNamespace {
				continue
			}
			if p.Network != nil && p.Network.RxBytes != nil && p.Network.TxBytes != nil {
				netByPod[p.PodRef.Name] = netCounters{rxBytes: *p.Network.RxBytes, txBytes: *p.Network.TxBytes}
			}
			if p.EphemeralStorage != nil && p.EphemeralStorage.UsedBytes != nil {
				storageGBByPod[p.PodRef.Name] = float64(*p.EphemeralStorage.UsedBytes) / (1024 * 1024 * 1024)
			}
		}
	}

	now := time.Now()
	c.lastInstanceIOMu.Lock()
	defer c.lastInstanceIOMu.Unlock()
	if c.lastInstanceIO == nil {
		c.lastInstanceIO = make(map[string]instanceIOSample)
	}

	results := make([]InstanceMetric, 0, len(infoByInstance))
	seen := make(map[string]bool, len(infoByInstance))
	for instanceID, info := range infoByInstance {
		seen[instanceID] = true
		m := InstanceMetric{InstanceID: instanceID}
		haveAny := false

		if u, ok := usageByPod[info.podName]; ok && info.cpuLimitMilli > 0 {
			m.CPUUsedPercent = float64(u.cpuMilli) / float64(info.cpuLimitMilli) * 100
			m.MemoryUsedGB = float64(u.memBytes) / (1024 * 1024 * 1024)
			haveAny = true
		}

		if cur, ok := netByPod[info.podName]; ok {
			if prev, havePrev := c.lastInstanceIO[instanceID]; havePrev {
				if rx, tx, ok := instanceNetRates(prev, cur, now); ok {
					m.NetworkRxMbps, m.NetworkTxMbps = rx, tx
					haveAny = true
				}
			}
			c.lastInstanceIO[instanceID] = instanceIOSample{rxBytes: cur.rxBytes, txBytes: cur.txBytes, at: now}
		}

		if gb, ok := storageGBByPod[info.podName]; ok {
			m.StorageUsedGB = gb
			haveAny = true
		}

		if haveAny {
			results = append(results, m)
		}
	}

	// Prune cache entries for instances no longer present (deleted or
	// moved off this node) — an unbounded map here would leak one entry
	// per instance ever seen, for the lifetime of the agent process.
	for id := range c.lastInstanceIO {
		if !seen[id] {
			delete(c.lastInstanceIO, id)
		}
	}

	return results, nil
}

// instanceNetRates computes MB/s from two /stats/summary samples and the
// wall-clock time between them. ok=false on a non-positive elapsed time
// or a counter that went backwards (pod restart resets cAdvisor's
// counters to zero) — see hostmetrics.go's netRatesFromSamples for the
// identical reasoning at the host level.
func instanceNetRates(prev instanceIOSample, cur netCounters, now time.Time) (rxMBps, txMBps float64, ok bool) {
	elapsed := now.Sub(prev.at)
	if elapsed <= 0 || cur.rxBytes < prev.rxBytes || cur.txBytes < prev.txBytes {
		return 0, 0, false
	}
	secs := elapsed.Seconds()
	const mb = 1024.0 * 1024.0
	return float64(cur.rxBytes-prev.rxBytes) / mb / secs,
		float64(cur.txBytes-prev.txBytes) / mb / secs,
		true
}

// statsSummary is the small subset of the kubelet's /stats/summary
// response (k8s.io/kubelet/pkg/apis/stats/v1alpha1.Summary) this package
// actually reads. Hand-declared rather than importing k8s.io/kubelet for
// the full (much larger) type: the response shape is a stable, versioned
// kubelet API, and this file only ever reads three leaf fields.
type statsSummary struct {
	Pods []struct {
		PodRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"podRef"`
		Network *struct {
			RxBytes *uint64 `json:"rxBytes"`
			TxBytes *uint64 `json:"txBytes"`
		} `json:"network"`
		EphemeralStorage *struct {
			UsedBytes *uint64 `json:"usedBytes"`
		} `json:"ephemeral-storage"`
	} `json:"pods"`
}

// fetchNodeStatsSummary reads one node's stats/summary through the API
// server's node proxy — NOT a direct connection to the kubelet's own
// port, so this needs no separate credential or network path beyond the
// same rest.Config every other DirectClient call already authenticates
// with.
//
// Recovers from a panic inside the client-go call chain (rather than
// only handling a returned error): a k8s.io/client-go fake/incomplete
// Clientset's RESTClient() can return nil, which client-go's own
// rest.NewRequest panics on rather than erroring — observed directly in
// this package's own tests. Network/storage instance metrics are the
// kind of secondary, best-effort data this whole file already treats as
// "skip this sweep, do not fail the call" for an ordinary error; a panic
// from the same code path deserves the identical treatment, not a crash
// of the agent process over one metric.
func (c *DirectClient) fetchNodeStatsSummary(ctx context.Context, nodeName string) (result *statsSummary, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = nil, fmt.Errorf("stats/summary proxy call panicked: %v", r)
		}
	}()

	raw, err := c.k8s.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("/api/v1/nodes/%s/proxy/stats/summary", nodeName)).
		DoRaw(ctx)
	if err != nil {
		return nil, err
	}
	var summary statsSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, fmt.Errorf("decode stats/summary: %w", err)
	}
	return &summary, nil
}
