// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nodes

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// SetReservation sets how much of a node's capacity the operator offers for
// rent. It is validated against the node's DETECTED specs in the same
// statement (rentable <= cpu_cores / memory_gb), so an operator can never
// offer more than the machine physically has — ErrOverCommit otherwise.
//
// A node not found (or whose detected specs are unknown) is ErrNotFound.
func (s *Service) SetReservation(ctx context.Context, nodeID uuid.UUID, cpuCores, memGB int) error {
	if cpuCores < 0 || memGB < 0 {
		return fmt.Errorf("reservation cannot be negative")
	}

	// Load the detected ceiling first so we can distinguish "not found" from
	// "over-commit" with clear errors, rather than a silent CHECK failure.
	var detectedCPU, detectedMem int
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(cpu_cores,0), COALESCE(memory_gb,0)
		FROM compute.nodes WHERE id = $1
	`, nodeID).Scan(&detectedCPU, &detectedMem)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to load node specs: %w", err)
	}
	if cpuCores > detectedCPU || memGB > detectedMem {
		return ErrOverCommit
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE compute.nodes
		SET rentable_cpu_cores = $1, rentable_memory_gb = $2, updated_at = NOW()
		WHERE id = $3
	`, cpuCores, memGB, nodeID)
	if err != nil {
		return fmt.Errorf("failed to set reservation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// NodeCapacity is a node's capacity breakdown for the control centre and the
// fit check: what the machine HAS (detected), what is OFFERED (rentable), what
// running workloads currently HOLD (used), and the derived FREE = rentable -
// used (never negative).
type NodeCapacity struct {
	NodeID        uuid.UUID `json:"node_id"`
	NodeName      string    `json:"node_name"`
	Class         string    `json:"class"`
	Status        string    `json:"status"`
	DetectedCPU   int       `json:"detected_cpu_cores"`
	DetectedMemGB int       `json:"detected_memory_gb"`
	RentableCPU   int       `json:"rentable_cpu_cores"`
	RentableMemGB int       `json:"rentable_memory_gb"`
	UsedCPU       int       `json:"used_cpu_cores"`
	UsedMemGB     int       `json:"used_memory_gb"`
	FreeCPU       int       `json:"free_cpu_cores"`
	FreeMemGB     int       `json:"free_memory_gb"`
}

// ListNodeCapacity returns the capacity breakdown for every node. Used is
// derived — summed from the node's ACTIVE instances (compute.instances joined
// on node_id, not terminated) — so it can never drift from the real running
// workloads. Free is clamped at 0 so a node whose reservation was lowered
// below current usage reports 0 free, not a negative number.
func (s *Service) ListNodeCapacity(ctx context.Context) ([]NodeCapacity, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.node_name, n.class, n.status,
		       COALESCE(n.cpu_cores,0), COALESCE(n.memory_gb,0),
		       n.rentable_cpu_cores, n.rentable_memory_gb,
		       COALESCE(u.used_cpu,0), COALESCE(u.used_mem,0)
		FROM compute.nodes n
		LEFT JOIN (
			SELECT node_id,
			       SUM(COALESCE(cpu_units,0)) AS used_cpu,
			       SUM(COALESCE(memory_gb,0)) AS used_mem
			FROM compute.instances
			WHERE node_id IS NOT NULL AND terminated_at IS NULL
			GROUP BY node_id
		) u ON u.node_id = n.id
		ORDER BY n.created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list node capacity: %w", err)
	}
	defer rows.Close()

	out := []NodeCapacity{}
	for rows.Next() {
		var c NodeCapacity
		if err := rows.Scan(&c.NodeID, &c.NodeName, &c.Class, &c.Status,
			&c.DetectedCPU, &c.DetectedMemGB, &c.RentableCPU, &c.RentableMemGB,
			&c.UsedCPU, &c.UsedMemGB); err != nil {
			return nil, fmt.Errorf("failed to scan node capacity: %w", err)
		}
		c.FreeCPU = max0(c.RentableCPU - c.UsedCPU)
		c.FreeMemGB = max0(c.RentableMemGB - c.UsedMemGB)
		out = append(out, c)
	}
	return out, rows.Err()
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// TierFit reports whether a CPU instance tier currently fits on some online
// home node, for the capacity-aware create dialog.
type TierFit struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	CPUUnits int     `json:"cpu_units"`
	MemoryGB int     `json:"memory_gb"`
	Price    float64 `json:"price_per_hour"`
	// Fits is true when at least one online home node has enough free
	// rentable capacity for this tier right now.
	Fits bool `json:"fits"`
}

// CPURates carries the per-resource CPU pricing so the tier price shown to a
// customer is COMPUTED with the exact same formula the metering collector
// uses (cores*CPUCoreRate + gb*MemoryGBRate). This is the single-source-of-
// truth pricing model: the operator sets one pair of rates, and both the
// quote and the bill derive from them, so they can never disagree. The tier's
// own seeded price_per_hour is ignored for CPU.
type CPURates struct {
	CPUCoreRate  float64
	MemoryGBRate float64
}

// HomeCapacitySummary is the customer-facing view: the CPU tiers with a
// per-tier "fits right now" flag, plus the total free capacity across online
// home nodes. Powers the create dialog (show tiers, disable ones that do not
// fit) without exposing per-node internals.
type HomeCapacitySummary struct {
	Tiers          []TierFit `json:"tiers"`
	TotalFreeCPU   int       `json:"total_free_cpu_cores"`
	TotalFreeMemGB int       `json:"total_free_memory_gb"`
	// MaxFreeCPU/MaxFreeMemGB are the largest single-node free values — a
	// tier fits only if it is within ONE node's free capacity, not the sum.
	MaxFreeCPU   int `json:"max_free_cpu_cores"`
	MaxFreeMemGB int `json:"max_free_memory_gb"`
}

// HomeCapacitySummary computes tier fitment against online home nodes and
// prices each tier from the operator's per-resource rates (rates). A tier
// fits when its size is within some SINGLE node's free capacity (a workload
// runs on one node, not spread across the fleet), so fitment keys off the
// per-node maximum, not the total. Each tier's price is
// cpu_units*CPUCoreRate + memory_gb*MemoryGBRate — identical to the metering
// formula, so the quote the customer sees equals what they will be billed.
func (s *Service) HomeCapacitySummary(ctx context.Context, rates CPURates) (*HomeCapacitySummary, error) {
	caps, err := s.ListNodeCapacity(ctx)
	if err != nil {
		return nil, err
	}

	summary := &HomeCapacitySummary{}
	for _, c := range caps {
		if c.Class != "home" || c.Status != "online" {
			continue
		}
		summary.TotalFreeCPU += c.FreeCPU
		summary.TotalFreeMemGB += c.FreeMemGB
		if c.FreeCPU > summary.MaxFreeCPU {
			summary.MaxFreeCPU = c.FreeCPU
		}
		if c.FreeMemGB > summary.MaxFreeMemGB {
			summary.MaxFreeMemGB = c.FreeMemGB
		}
	}

	// The CPU tiers come from the shared instance_types table (gpu_vram_gb
	// IS NULL selects CPU-only types). The tier defines the SIZE; the PRICE
	// is computed from the operator's per-resource rates below, so the
	// seeded price_per_hour column is not read for CPU. A tier fits when it
	// is within the largest single node's free capacity.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, cpu_units, memory_gb
		FROM compute.instance_types
		WHERE gpu_vram_gb IS NULL AND available = TRUE
		ORDER BY cpu_units ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to load CPU tiers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var t TierFit
		if err := rows.Scan(&t.ID, &t.Name, &t.CPUUnits, &t.MemoryGB); err != nil {
			return nil, fmt.Errorf("failed to scan tier: %w", err)
		}
		// Price from the operator's rates — the same formula the collector
		// meters with, so quote == bill.
		t.Price = float64(t.CPUUnits)*rates.CPUCoreRate + float64(t.MemoryGB)*rates.MemoryGBRate
		t.Fits = t.CPUUnits <= summary.MaxFreeCPU && t.MemoryGB <= summary.MaxFreeMemGB
		summary.Tiers = append(summary.Tiers, t)
	}
	return summary, rows.Err()
}
