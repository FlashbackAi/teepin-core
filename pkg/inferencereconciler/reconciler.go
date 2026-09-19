// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package inferencereconciler turns a compute.node_services row's desired
// state into reality on Linux/CUDA nodes — the piece that makes a Control
// Center "mount" actually start a real vLLM/vllm-omni instance, rather
// than just recording that one was asked for. MLX rows on a native Mac node
// (teepin-modeld) go through the same reconciler via cluster.Client; it only
// ever touches kind=inference_model rows whose engine it recognises.
//
// Deliberately reuses cluster.Client wholesale rather than talking to
// Kubernetes or a home node's agent directly: that interface already
// abstracts direct-vs-tunneled dispatch identically (see pkg/cluster's own
// doc comment), so this package never needs to know which node class it's
// talking to.
package inferencereconciler

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/inferencegateway"
	"github.com/FlashbackAi/teepin-core/pkg/nodes"
	"github.com/FlashbackAi/teepin-core/pkg/nodeservices"
)

// EngineConfig configures how each engine is run (container image, or host command for MLX) — exported
// so main.go can supply real, deployed image references rather than this
// package guessing at one.
type EngineConfig = engineConfig

// Reconciler converges compute.node_services (kind=inference_model)
// against real cluster.Client instances, one pass at a time.
type Reconciler struct {
	nodeSvcs *nodeservices.Service
	nodes    *nodes.Service
	cluster  cluster.Client
	images   EngineConfig
}

// New constructs a Reconciler. images.VLLMImage/VLLMOmniImage left blank disables
// that engine — a mount requesting it fails clearly (see buildInstanceSpec)
// rather than being silently accepted and never started.
func New(nodeSvcs *nodeservices.Service, nodesSvc *nodes.Service, clusterClient cluster.Client, images EngineConfig) *Reconciler {
	return &Reconciler{nodeSvcs: nodeSvcs, nodes: nodesSvc, cluster: clusterClient, images: images}
}

// Reconcile runs one convergence pass over every inference_model
// node_services row. Safe to call on a timer — each row is independent,
// and one row's failure is reported against that row alone rather than
// aborting the pass for every other mount.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	rows, err := r.nodeSvcs.ListByKind(ctx, nodeservices.KindInferenceModel)
	if err != nil {
		return fmt.Errorf("list inference_model node services: %w", err)
	}

	nodesByID, err := r.nodeIndex(ctx)
	if err != nil {
		return fmt.Errorf("index nodes: %w", err)
	}

	p := &pass{rows: rows, released: make(map[uuid.UUID]bool)}

	// Unmounts first, so memory they hold is free before any mount is
	// considered in the same pass.
	ordered := make([]nodeservices.NodeService, 0, len(rows))
	for _, row := range rows {
		if row.DesiredState == nodeservices.DesiredUnmounted {
			ordered = append(ordered, row)
		}
	}
	for _, row := range rows {
		if row.DesiredState != nodeservices.DesiredUnmounted {
			ordered = append(ordered, row)
		}
	}

	var errs []error
	for _, row := range ordered {
		if p.released[row.ID] {
			continue // evicted earlier in this pass
		}
		if err := r.reconcileOne(ctx, row, nodesByID, p); err != nil {
			errs = append(errs, fmt.Errorf("node service %s: %w", row.ID, err))
		}
	}
	return errors.Join(errs...)
}

// nodeIndex loads every node once per pass and indexes by ID. Mirrors the
// console's own "no GET-by-id endpoint — the fleet is small enough to list
// and filter in memory" convention (controlcenter/nodes/[id]/page.tsx)
// rather than adding a new single-node lookup to pkg/nodes.
func (r *Reconciler) nodeIndex(ctx context.Context) (map[uuid.UUID]nodes.Node, error) {
	all, err := r.nodes.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	idx := make(map[uuid.UUID]nodes.Node, len(all))
	for _, n := range all {
		idx[n.ID] = n
	}
	return idx, nil
}

func (r *Reconciler) reconcileOne(ctx context.Context, row nodeservices.NodeService, nodesByID map[uuid.UUID]nodes.Node, p *pass) error {
	switch {
	case row.DesiredState == nodeservices.DesiredMounted && row.ObservedState == nodeservices.ObservedMounted:
		return r.verify(ctx, row, nodesByID, p)
	case row.DesiredState == nodeservices.DesiredMounted:
		return r.mount(ctx, row, nodesByID, p)
	case row.DesiredState == nodeservices.DesiredUnmounted && row.ObservedState != nodeservices.ObservedUnmounted:
		return r.unmount(ctx, row)
	default:
		return nil // already converged
	}
}

// verify is the drift check for a row already recorded as mounted: the
// instance can die after that report (host reboot, OOM, crashed engine),
// and nothing else would ever notice. A vanished or failed instance is
// remounted; a status-lookup error is returned without touching the row,
// so a flaky tunnel cannot flap a healthy mount.
func (r *Reconciler) verify(ctx context.Context, row nodeservices.NodeService, nodesByID map[uuid.UUID]nodes.Node, p *pass) error {
	st, err := r.cluster.GetInstanceStatus(ctx, cluster.AllTenants(), instanceIDFor(row.ID.String()))
	switch {
	case errors.Is(err, cluster.ErrNotFound):
		log.Printf("inferencereconciler: mounted %s has no instance; remounting", row.ID)
		return r.mount(ctx, row, nodesByID, p)
	case err != nil:
		return fmt.Errorf("check instance status: %w", err)
	case st.Status == statusFailed || st.Status == statusTerminated:
		log.Printf("inferencereconciler: mounted %s instance is %s (%s); remounting", row.ID, st.Status, st.Message)
		return r.mount(ctx, row, nodesByID, p)
	default:
		return nil
	}
}

