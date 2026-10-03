// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/teepinbuild"
)

// Deployment plans are recorded as the agent presents them, and approval names
// one (see pkg/teepinbuild/plans.go). Recording is the agent's call; approving is
// the customer's, and is not reachable with an agent credential.

type recordBuildPlanRequest struct {
	Resources []teepinbuild.PlanResource `json:"resources"`
}

// RecordBuildPlan stores a plan the agent is about to show the customer and
// returns its id, which travels with the plan to the console and comes back
// when the customer approves.
// POST /v1/build/sessions/:id/plans
func (s *Server) RecordBuildPlan(c *gin.Context) {
	if s.teepinBuild == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Teepin Build is not available on this deployment"})
		return
	}
	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}
	callerSession, ok := auth.GetSessionID(c)
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "this endpoint requires a Teepin Build session credential"})
		return
	}
	if callerSession != sessionID {
		c.JSON(http.StatusForbidden, gin.H{"error": "this credential does not belong to that session"})
		return
	}
	_, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	var req recordBuildPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	id, err := s.teepinBuild.RecordPlan(c.Request.Context(), sessionID, accountID, req.Resources)
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"plan_id": id})
	case errors.Is(err, teepinbuild.ErrInvalidPlan), errors.Is(err, teepinbuild.ErrTooManyPlans):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, teepinbuild.ErrSessionNotFound), errors.Is(err, teepinbuild.ErrSessionClosed):
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found or no longer open"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// approveBuildDeployRequest is the optional body of approve-deploy. An older
// console sends none, which approves the newest plan.
type approveBuildDeployRequest struct {
	PlanID string `json:"plan_id"`
}

// parseApprovePlanID reads the optional plan_id from an approve-deploy request.
// An empty body, or no plan_id, is uuid.Nil ("the newest plan").
func parseApprovePlanID(c *gin.Context) (uuid.UUID, bool) {
	var req approveBuildDeployRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return uuid.Nil, false
		}
	}
	if req.PlanID == "" {
		return uuid.Nil, true
	}
	id, err := uuid.Parse(req.PlanID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan id"})
		return uuid.Nil, false
	}
	return id, true
}

// refuseUnapprovedResources enforces that a deploy stays inside the approved
// plan. It writes the response and returns false when the deploy must stop.
func (s *Server) refuseUnapprovedResources(c *gin.Context, sessionID uuid.UUID, cpuUnits, memoryGB, storageGB int) bool {
	err := s.teepinBuild.CheckResourcesApproved(c.Request.Context(), sessionID, cpuUnits, memoryGB, storageGB)
	if err == nil {
		return true
	}
	if errors.Is(err, teepinbuild.ErrExceedsApprovedPlan) {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error(), "code": "exceeds_approved_plan"})
		return false
	}
	// Fails closed, like the approval check beside it: an unreadable plan
	// must not become permission.
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unable to verify the approved plan, please retry"})
	return false
}

// enrichApprovedPlan adds approved_plan_id: which presented plan the customer
// approved, so the console can tell a plan it has not approved yet (the agent
// asked for more) from the one already running. Best effort: absent when the
// lookup fails or nothing is bound, which reads as "not known".
func (s *Server) enrichApprovedPlan(ctx context.Context, resp gin.H, sess *teepinbuild.Session) {
	if id, err := s.teepinBuild.ApprovedPlanID(ctx, sess.ID); err == nil && id != "" {
		resp["approved_plan_id"] = id
	}
}
