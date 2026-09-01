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

// reviveWindow bounds how far back Reconcile looks for a terminated
// instance worth reviving — see ListRecentlyTerminated's own doc comment
// for why this is bounded rather than scanning every terminated instance
// ever.
const reviveWindow = time.Hour

// Reconciler keeps compute.instances in sync with the live cluster:
// status changes update the stored record, instances that have
// disappeared are marked terminated so billing stops, and — the other
// direction — a recently-terminated instance whose workload turns out to
// still be reporting a live, healthy status gets revived. Bidirectional
// by deliberate design, not just the disappearance half: found live
// 2026-09-01, a redeploy's pod replacement took long enough that the
// reconciler concluded the instance was gone and marked it terminated,
// then the new pod came up healthy seconds later — and because
// ListActive's own WHERE clause permanently excludes a terminated row,
// nothing would ever have noticed on its own; the instance would have
// stayed marked terminated (customer-facing edge returning "not
// reachable", billing stopped) despite being genuinely up, until an
// unrelated NEW deploy action happened to revive it as a side effect
// (Store.UpdateImage). This makes the database resilient to API-server
// restarts and out-of-band deletions in both directions, not just one.
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

	recentlyTerminated, err := r.store.ListRecentlyTerminated(ctx, reviveWindow)
	if err != nil {
		return fmt.Errorf("failed to list recently terminated instances: %w", err)
	}

	if len(instances) == 0 && len(recentlyTerminated) == 0 {
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
		//
		// An observed endpoint that is entirely empty is never treated as
		// a change when the database already has one: AgentClient.
		// RecordStatus preserves a cached endpoint across an all-empty
		// wire report (see that method's own comment), but that
		// protection only applies once a cache entry already exists. A
		// cold cache — the control plane restarted, or this is the first
		// status this process has seen for the instance — has nothing to
		// preserve, so a legitimately-empty first report (e.g. a redeploy
		// whose port auto-detection failed transiently) would otherwise
		// reach here as a real "change" and get persisted, erasing a
		// working endpoint the instance already had. Found live
		// 2026-08-31 alongside the redeployKumbhaInstance port-detection
		// fix (kumbha_handlers.go) — this is the same failure one layer
		// further down, and closes it for every producer of
		// InstanceStatus, not just that one call site.
		observedEndpointEmpty := observed.EndpointURL == "" && observed.DNSName == "" &&
			observed.PublicIP == "" && !observed.TLSEnabled && !observed.TLSReady
		endpointChanged := !observedEndpointEmpty && (observed.EndpointURL != inst.Endpoint ||
			observed.DNSName != inst.DNSName ||
			observed.PublicIP != inst.PublicIP ||
			observed.TLSEnabled != inst.TLSEnabled ||
			observed.TLSReady != inst.TLSReady)

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

	// The other direction: a recently-terminated instance whose workload
	// is reporting a live status again gets revived — see this type's own
	// doc comment for the live incident this closes. Only "pending" or
	// "running" count as revival-worthy; a report of "failed" or
	// "terminated" for an already-terminated instance is not a reason to
	// bring it back; it means the cluster agrees it is gone, same as
	// before.
	for i := range recentlyTerminated {
		inst := &recentlyTerminated[i]
		observed, exists := live[inst.ID]
		if !exists || (observed.Status != StatusPending && observed.Status != StatusRunning) {
			continue
		}
		log.Printf("Instance %s: terminated -> %s (workload is reporting healthy again, reviving)", inst.ID, observed.Status)
		if err := r.store.Revive(ctx, inst.ID, observed.Status); err != nil {
			log.Printf("Failed to revive %s: %v", inst.ID, err)
		}
	}

	return nil
}
