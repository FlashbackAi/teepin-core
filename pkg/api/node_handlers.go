// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/nodes"
)

// CPURateProvider supplies the live CPU/memory rates so the home-capacity
// tier prices are computed with the same formula metering uses. An interface
// (satisfied by *billing.Service) so the handler does not hard-depend on the
// billing service and can be tested with a stub. Nil ⇒ rates treated as 0
// (tiers price at $0, matching the "metered but not billed until a rate is
// set" default).
type CPURateProvider interface {
	CPUCoreRate(ctx context.Context) float64
	MemoryGBRate(ctx context.Context) float64
}

// NodeHandler serves node enrollment (public, token-authenticated) and the
// operator-only node management endpoints. It is only mounted when the
// home-compute feature is enabled; absent that, none of these routes exist.
type NodeHandler struct {
	nodes *nodes.Service
	rates CPURateProvider
}

// NewNodeHandler wires the node service. rates may be nil (rates default to 0).
func NewNodeHandler(svc *nodes.Service, rates CPURateProvider) *NodeHandler {
	return &NodeHandler{nodes: svc, rates: rates}
}

// enrollRequest is what an agent posts to redeem its one-time token. It
// deliberately has NO class field — class is fixed by the token, server-side.
type enrollRequest struct {
	Token        string `json:"token" binding:"required"`
	NodeName     string `json:"node_name" binding:"required"`
	ProviderID   string `json:"provider_id" binding:"required"`
	Region       string `json:"region"`
	CPUCores     int    `json:"cpu_cores"`
	MemoryGB     int    `json:"memory_gb"`
	GPUModel     string `json:"gpu_model"`
	GPUCount     int    `json:"gpu_count"`
	MIGCapable   bool   `json:"mig_capable"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	AgentVersion string `json:"agent_version"`
}

// Enroll is POST /v1/nodes/enroll — UNAUTHENTICATED at the router because the
// enrolling agent has no credential yet; the one-time token in the body IS
// the authentication. On success it returns the per-node credential ONCE.
func (h *NodeHandler) Enroll(c *gin.Context) {
	var req enrollRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	credential, node, err := h.nodes.Enroll(c.Request.Context(), req.Token, nodes.NodeSpecs{
		NodeName:     req.NodeName,
		ProviderID:   req.ProviderID,
		Region:       req.Region,
		CPUCores:     req.CPUCores,
		MemoryGB:     req.MemoryGB,
		GPUModel:     req.GPUModel,
		GPUCount:     req.GPUCount,
		MIGCapable:   req.MIGCapable,
		OS:           req.OS,
		Arch:         req.Arch,
		AgentVersion: req.AgentVersion,
	})
	if err != nil {
		// A bad/expired/used token is a 401 — the request is well-formed,
		// the credential is not acceptable. Distinct sentinels are collapsed
		// to one status so enumeration learns nothing about which tokens
		// exist; the message still tells an honest operator what happened.
		switch {
		case errors.Is(err, nodes.ErrTokenInvalid),
			errors.Is(err, nodes.ErrTokenExpired),
			errors.Is(err, nodes.ErrTokenConsumed):
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "enrollment failed"})
		}
		return
	}

	// The credential is shown exactly once — the agent persists it locally.
	c.JSON(http.StatusOK, gin.H{
		"credential": credential,
		"node": gin.H{
			"id":        node.ID,
			"node_name": node.NodeName,
			"class":     node.Class,
			"status":    node.Status,
		},
	})
}

// ListNodes is GET /v1/admin/nodes — operator view of all nodes, each with its
// capacity breakdown (detected / rentable / used / free).
func (h *NodeHandler) ListNodes(c *gin.Context) {
	list, err := h.nodes.ListNodes(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list nodes"})
		return
	}
	// Capacity (used/free) is derived from running instances; attach it
	// keyed by node id so the console can show "24 rented, 8 used, 16 free".
	caps, err := h.nodes.ListNodeCapacity(c.Request.Context())
	if err != nil {
		// Non-fatal: the node list is still useful without the derived
		// used/free breakdown.
		c.JSON(http.StatusOK, gin.H{"nodes": list, "count": len(list)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"nodes": list, "capacity": caps, "count": len(list)})
}

// reservationRequest sets how much of a node to rent out. Zero is valid (offer
// nothing); neither field may exceed the node's detected specs.
type reservationRequest struct {
	CPUCores int `json:"cpu_cores"`
	MemoryGB int `json:"memory_gb"`
}

// SetReservation is PUT /v1/admin/nodes/:id/reservation.
func (h *NodeHandler) SetReservation(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid node id"})
		return
	}
	var req reservationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.nodes.SetReservation(c.Request.Context(), id, req.CPUCores, req.MemoryGB); err != nil {
		switch {
		case errors.Is(err, nodes.ErrOverCommit):
			// Offering more than the machine has is a request problem.
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, nodes.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set reservation"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "reservation updated", "id": id,
		"cpu_cores": req.CPUCores, "memory_gb": req.MemoryGB})
}

// createTokenRequest mints an enrollment token. class defaults to 'home';
// 'datacenter' must be chosen deliberately by the operator.
type createTokenRequest struct {
	Label      string `json:"label" binding:"required"`
	Class      string `json:"class"`
	TTLMinutes int    `json:"ttl_minutes"`
}

// CreateEnrollmentToken is POST /v1/admin/nodes/enrollment-tokens. Returns
// the plaintext token ONCE for the operator to hand to the machine.
func (h *NodeHandler) CreateEnrollmentToken(c *gin.Context) {
	var req createTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	class := req.Class
	if class == "" {
		class = nodes.ClassHome
	}
	ttl := time.Duration(req.TTLMinutes) * time.Minute

	plaintext, tok, err := h.nodes.CreateEnrollmentToken(
		c.Request.Context(), req.Label, class, "admin", ttl)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"token":      plaintext, // shown once
		"class":      tok.Class,
		"label":      tok.Label,
		"expires_at": tok.ExpiresAt,
	})
}

// HomeCapacity is GET /v1/compute/home-capacity — the customer-facing view
// used by the create dialog: the CPU tiers with a per-tier "fits right now"
// flag, plus free-capacity totals. JWT-authed (a capacity view is not a
// compute resource). Exposes no per-node internals.
func (h *NodeHandler) HomeCapacity(c *gin.Context) {
	// Price each tier from the live CPU/memory rates so the quote matches the
	// bill. Nil provider ⇒ zero rates (metered but not billed).
	var rates nodes.CPURates
	if h.rates != nil {
		rates.CPUCoreRate = h.rates.CPUCoreRate(c.Request.Context())
		rates.MemoryGBRate = h.rates.MemoryGBRate(c.Request.Context())
	}
	summary, err := h.nodes.HomeCapacitySummary(c.Request.Context(), rates)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read home capacity"})
		return
	}
	c.JSON(http.StatusOK, summary)
}

// renameRequest changes a node's display name.
type renameRequest struct {
	NodeName string `json:"node_name" binding:"required"`
}

// RenameNode is PATCH /v1/admin/nodes/:id.
func (h *NodeHandler) RenameNode(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid node id"})
		return
	}
	var req renameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.nodes.RenameNode(c.Request.Context(), id, req.NodeName); err != nil {
		if errors.Is(err, nodes.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "node renamed", "id": id, "node_name": req.NodeName})
}

// DeleteNode is DELETE /v1/admin/nodes/:id. Refuses (409) when the node still
// has active instances — the operator must terminate them or disable instead.
func (h *NodeHandler) DeleteNode(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid node id"})
		return
	}
	if err := h.nodes.DeleteNode(c.Request.Context(), id); err != nil {
		switch {
		case errors.Is(err, nodes.ErrNodeHasInstances):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case errors.Is(err, nodes.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete node"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "node deleted", "id": id})
}

// DisableNode is POST /v1/admin/nodes/:id/disable.
func (h *NodeHandler) DisableNode(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid node id"})
		return
	}
	if err := h.nodes.DisableNode(c.Request.Context(), id); err != nil {
		if errors.Is(err, nodes.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to disable node"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "node disabled", "id": id})
}

// GetNodeMetrics is GET /v1/admin/nodes/:id/metrics — a node's raw
// utilization history, oldest first. The foundation read path for
// telemetry consumers (internal stats/graphs, the status page, a
// marketing-site globe — ROADMAP.md's 2026-09-03 entry); none of those
// are built yet, this is just what they would call.
//
// ?since=<Go duration, e.g. "1h", "24h"> bounds how far back to look.
// Omitted or unparseable falls back to nodes.DefaultMetricsWindow (1h) —
// see that const's own doc comment for why this is deliberately NOT the
// max window (a mistyped/omitted param should degrade to a small, sane
// default, not silently cost the largest possible query). An explicit
// value past nodes.MaxMetricsWindow is still honored, clamped down to
// it, rather than rejected — a caller who explicitly asked for more than
// the max gets the max, not a 400, on a read-only endpoint like this.
func (h *NodeHandler) GetNodeMetrics(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid node id"})
		return
	}
	exists, err := h.nodes.NodeExists(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to look up node"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
		return
	}
	var since time.Duration
	if s := c.Query("since"); s != "" {
		since, _ = time.ParseDuration(s) // zero on failure -> ListMetrics' own default
	}
	samples, err := h.nodes.ListMetrics(c.Request.Context(), id, since)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list node metrics"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"node_id": id, "samples": samples})
}
