// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/nodeservices"
)

// NodeServicesHandler serves the generic, Control-Center-driven mount/
// unmount admin API. An inference model server (kind=inference_model) is
// the first user; pushing a teepin-agent binary update to a host
// (kind=agent_binary) is meant to use these EXACT same endpoints later,
// not a separate mechanism — see pkg/nodeservices' own doc comment.
type NodeServicesHandler struct {
	svc *nodeservices.Service
}

// NewNodeServicesHandler wires the node-services service.
func NewNodeServicesHandler(svc *nodeservices.Service) *NodeServicesHandler {
	return &NodeServicesHandler{svc: svc}
}

type mountRequest struct {
	NodeID string          `json:"node_id" binding:"required"`
	Kind   string          `json:"kind" binding:"required"`
	Config json.RawMessage `json:"config"`
}

// Mount is POST /v1/admin/node-services — records a desired mount. Always
// creates a new row (see nodeservices.Mount's own doc comment on why two
// mounts of the same kind on one node are never conflated).
func (h *NodeServicesHandler) Mount(c *gin.Context) {
	var req mountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	nodeID, err := uuid.Parse(req.NodeID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid node_id"})
		return
	}

	ns, err := h.svc.Mount(c.Request.Context(), nodeID, nodeservices.Kind(req.Kind), req.Config, "admin-api")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, ns)
}

// Unmount is DELETE /v1/admin/node-services/:id — flips desired_state to
// unmounted. The row itself is never deleted (history, and an idempotent
// re-mount both want it to still exist) — whatever reconciles this kind
// is responsible for actually tearing the underlying thing down.
func (h *NodeServicesHandler) Unmount(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if err := h.svc.Unmount(c.Request.Context(), id); err != nil {
		if errors.Is(err, nodeservices.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "node service not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "unmount requested", "id": id})
}

// Get is GET /v1/admin/node-services/:id.
func (h *NodeServicesHandler) Get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	ns, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, nodeservices.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "node service not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, ns)
}

// List is GET /v1/admin/node-services?node_id=...  or  ?kind=... — exactly
// one filter is required; listing every row platform-wide with no filter
// isn't a real Control Center use case yet, and requiring one keeps this
// endpoint from needing pagination on day one.
func (h *NodeServicesHandler) List(c *gin.Context) {
	if nodeIDParam := c.Query("node_id"); nodeIDParam != "" {
		nodeID, err := uuid.Parse(nodeIDParam)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid node_id"})
			return
		}
		list, err := h.svc.ListForNode(c.Request.Context(), nodeID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"node_services": list})
		return
	}

	if kindParam := c.Query("kind"); kindParam != "" {
		list, err := h.svc.ListByKind(c.Request.Context(), nodeservices.Kind(kindParam))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"node_services": list})
		return
	}

	c.JSON(http.StatusBadRequest, gin.H{"error": "node_id or kind query parameter is required"})
}
