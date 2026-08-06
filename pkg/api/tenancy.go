// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
)

// requireAccount resolves the caller's account — the tenant boundary
// every customer-data query must filter on.
//
// Returns false and writes the response when the caller has no account
// in context, so handlers can simply `return` afterwards. Centralised
// deliberately: a handler that forgets to scope by account is a
// cross-tenant data leak, so there is exactly one way to obtain it.
func requireAccount(c *gin.Context) (uuid.UUID, bool) {
	accountID, ok := auth.GetAccountID(c)
	if !ok || accountID == uuid.Nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": "authentication required",
		})
		return uuid.Nil, false
	}
	return accountID, true
}

// requireAccountProject resolves the caller's account and verifies the
// given project belongs to it.
//
// A project in another account is reported as 404, never 403: a
// "forbidden" response confirms the project exists, which leaks the
// existence of other customers' resources.
func (h *AuthHandler) requireAccountProject(c *gin.Context, projectID uuid.UUID) (uuid.UUID, bool) {
	accountID, ok := requireAccount(c)
	if !ok {
		return uuid.Nil, false
	}

	if _, err := h.authService.GetProject(c.Request.Context(), accountID, projectID); err != nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return uuid.Nil, false
	}

	return accountID, true
}

// parseProjectID reads the :id path parameter as a project UUID.
func parseProjectID(c *gin.Context) (uuid.UUID, bool) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid project ID"})
		return uuid.Nil, false
	}
	return projectID, true
}

// requireWriteRole rejects read-only callers. Must run after
// authentication.
func requireWriteRole(c *gin.Context) bool {
	role, _ := auth.GetRole(c)
	if role == auth.RoleViewer {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "this operation requires write access",
		})
		return false
	}
	return true
}
