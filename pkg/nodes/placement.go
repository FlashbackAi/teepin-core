// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nodes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// Placement errors, mapped by the API to distinct HTTP statuses so a
// customer can tell "no capacity right now" (retry) from "nothing matches my
// architecture" (fix the request).
var (
	// ErrNoHomeCapacity: no online home node is available at all.
	ErrNoHomeCapacity = errors.New("no home compute capacity available")
	// ErrArchUnavailable: home capacity exists, but none matches the
	// requested CPU architecture — a request problem, not a transient one.
	ErrArchUnavailable = errors.New("no home node matches the requested architecture")
	// ErrInsufficientCapacity: arch-matched home nodes are online, but none
	// has enough FREE (rentable - used) capacity for the requested size — a
	// transient condition (capacity may free up), mapped to 503.
	ErrInsufficientCapacity = errors.New("no home node has enough free capacity for this size")
)

// PlacementReq describes what a home CPU workload needs: the architecture
// constraint (an amd64 image cannot run on an arm64 node's Linux) and the
// requested size, which must fit within a node's free rentable capacity.
type PlacementReq struct {
	// Arch is the required CPU architecture ("amd64", "arm64"). Empty means
	// "no preference" — used by a single-arch pilot that does not thread
	// arch through yet.
	Arch string
	// CPUUnits / MemoryGB are the requested size (from the chosen instance
	// tier). A node is eligible only if its free rentable capacity covers
	// both. Zero means "no size constraint" (the pre-2.5 behaviour).
	CPUUnits int
	MemoryGB int
	// PCores/ECores are an explicit P-core/E-core split the customer
	// requested. Both 0 (the default) means "no preference" — PlaceCPU
	// either falls back to the single undifferentiated CPUUnits path (on a
	// node with no detected split) or resolves a PROPORTIONAL default split
	// from what is actually free on the chosen node — see PlaceCPU's own
	// doc comment. A non-zero value is validated against a node's free
	// P/E capacity as part of node selection, same as CPUUnits/MemoryGB.
	PCores int
	ECores int
}

// Placement is the resolved target for a home workload.
type Placement struct {
	NodeName   string
	ProviderID string
	Arch       string
	// PCoresUsed/ECoresUsed are the ACTUAL P/E split this placement
	// resolved to. nil when the chosen node has no detected split (today's
	// single-scalar behaviour, unaffected); otherwise either the caller's
	// own explicit request (already capacity-checked) or a proportional
	// default. Callers persist these onto compute.instances'
	// p_cores_used/e_cores_used so billing can price them separately (see
	// pkg/billing/collector.go).
	PCoresUsed *int
	ECoresUsed *int
}

