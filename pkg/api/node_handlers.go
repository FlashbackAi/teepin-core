// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/nodes"
)

// NodeHandler serves node enrollment (public, token-authenticated) and the
// operator-only node management endpoints. It is only mounted when the
// home-compute feature is enabled; absent that, none of these routes exist.
type NodeHandler struct {
	nodes *nodes.Service
}

// NewNodeHandler wires the node service.
func NewNodeHandler(svc *nodes.Service) *NodeHandler {
	return &NodeHandler{nodes: svc}
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

// ListNodes is GET /v1/admin/nodes — operator view of all nodes.
func (h *NodeHandler) ListNodes(c *gin.Context) {
	list, err := h.nodes.ListNodes(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list nodes"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"nodes": list, "count": len(list)})
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
