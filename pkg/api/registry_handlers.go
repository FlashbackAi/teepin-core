// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"net/http"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/harbor"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RegistryHandler handles container registry operations
type RegistryHandler struct {
	harborService *harbor.Service
	authService   *auth.Service
}

// NewRegistryHandler creates a new registry handler
func NewRegistryHandler(harborService *harbor.Service, authService *auth.Service) *RegistryHandler {
	return &RegistryHandler{
		harborService: harborService,
		authService:   authService,
	}
}

// ProvisionRegistry provisions a Harbor registry for a project
// POST /v1/projects/:id/registry
func (h *RegistryHandler) ProvisionRegistry(c *gin.Context) {
	// Get project ID from URL
	projectIDStr := c.Param("id")
	projectID, err := uuid.Parse(projectIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project ID"})
		return
	}

	// Verify user has access to this project
	userID, exists := auth.GetUserID(c)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	// Get project details
	project, err := h.authService.GetProject(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	// Verify ownership
	if project.OwnerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
		return
	}

	// Provision registry
	registryAccess, err := h.harborService.ProvisionProjectRegistry(
		c.Request.Context(),
		projectID,
		project.Name,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, registryAccess)
}

// GetRegistryCredentials retrieves registry credentials for a project
// GET /v1/projects/:id/registry
func (h *RegistryHandler) GetRegistryCredentials(c *gin.Context) {
	// Get project ID from URL
	projectIDStr := c.Param("id")
	projectID, err := uuid.Parse(projectIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project ID"})
		return
	}

	// Verify user has access to this project
	userID, exists := auth.GetUserID(c)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	// Get project details
	project, err := h.authService.GetProject(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	// Verify ownership
	if project.OwnerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
		return
	}

	// Get registry credentials
	registryAccess, err := h.harborService.GetRegistryCredentials(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "registry not provisioned"})
		return
	}

	c.JSON(http.StatusOK, registryAccess)
}

// RevokeRegistry revokes registry access for a project
// DELETE /v1/projects/:id/registry
func (h *RegistryHandler) RevokeRegistry(c *gin.Context) {
	// Get project ID from URL
	projectIDStr := c.Param("id")
	projectID, err := uuid.Parse(projectIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project ID"})
		return
	}

	// Verify user has access to this project
	userID, exists := auth.GetUserID(c)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	// Get project details
	project, err := h.authService.GetProject(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	// Verify ownership
	if project.OwnerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
		return
	}

	// Revoke registry access
	err = h.harborService.RevokeRegistryAccess(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "registry access revoked successfully",
	})
}

// GetDockerLoginCommand returns the docker login command for a project
// GET /v1/projects/:id/registry/login-command
func (h *RegistryHandler) GetDockerLoginCommand(c *gin.Context) {
	// Get project ID from URL
	projectIDStr := c.Param("id")
	projectID, err := uuid.Parse(projectIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project ID"})
		return
	}

	// Verify user has access to this project
	userID, exists := auth.GetUserID(c)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	// Get project details
	project, err := h.authService.GetProject(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	// Verify ownership
	if project.OwnerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
		return
	}

	// Get registry credentials
	registryAccess, err := h.harborService.GetRegistryCredentials(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "registry not provisioned"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"registry_url":         registryAccess.RegistryURL,
		"username":             registryAccess.Username,
		"image_prefix":         registryAccess.ImagePrefix,
		"docker_login_command": registryAccess.DockerLoginCmd,
		"instructions": []string{
			"1. Use your TEEPIN API key as the password",
			"2. Run: " + registryAccess.DockerLoginCmd,
			"3. Tag images with prefix: " + registryAccess.ImagePrefix + "/image-name:tag",
			"4. Push: docker push " + registryAccess.ImagePrefix + "/image-name:tag",
		},
	})
}