// mount drives one row toward a running instance. It is re-entered every
// pass until the instance is actually running, and only then records
// Mounted — a row must never advertise an endpoint the router would
// dispatch to before the engine can answer.
func (r *Reconciler) mount(ctx context.Context, row nodeservices.NodeService, nodesByID map[uuid.UUID]nodes.Node, p *pass) error {
	cfg, err := inferencegateway.ParseModelServiceConfig(row.Config)
	if err != nil {
		return r.fail(ctx, row.ID, fmt.Errorf("parse config: %w", err))
	}

	node, ok := nodesByID[row.NodeID]
	if !ok {
		return r.fail(ctx, row.ID, fmt.Errorf("node %s not found", row.NodeID))
	}

	instanceID := instanceIDFor(row.ID.String())

	st, err := r.cluster.GetInstanceStatus(ctx, cluster.AllTenants(), instanceID)
	switch {
	case err == nil:
		switch st.Status {
		case statusRunning:
			return r.reportRunning(ctx, row, node, instanceID)
		case statusFailed:
			// A crash: clear the dead instance so the next pass starts a
			// fresh one, and surface why this one died.
			if delErr := r.cluster.DeleteInstance(ctx, cluster.AllTenants(), instanceID); delErr != nil {
				log.Printf("inferencereconciler: cleanup of failed instance %s failed: %v", instanceID, delErr)
			}
			return r.fail(ctx, row.ID, fmt.Errorf("instance failed: %s", st.Message))
		case statusTerminated:
			// Gone, not crashed (an agent restart, a reboot): drop the stale
			// record and fall through to start it again in this same pass.
			if delErr := r.cluster.DeleteInstance(ctx, cluster.AllTenants(), instanceID); delErr != nil {
				log.Printf("inferencereconciler: cleanup of terminated instance %s failed: %v", instanceID, delErr)
			}
		default:
			return r.nodeSvcs.ReportObserved(ctx, row.ID, nodeservices.ObservedPending, nil, nil)
		}
	case !errors.Is(err, cluster.ErrNotFound):
		return fmt.Errorf("check instance status: %w", err)
	}

	if err := r.admit(ctx, row, cfg, node, p); err != nil {
		return r.fail(ctx, row.ID, err)
	}

	spec, err := buildInstanceSpec(instanceID, cfg, r.images, node.NodeName, node.ProviderID, node.Class)
	if err != nil {
		return r.fail(ctx, row.ID, err)
	}
	if _, err := r.cluster.CreateInstance(ctx, spec); err != nil {
		return r.fail(ctx, row.ID, err)
	}
	log.Printf("inferencereconciler: started %s on node %s (instance=%s)", row.ID, node.NodeName, instanceID)
	return r.nodeSvcs.ReportObserved(ctx, row.ID, nodeservices.ObservedPending, nil, nil)
}

// reportRunning records Mounted with the endpoint the gateway will dial.
func (r *Reconciler) reportRunning(ctx context.Context, row nodeservices.NodeService, node nodes.Node, instanceID string) error {
	endpoint, err := r.endpointFor(ctx, node, instanceID)
	if err != nil {
		// Running but not yet addressable (e.g. pod has no IP): stay pending.
		log.Printf("inferencereconciler: %s running but not addressable yet: %v", row.ID, err)
		return r.nodeSvcs.ReportObserved(ctx, row.ID, nodeservices.ObservedPending, nil, nil)
	}
	log.Printf("inferencereconciler: mounted %s on node %s (instance=%s, endpoint=%s)", row.ID, node.NodeName, instanceID, endpoint)
	return r.nodeSvcs.ReportObserved(ctx, row.ID, nodeservices.ObservedMounted, nil, &endpoint)
}

// endpointFor resolves where the gateway reaches the instance. A home node
// sits behind NAT, so it is addressed by tunnel scheme (the gateway's
// tunnel-backed transport resolves it); a datacenter pod has a real
// in-cluster address.
func (r *Reconciler) endpointFor(ctx context.Context, node nodes.Node, instanceID string) (string, error) {
	if node.Class == "home" {
		return inferencegateway.TunnelEndpoint(node.ProviderID, instanceID, servePort), nil
	}
	addr, err := r.cluster.ResolveInstanceAddress(ctx, instanceID, servePort)
	if err != nil {
		return "", err
	}
	return "http://" + addr, nil
}

func (r *Reconciler) unmount(ctx context.Context, row nodeservices.NodeService) error {
	instanceID := instanceIDFor(row.ID.String())
	if err := r.cluster.DeleteInstance(ctx, cluster.AllTenants(), instanceID); err != nil {
		return r.fail(ctx, row.ID, err)
	}
	log.Printf("inferencereconciler: unmounted %s (instance=%s)", row.ID, instanceID)
	return r.nodeSvcs.ReportObserved(ctx, row.ID, nodeservices.ObservedUnmounted, nil, nil)
}

// fail records the failure via ReportObserved and returns the original
// cause — a reporting failure is appended to the error rather than
// replacing it, so the real problem is never hidden behind a secondary
// one.
func (r *Reconciler) fail(ctx context.Context, id uuid.UUID, cause error) error {
	msg := cause.Error()
	if repErr := r.nodeSvcs.ReportObserved(ctx, id, nodeservices.ObservedError, &msg, nil); repErr != nil {
		return fmt.Errorf("%w (also failed to report observed state: %v)", cause, repErr)
	}
	return cause
}

// Instance lifecycle strings as reported by cluster.InstanceStatus.Status.
const (
	statusRunning    = "running"
	statusFailed     = "failed"
	statusTerminated = "terminated"
)
