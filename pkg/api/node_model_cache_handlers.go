// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/inferencegateway"
	"github.com/FlashbackAi/teepin-core/pkg/nodes"
	"github.com/FlashbackAi/teepin-core/pkg/nodeservices"
)

// modelCacheAgents is the slice of cluster.Registry this handler needs.
type modelCacheAgents interface {
	CachedModels(providerID string) (models []cluster.CachedModel, known, online bool)
	DeleteCachedModel(ctx context.Context, providerID, repoID string) error
}

// nodeLister and mountLister are the slices of pkg/nodes and pkg/nodeservices
// this handler needs.
type nodeLister interface {
	ListNodes(ctx context.Context) ([]nodes.Node, error)
}

type mountLister interface {
	ListForNode(ctx context.Context, nodeID uuid.UUID) ([]nodeservices.NodeService, error)
}

// NodeModelCacheHandler lets an operator see which models are downloaded onto
// a node's disk and delete the ones no longer needed, from Control Center
// alone. Mounted under /v1/admin behind the operator token.
type NodeModelCacheHandler struct {
	agents modelCacheAgents
	nodes  nodeLister
	mounts mountLister
}

// NewNodeModelCacheHandler wires the handler.
func NewNodeModelCacheHandler(agents modelCacheAgents, n nodeLister, m mountLister) *NodeModelCacheHandler {
	return &NodeModelCacheHandler{agents: agents, nodes: n, mounts: m}
}

type cachedModelView struct {
	RepoID    string `json:"repo_id"`
	SizeBytes int64  `json:"size_bytes"`
	// InUse is true while a mount on this node references the model (desired
	// mounted, or still running/starting). An in-use model cannot be deleted.
	InUse bool `json:"in_use"`
}

type cachedModelsResponse struct {
	Models []cachedModelView `json:"models"`
	// Known is false when the agent does not report a model cache (an older
	// agent, or a node that runs no native models).
	Known bool `json:"known"`
	// Online is false when the node's agent is not connected right now.
	Online bool `json:"online"`
}

func (h *NodeModelCacheHandler) providerFor(c *gin.Context) (nodes.Node, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid node id"})
		return nodes.Node{}, false
	}
	all, err := h.nodes.ListNodes(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load nodes"})
		return nodes.Node{}, false
	}
	for _, n := range all {
		if n.ID == id {
			return n, true
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
	return nodes.Node{}, false
}

// inUseRepos returns the repo ids referenced by any mount on the node that is
// still wanted or still occupying it.
func (h *NodeModelCacheHandler) inUseRepos(ctx context.Context, nodeID uuid.UUID) (map[string]bool, error) {
	rows, err := h.mounts.ListForNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, row := range rows {
		if row.Kind != nodeservices.KindInferenceModel {
			continue
		}
		if row.DesiredState == nodeservices.DesiredUnmounted && row.ObservedState == nodeservices.ObservedUnmounted {
			continue
		}
		cfg, err := inferencegateway.ParseModelServiceConfig(row.Config)
		if err != nil {
			continue
		}
		if repo := inferencegateway.HuggingFaceRepoID(cfg.ModelSource); repo != "" {
			used[strings.ToLower(repo)] = true
		}
	}
	return used, nil
}

// List is GET /v1/admin/nodes/:id/cached-models.
func (h *NodeModelCacheHandler) List(c *gin.Context) {
	node, ok := h.providerFor(c)
	if !ok {
		return
	}
	models, known, online := h.agents.CachedModels(node.ProviderID)
	resp := cachedModelsResponse{Models: []cachedModelView{}, Known: known, Online: online}
	if !online {
		c.JSON(http.StatusOK, resp)
		return
	}
	used, err := h.inUseRepos(c.Request.Context(), node.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load mounts"})
		return
	}
	for _, m := range models {
		resp.Models = append(resp.Models, cachedModelView{RepoID: m.RepoID, SizeBytes: m.SizeBytes, InUse: used[strings.ToLower(m.RepoID)]})
	}
	c.JSON(http.StatusOK, resp)
}

// Delete is DELETE /v1/admin/nodes/:id/cached-models?repo=org/name. The repo
// travels as a query parameter because it contains a slash.
func (h *NodeModelCacheHandler) Delete(c *gin.Context) {
	node, ok := h.providerFor(c)
	if !ok {
		return
	}
	repo := strings.TrimSpace(c.Query("repo"))
	if repo == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "repo is required"})
		return
	}

	// Refuse while a mount references it: deleting the weights out from under
	// a running server would fail its next request in a confusing way.
	used, err := h.inUseRepos(c.Request.Context(), node.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load mounts"})
		return
	}
	if used[strings.ToLower(repo)] {
		c.JSON(http.StatusConflict, gin.H{"error": "this model is mounted on the node; unmount it first"})
		return
	}

	err = h.agents.DeleteCachedModel(c.Request.Context(), node.ProviderID, repo)
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"message": "deleted " + repo})
	case errors.Is(err, cluster.ErrProviderOffline):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "the node's agent is offline"})
	case errors.Is(err, cluster.ErrModelNotCached):
		c.JSON(http.StatusNotFound, gin.H{"error": "that model is not in the node's cache"})
	default:
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
	}
}