// PlaceCPU selects a home node to run a CPU workload on. It considers only
// nodes that are class='home', status='online', k8s_ready (the node's own
// Kubernetes was reachable as of its last report — "online" alone only means
// the agent's gRPC session is connected, not that it can execute anything),
// and not disabled/revoked, and (when an arch is requested) whose arch
// matches. Among eligible nodes it picks the LEAST LOADED — fewest active
// instances — so work spreads rather than piling onto the first node.
//
// The arch check is split from the capacity check so the caller can tell a
// customer "your amd64 image has no arm64-free home node" (fixable) apart
// from "no home nodes are online right now" (retryable).
func (s *Service) PlaceCPU(ctx context.Context, req PlacementReq) (*Placement, error) {
	// Count online home nodes, and among them how many match the requested
	// arch. This lets us distinguish the three failure modes precisely,
	// before considering fit.
	var online, archMatched int
	if err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE $2 = FALSE OR arch = $1)
		FROM compute.nodes
		WHERE class = 'home' AND status = 'online' AND k8s_ready = TRUE AND revoked_at IS NULL
	`, req.Arch, req.Arch != "").Scan(&online, &archMatched); err != nil {
		return nil, fmt.Errorf("failed to check home capacity: %w", err)
	}
	if online == 0 {
		return nil, ErrNoHomeCapacity
	}
	if archMatched == 0 {
		return nil, ErrArchUnavailable
	}

	// A customer-specified P/E split is an ADDITIONAL capacity constraint on
	// top of the combined rentable_cpu_cores check below — the ($9 = FALSE
	// OR ...) clause skips it entirely when no preference was given (the
	// pre-existing, undifferentiated behaviour), same pattern as arch's own
	// ($2 = FALSE OR ...) guard.
	wantSplit := req.PCores > 0 || req.ECores > 0

	// Least-loaded arch-matched node whose FREE rentable capacity covers the
	// request. Free = rentable - used, where used sums cpu_units/memory_gb
	// over the node's active instances. Least-loaded = fewest active
	// instances, so work spreads. Also returns the node's detected P/E
	// totals and used P/E sums, so the caller can resolve the actual split
	// (an explicit request, or a proportional default) without a second
	// query.
	row := s.db.QueryRowContext(ctx, `
		SELECT n.node_name, n.provider_id, COALESCE(n.arch,''),
		       COALESCE(n.p_cores,0), COALESCE(n.e_cores,0),
		       COALESCE(load.used_p,0), COALESCE(load.used_e,0)
		FROM compute.nodes n
		LEFT JOIN (
			SELECT node_id,
			       COUNT(*) AS active,
			       SUM(COALESCE(cpu_units,0)) AS used_cpu,
			       SUM(COALESCE(memory_gb,0)) AS used_mem,
			       SUM(COALESCE(p_cores_used,0)) AS used_p,
			       SUM(COALESCE(e_cores_used,0)) AS used_e
			FROM compute.instances
			WHERE node_id IS NOT NULL AND terminated_at IS NULL
			GROUP BY node_id
		) load ON load.node_id = n.id
		WHERE n.class = 'home' AND n.status = 'online' AND n.k8s_ready = TRUE AND n.revoked_at IS NULL
		  AND ($2 = FALSE OR n.arch = $1)
		  AND n.rentable_cpu_cores - COALESCE(load.used_cpu, 0) >= $3
		  AND n.rentable_memory_gb - COALESCE(load.used_mem, 0) >= $4
		  AND ($7 = FALSE OR (
		        COALESCE(n.p_cores,0) - COALESCE(load.used_p,0) >= $5
		    AND COALESCE(n.e_cores,0) - COALESCE(load.used_e,0) >= $6
		  ))
		ORDER BY COALESCE(load.active, 0) ASC, n.last_seen_at DESC
		LIMIT 1
	`, req.Arch, req.Arch != "", req.CPUUnits, req.MemoryGB, req.PCores, req.ECores, wantSplit)

	var p Placement
	var nodePCores, nodeECores, usedP, usedE int
	err := row.Scan(&p.NodeName, &p.ProviderID, &p.Arch, &nodePCores, &nodeECores, &usedP, &usedE)
	if err == sql.ErrNoRows {
		// Arch-matched nodes exist (checked above) but none has room for
		// this size (or, with an explicit P/E request, this specific
		// split) — a capacity problem, distinct from arch or no-nodes.
		return nil, ErrInsufficientCapacity
	}
	if err != nil {
		return nil, fmt.Errorf("failed to place home workload: %w", err)
	}

	switch {
	case nodePCores == 0 && nodeECores == 0:
		// No split detected on this node — undifferentiated path, exactly
		// as before this feature. PCoresUsed/ECoresUsed stay nil.
	case wantSplit:
		// Already capacity-checked against this node in the WHERE clause.
		pCores, eCores := req.PCores, req.ECores
		p.PCoresUsed, p.ECoresUsed = &pCores, &eCores
	default:
		// No customer preference, but the chosen node DOES have a detected
		// split: allocate PROPORTIONALLY to what is actually free right
		// now — never a fixed 50/50 (see ROADMAP.md's own reasoning for
		// this policy).
		pUsed, eUsed := proportionalSplit(req.CPUUnits, max0(nodePCores-usedP), max0(nodeECores-usedE))
		p.PCoresUsed, p.ECoresUsed = &pUsed, &eUsed
	}
	return &p, nil
}

// proportionalSplit divides `total` CPU units between P-cores and E-cores in
// proportion to what is actually free of each (freeP, freeE) on the chosen
// node, rounding the P-core share and giving E-cores the exact remainder so
// the two always sum to `total`. Callers only invoke this once a node with
// freeP+freeE > 0 has already been selected (PlaceCPU only reaches here
// when the node reports a detected split), but the zero case is still
// guarded defensively rather than assumed.
func proportionalSplit(total, freeP, freeE int) (pUsed, eUsed int) {
	if freeP+freeE <= 0 {
		return 0, total
	}
	pUsed = int(math.Round(float64(total) * float64(freeP) / float64(freeP+freeE)))
	if pUsed > total {
		pUsed = total
	}
	if pUsed < 0 {
		pUsed = 0
	}
	return pUsed, total - pUsed
}
