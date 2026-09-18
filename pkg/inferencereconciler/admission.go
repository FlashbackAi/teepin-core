// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inferencereconciler

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"log"
	"sort"

	"github.com/FlashbackAi/teepin-core/pkg/inferencegateway"
	"github.com/FlashbackAi/teepin-core/pkg/nodes"
	"github.com/FlashbackAi/teepin-core/pkg/nodeservices"
)

// defaultNodeReserveGB is memory kept back for macOS and the operator's own
// use on a native node when EngineConfig.NodeReserveGB is unset.
const defaultNodeReserveGB = 6

// pass is the state shared across one Reconcile run.
type pass struct {
	rows     []nodeservices.NodeService
	released map[uuid.UUID]bool // rows whose instance was torn down this pass
}

// holdsMemory reports whether a row's model process currently occupies (or
// is starting up and about to occupy) node memory.
func holdsMemory(row nodeservices.NodeService, p *pass) bool {
	if p.released[row.ID] {
		return false
	}
	return row.ObservedState == nodeservices.ObservedMounted || row.ObservedState == nodeservices.ObservedPending
}

// admit decides whether an MLX row may start now, applying a memory budget
// per native node: node RAM minus a reserve. Only MLX is budgeted here —
// container engines are sized by the Kubernetes scheduler.
//
// If the model fits alongside what is already loaded, it proceeds. If not,
// older models on the same node are evicted (unmounted, oldest first) until
// it fits, so the newest request wins deterministically and two requests
// can never evict each other in a loop. If it cannot fit even on an empty
// node, the mount is refused with the numbers. The eviction is synchronous:
// memory is released before the new model starts, never after.
func (r *Reconciler) admit(ctx context.Context, row nodeservices.NodeService, cfg inferencegateway.ModelServiceConfig, node nodes.Node, p *pass) error {
	if cfg.Engine != engineMLX {
		return nil
	}
	if cfg.MemoryGB <= 0 {
		return fmt.Errorf("memory_gb is required for engine %q (the model's weight footprint in GB)", engineMLX)
	}
	reserve := r.images.NodeReserveGB
	if reserve <= 0 {
		reserve = defaultNodeReserveGB
	}
	budget := node.MemoryGB - reserve
	if cfg.MemoryGB > budget {
		return fmt.Errorf("model needs %dGB but node %q has a %dGB model budget (%dGB RAM minus %dGB reserve)",
			cfg.MemoryGB, node.NodeName, budget, node.MemoryGB, reserve)
	}

	type holder struct {
		row nodeservices.NodeService
		gb  int
	}
	var holders []holder
	used := 0
	for _, peer := range p.rows {
		if peer.ID == row.ID || peer.NodeID != row.NodeID || !holdsMemory(peer, p) {
			continue
		}
		peerCfg, err := inferencegateway.ParseModelServiceConfig(peer.Config)
		if err != nil || peerCfg.Engine != engineMLX {
			continue
		}
		holders = append(holders, holder{peer, peerCfg.MemoryGB})
		used += peerCfg.MemoryGB
	}
	if used+cfg.MemoryGB <= budget {
		return nil
	}

	// Oldest first; never evict a model newer than the one being mounted.
	sort.Slice(holders, func(i, j int) bool { return holders[i].row.CreatedAt.Before(holders[j].row.CreatedAt) })
	var victims []holder
	freed := 0
	for _, h := range holders {
		if used-freed+cfg.MemoryGB <= budget {
			break
		}
		if h.row.CreatedAt.After(row.CreatedAt) {
			break
		}
		victims = append(victims, h)
		freed += h.gb
	}
	if used-freed+cfg.MemoryGB > budget {
		return fmt.Errorf("model needs %dGB but only %dGB of node %q's %dGB budget can be freed without evicting a newer model",
			cfg.MemoryGB, budget-(used-freed), node.NodeName, budget)
	}

	for _, v := range victims {
		log.Printf("inferencereconciler: evicting %s (%dGB) from node %s to make room for %s", v.row.ID, v.gb, node.NodeName, row.ID)
		if err := r.nodeSvcs.Unmount(ctx, v.row.ID); err != nil {
			return fmt.Errorf("evict %s: %w", v.row.ID, err)
		}
		if err := r.unmount(ctx, v.row); err != nil {
			return fmt.Errorf("evict %s: %w", v.row.ID, err)
		}
		p.released[v.row.ID] = true
	}
	return nil
}
