// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
)

// createExecSessionRequest is deliberately small: v1 exposes only the
// two customer-facing choices a shell session can meaningfully vary —
// which container (when a pod has more than one; empty picks the
// pod's first/only container) and what to run (empty = the agent
// probes for a shell). Command is accepted here, not left shell-only,
// per the confirmed v1 scope.
type createExecSessionRequest struct {
	Container string   `json:"container,omitempty"`
	Command   []string `json:"command,omitempty"`
}

// CreateExecSession issues a short-lived, single-use ticket for the
// WebSocket attach step (GET .../exec/attach). Copies GetInstanceLogs'
// tenancy pattern exactly: requireScope, then a cluster call scoped to
// the caller's project — existence must not leak, so a nonexistent and
// another tenant's instance both look identical (404).
// POST /v1/compute/instances/:id/exec
func (s *Server) CreateExecSession(c *gin.Context) {
	if s.execTickets == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "interactive terminals are not available on this deployment"})
		return
	}

	instanceID := c.Param("id")
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	var req createExecSessionRequest
	// A missing/empty body is the common case (plain "connect, give me a
	// shell") — only reject a body that is PRESENT but malformed.
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
	}

	status, err := s.cluster.GetInstanceStatus(c.Request.Context(), scopeFor(projectID), instanceID)
	if err != nil {
		if errors.Is(err, cluster.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
			return
		}
		if errors.Is(err, cluster.ErrClusterUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Capacity is temporarily unreachable; a terminal session cannot be started right now",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if status.Status != "running" {
		c.JSON(http.StatusConflict, gin.H{
			"error": "the instance is " + status.Status + "; a terminal can only attach to a running instance",
		})
		return
	}

	userID, _ := auth.GetUserID(c) // uuid.Nil if absent (e.g. a project API key) — valid, written as NULL

	ticketID, secret, err := s.execTickets.Issue(cluster.ExecTicket{
		InstanceID: instanceID,
		ProjectID:  projectID,
		AccountID:  accountID,
		UserID:     userID,
		Container:  req.Container,
		Command:    req.Command,
		TTY:        true,
	})
	if err != nil {
		if errors.Is(err, cluster.ErrTicketStoreFull) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "too many terminal sessions are starting right now; try again shortly"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Audit trail from the moment access is GRANTED, not just from a
	// successful attach — see migrations/022_exec_sessions. Best-effort:
	// a write failure here must not block a customer from getting their
	// terminal, but it is logged loudly since this table is the whole
	// audit story for the most sensitive action on the platform.
	if s.store != nil {
		if err := s.store.CreateExecSession(c.Request.Context(), compute.ExecSessionRecord{
			TicketID:   ticketID,
			InstanceID: instanceID,
			AccountID:  accountID,
			ProjectID:  projectID,
			UserID:     userID,
			Container:  req.Container,
			Command:    req.Command,
		}); err != nil {
			log.Printf("AUDIT WARNING: failed to record exec_sessions row for ticket %s: %v", ticketID, err)
		}
	}
	log.Printf("AUDIT exec_issue ticket_id=%s instance_id=%s account_id=%s project_id=%s user_id=%s src_ip=%s",
		ticketID, instanceID, accountID, projectID, userID, c.ClientIP())

	c.JSON(http.StatusOK, gin.H{
		"ticket_id":     ticketID,
		"ticket_secret": secret,
		"attach_path":   "/v1/compute/instances/" + instanceID + "/exec/attach",
		"expires_in":    30,
	})
}
