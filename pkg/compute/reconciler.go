// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package compute

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// Reconciler keeps compute.instances in sync with the live cluster:
// status changes update the stored record, and instances that have
// disappeared are marked terminated so billing stops. This makes the
// database resilient to API-server restarts and out-of-band deletions.
type Reconciler struct {
	store    *Store
	cluster  cluster.Client
	interval time.Duration
}

// NewReconciler creates a reconciler over the given cluster client.
func NewReconciler(store *Store, clusterClient cluster.Client) *Reconciler {
	return &Reconciler{
		store:    store,
		cluster:  clusterClient,
		interval: time.Minute,
	}
}

// Start runs the reconcile loop until the context is cancelled.
func (r *Reconciler) Start(ctx context.Context) {
	log.Println("Starting instance reconciler")

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	// Reconcile immediately on startup to converge state that drifted
	// while the API server was down.
	if err := r.Reconcile(ctx); err != nil {
		log.Printf("Instance reconcile error: %v", err)
	}

	for {
		select {
		case <-ticker.C:
			if err := r.Reconcile(ctx); err != nil {
				log.Printf("Instance reconcile error: %v", err)
			}
		case <-ctx.Done():
			log.Println("Instance reconciler stopped")
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

	// AllTenants: the reconciler must see every customer's instances. An
	// instance it cannot see is one it keeps billing after the workload
	// has gone.
	statuses, err := r.cluster.ListInstanceStatuses(ctx, cluster.AllTenants())
	if err != nil {
		// CRITICAL: an unreachable cluster must never be read as "no
		// instances exist". The absence rule below marks missing
		// instances terminated, so treating a network failure as an empty
		// list would stop billing for every running instance on the
		// platform and tell every customer their workloads had vanished.
		//
		// Returning here leaves the database untouched until the cluster
		// answers again. Stale state is recoverable; mass false
		// termination is not.
		if errors.Is(err, cluster.ErrClusterUnavailable) {
			log.Printf("Reconcile skipped: cluster unreachable (%v) - instance state left unchanged", err)
			return nil
		}
		return fmt.Errorf("failed to list cluster instances: %w", err)
	}

	live := make(map[string]cluster.InstanceStatus, len(statuses))
	for _, st := range statuses {
		if st.InstanceID != "" {
			live[st.InstanceID] = st
		}
	}

	for i := range instances {
		inst := &instances[i]
		observed, exists := live[inst.ID]

		if !exists {
			// The workload is gone (deleted out-of-band, evicted, or the
			// node rebooted): stop billing.
			log.Printf("Instance %s is no longer in the cluster - marking terminated", inst.ID)
			if err := r.store.MarkTerminated(ctx, inst.ID); err != nil {
				log.Printf("Failed to terminate %s: %v", inst.ID, err)
			}
			continue
		}

		// Endpoint fields are checked independently of status: the
		// TLS-ready flip (cert-manager finishing issuance 30-90s after
		// create) happens with status unchanged the whole time
		// ("running" before and after). A status-only comparison would
		// skip this instance every single pass and the certificate
		// becoming ready would never reach the database (Stage 3 plan A6).
		endpointChanged := observed.EndpointURL != inst.Endpoint ||
			observed.DNSName != inst.DNSName ||
			observed.PublicIP != inst.PublicIP ||
			observed.TLSEnabled != inst.TLSEnabled ||
			observed.TLSReady != inst.TLSReady

		if observed.Status == inst.Status && !endpointChanged {
			continue
		}

		if observed.Status != inst.Status {
			log.Printf("Instance %s: %s -> %s", inst.ID, inst.Status, observed.Status)
		}
		if observed.Status == StatusTerminated {
			// Completed workloads must get terminated_at stamped so
			// billing stops; a plain status update would not do that.
			if err := r.store.MarkTerminated(ctx, inst.ID); err != nil {
				log.Printf("Failed to terminate %s: %v", inst.ID, err)
			}
			continue
		}
		if observed.Status != inst.Status {
			if err := r.store.UpdateStatus(ctx, inst.ID, observed.Status); err != nil {
				log.Printf("Failed to update %s: %v", inst.ID, err)
			}
		}
		if endpointChanged {
			if err := r.store.UpdateEndpoint(ctx, inst.ID, EndpointFields{
				Endpoint:   observed.EndpointURL,
				DNSName:    observed.DNSName,
				PublicIP:   observed.PublicIP,
				TLSEnabled: observed.TLSEnabled,
				TLSReady:   observed.TLSReady,
			}); err != nil {
				log.Printf("Failed to update endpoint for %s: %v", inst.ID, err)
			}
		}
	}

	return nil
}
