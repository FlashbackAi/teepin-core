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
)

// PlacementReq describes what a home CPU workload needs. Deliberately small
// for the pilot: architecture is the one hard constraint (an amd64 image
// cannot run on an arm64 node's Linux). CPU/memory sizing is enforced later
// by the node's own scheduler once k3s is placing pods.
type PlacementReq struct {
	// Arch is the required CPU architecture ("amd64", "arm64"). Empty means
	// "no preference" — used by a single-arch pilot that does not thread
	// arch through yet.
	Arch string
}

// Placement is the resolved target for a home workload.
type Placement struct {
	NodeName   string
	ProviderID string
	Arch       string
}

// PlaceCPU selects a home node to run a CPU workload on. It considers only
// nodes that are class='home', status='online', and not disabled/revoked,
// and (when an arch is requested) whose arch matches. Among eligible nodes it
// picks the LEAST LOADED — fewest active instances — so work spreads rather
// than piling onto the first node.
//
// The arch check is split from the capacity check so the caller can tell a
// customer "your amd64 image has no arm64-free home node" (fixable) apart
// from "no home nodes are online right now" (retryable).
func (s *Service) PlaceCPU(ctx context.Context, req PlacementReq) (*Placement, error) {
	// First: is there ANY eligible home node, ignoring arch? This lets us
	// return ErrArchUnavailable (vs ErrNoHomeCapacity) precisely.
	var anyOnline int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM compute.nodes
		WHERE class = 'home' AND status = 'online' AND revoked_at IS NULL
	`).Scan(&anyOnline); err != nil {
		return nil, fmt.Errorf("failed to check home capacity: %w", err)
	}
	if anyOnline == 0 {
		return nil, ErrNoHomeCapacity
	}

	// Least-loaded eligible node. Active instances counted via node_id;
	// an instance is active when not terminated. NULLS handled by the
	// LEFT JOIN + COALESCE so a brand-new node (zero instances) sorts first.
	//
	// The arch filter is applied only when requested. $1 = requested arch,
	// $2 = whether to apply the filter.
	row := s.db.QueryRowContext(ctx, `
		SELECT n.node_name, n.provider_id, COALESCE(n.arch,'')
		FROM compute.nodes n
		LEFT JOIN (
			SELECT node_id, COUNT(*) AS active
			FROM compute.instances
			WHERE node_id IS NOT NULL AND terminated_at IS NULL
			GROUP BY node_id
		) load ON load.node_id = n.id
		WHERE n.class = 'home' AND n.status = 'online' AND n.revoked_at IS NULL
		  AND ($2 = FALSE OR n.arch = $1)
		ORDER BY COALESCE(load.active, 0) ASC, n.last_seen_at DESC
		LIMIT 1
	`, req.Arch, req.Arch != "")

	var p Placement
	err := row.Scan(&p.NodeName, &p.ProviderID, &p.Arch)
	if err == sql.ErrNoRows {
		// There were online home nodes (checked above) but none matched the
		// arch filter — so this is specifically an arch problem.
		if req.Arch != "" {
			return nil, ErrArchUnavailable
		}
		return nil, ErrNoHomeCapacity
	}
	if err != nil {
		return nil, fmt.Errorf("failed to place home workload: %w", err)
	}
	return &p, nil
}
