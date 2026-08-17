// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nodes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
}

// Placement is the resolved target for a home workload.
type Placement struct {
	NodeName   string
	ProviderID string
	Arch       string
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

	// Least-loaded arch-matched node whose FREE rentable capacity covers the
	// request. Free = rentable - used, where used sums cpu_units/memory_gb
	// over the node's active instances. Least-loaded = fewest active
	// instances, so work spreads.
	row := s.db.QueryRowContext(ctx, `
		SELECT n.node_name, n.provider_id, COALESCE(n.arch,'')
		FROM compute.nodes n
		LEFT JOIN (
			SELECT node_id,
			       COUNT(*) AS active,
			       SUM(COALESCE(cpu_units,0)) AS used_cpu,
			       SUM(COALESCE(memory_gb,0)) AS used_mem
			FROM compute.instances
			WHERE node_id IS NOT NULL AND terminated_at IS NULL
			GROUP BY node_id
		) load ON load.node_id = n.id
		WHERE n.class = 'home' AND n.status = 'online' AND n.k8s_ready = TRUE AND n.revoked_at IS NULL
		  AND ($2 = FALSE OR n.arch = $1)
		  AND n.rentable_cpu_cores - COALESCE(load.used_cpu, 0) >= $3
		  AND n.rentable_memory_gb - COALESCE(load.used_mem, 0) >= $4
		ORDER BY COALESCE(load.active, 0) ASC, n.last_seen_at DESC
		LIMIT 1
	`, req.Arch, req.Arch != "", req.CPUUnits, req.MemoryGB)

	var p Placement
	err := row.Scan(&p.NodeName, &p.ProviderID, &p.Arch)
	if err == sql.ErrNoRows {
		// Arch-matched nodes exist (checked above) but none has room for
		// this size — a capacity problem, distinct from arch or no-nodes.
		return nil, ErrInsufficientCapacity
	}
	if err != nil {
		return nil, fmt.Errorf("failed to place home workload: %w", err)
	}
	return &p, nil
}
