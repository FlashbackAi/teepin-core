// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package compute

import (
	"context"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// labelInstanceID is the pod label carrying the TEEPIN instance ID.
const labelInstanceID = "app.teepin.cloud/instance-id"

// labelManaged identifies TEEPIN-managed pods.
const labelManaged = "app.teepin.cloud/managed"

// Reconciler keeps compute.instances in sync with the live cluster:
// pod phase changes update the stored status, and instances whose pod
// has disappeared are marked terminated so billing stops. This makes
// the database resilient to API-server restarts and out-of-band pod
// deletions.
type Reconciler struct {
	store     *Store
	k8sClient kubernetes.Interface
	namespace string
	interval  time.Duration
}

// NewReconciler creates a reconciler for the given namespace.
func NewReconciler(store *Store, k8sClient kubernetes.Interface, namespace string) *Reconciler {
	return &Reconciler{
		store:     store,
		k8sClient: k8sClient,
		namespace: namespace,
		interval:  time.Minute,
	}
}

// Start runs the reconcile loop until the context is cancelled.
func (r *Reconciler) Start(ctx context.Context) {
	log.Println("🔄 Starting instance reconciler...")

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	// Reconcile immediately on startup to converge state that drifted
	// while the API server was down.
	if err := r.Reconcile(ctx); err != nil {
		log.Printf("⚠️  Instance reconcile error: %v", err)
	}

	for {
		select {
		case <-ticker.C:
			if err := r.Reconcile(ctx); err != nil {
				log.Printf("⚠️  Instance reconcile error: %v", err)
			}
		case <-ctx.Done():
			log.Println("🔄 Instance reconciler stopped")
			return
		}
	}
}

// Reconcile performs one synchronization pass.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	instances, err := r.store.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("failed to list active instances: %w", err)
	}
	if len(instances) == 0 {
		return nil
	}

	pods, err := r.k8sClient.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelManaged + "=true",
	})
	if err != nil {
		return fmt.Errorf("failed to list managed pods: %w", err)
	}

	// Index the newest pod per instance ID.
	podByInstance := make(map[string]*corev1.Pod, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if id := pod.Labels[labelInstanceID]; id != "" {
			podByInstance[id] = pod
		}
	}

	for i := range instances {
		inst := &instances[i]
		pod, exists := podByInstance[inst.ID]

		if !exists {
			// Pod is gone (deleted out-of-band or by us): stop billing.
			log.Printf("🔄 Instance %s has no pod — marking terminated", inst.ID)
			if err := r.store.MarkTerminated(ctx, inst.ID); err != nil {
				log.Printf("⚠️  Failed to terminate %s: %v", inst.ID, err)
			}
			continue
		}

		status := podPhaseToStatus(pod.Status.Phase)
		if status == inst.Status {
			continue
		}

		log.Printf("🔄 Instance %s: %s → %s", inst.ID, inst.Status, status)
		if status == StatusTerminated {
			// Completed workloads must get terminated_at stamped so
			// billing stops; a plain status update would not do that.
			if err := r.store.MarkTerminated(ctx, inst.ID); err != nil {
				log.Printf("⚠️  Failed to terminate %s: %v", inst.ID, err)
			}
			continue
		}
		if err := r.store.UpdateStatus(ctx, inst.ID, status); err != nil {
			log.Printf("⚠️  Failed to update %s: %v", inst.ID, err)
		}
	}

	return nil
}

// podPhaseToStatus maps a Kubernetes pod phase to a TEEPIN instance status.
func podPhaseToStatus(phase corev1.PodPhase) string {
	switch phase {
	case corev1.PodRunning:
		return StatusRunning
	case corev1.PodPending:
		return StatusPending
	case corev1.PodFailed:
		return StatusFailed
	case corev1.PodSucceeded:
		// A completed batch workload no longer consumes resources.
		return StatusTerminated
	default:
		return StatusPending
	}
}
