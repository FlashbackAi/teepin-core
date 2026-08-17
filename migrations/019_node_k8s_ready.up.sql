-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Health-check depth: "online" (migration 016, compute.nodes.status) means
-- only that the agent's gRPC session is connected -- it says nothing about
-- whether that node's own Kubernetes (k3s on a home node) can actually
-- schedule a pod. A node stays "online" if the agent process is alive even
-- after k3s has crashed underneath it, which let placement keep routing
-- work to a node that could not execute it.
--
-- k8s_ready is a separate, continuously-refreshed signal: it reflects
-- cluster.Client.Healthy(ctx) on the agent's own cluster client, reported
-- every ~30s alongside inventory (see GPUInventory.cluster_ready,
-- pkg/cluster/grpcserver.go reportInventorySeen, pkg/nodes UpsertSeen).
--
-- Default FALSE: an existing node has not yet reported the new field after
-- this upgrade, so it is NOT assumed ready -- it becomes ready the moment
-- its next inventory report (within one inventoryInterval, <=30s, of the
-- agent restarting on the new build) carries a healthy check. PlaceCPU
-- filters on this alongside status='online', so a node that is connected
-- but cannot execute is treated the same as a node with zero free capacity.

BEGIN;

ALTER TABLE compute.nodes
    ADD COLUMN IF NOT EXISTS k8s_ready BOOLEAN NOT NULL DEFAULT FALSE;

COMMIT;
