package api

import (
	"net/http"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type AuthHandler struct {
	authService *auth.Service
}

func NewAuthHandler(authService *auth.Service) *AuthHandler {
	return &AuthHandler{authService: authService}
}

// Register handles user registration
// POST /v1/auth/register
func (h *AuthHandler) Register(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required,email"`
		Password string `json:"password" binding:"required,min=8"`
		FullName string `json:"full_name"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, err := h.authService.RegisterUser(c.Request.Context(), req.Email, req.Password, req.FullName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, user)
}

// Login handles user login
// POST /v1/auth/login
func (h *AuthHandler) Login(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required,email"`
		Password string `json:"password" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	accessToken, refreshToken, err := h.authService.Login(c.Request.Context(), req.Email, req.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"token_type":    "Bearer",
		"expires_in":    900, // 15 minutes
	})
}

// GetCurrentUser returns the currently authenticated user
// GET /v1/auth/me
func (h *AuthHandler) GetCurrentUser(c *gin.Context) {
	userID, exists := auth.GetUserID(c)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return
	}

	user, err := h.authService.GetUserByID(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	c.JSON(http.StatusOK, user)
}

// CreateProject creates a new project
// POST /v1/projects
func (h *AuthHandler) CreateProject(c *gin.Context) {
	accountID, ok := requireAccount(c)
	if !ok {
		return
	}
	if !requireWriteRole(c) {
		return
	}
	userID, _ := auth.GetUserID(c)

	var req struct {
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// The account owns the project; userID only records who created it.
	project, err := h.authService.CreateProject(c.Request.Context(), accountID, userID, req.Name, req.Description)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, project)
}

// ListProjects lists all projects for the authenticated user
// GET /v1/projects
func (h *AuthHandler) ListProjects(c *gin.Context) {
	accountID, ok := requireAccount(c)
	if !ok {
		return
	}

	// Every member of the account sees its projects — scoping by user
	// would hide a colleague's projects from their own teammates.
	projects, err := h.authService.ListProjects(c.Request.Context(), accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"projects": projects, "count": len(projects)})
}

// GetProject retrieves a specific project
// GET /v1/projects/:id
func (h *AuthHandler) GetProject(c *gin.Context) {
	projectID, ok := parseProjectID(c)
	if !ok {
		return
	}
	accountID, ok := requireAccount(c)
	if !ok {
		return
	}

	// Tenancy is enforced in the query itself: a project in another
	// account simply does not match, so it is reported as not found.
	// The previous owner_id comparison both leaked existence (403 tells
	// you it exists) and denied colleagues access to their own account's
	// projects.
	project, err := h.authService.GetProject(c.Request.Context(), accountID, projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	c.JSON(http.StatusOK, project)
}

// CreateAPIKey creates a new API key for a project
// POST /v1/projects/:id/api-keys
func (h *AuthHandler) CreateAPIKey(c *gin.Context) {
	userID, exists := auth.GetUserID(c)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return
	}

	projectID, ok := parseProjectID(c)
	if !ok {
		return
	}
	if !requireWriteRole(c) {
		return
	}

	// Tenancy: the project must belong to the caller's account.
	// Reported as 404 rather than 403 so the response never confirms
	// that another account's project exists.
	if _, ok := h.requireAccountProject(c, projectID); !ok {
		return
	}

	var req struct {
		Name   string   `json:"name" binding:"required"`
		Scopes []string `json:"scopes"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Scopes == nil {
		req.Scopes = []string{"instances:read", "instances:write"}
	}

	key, apiKey, err := h.authService.CreateAPIKey(c.Request.Context(), userID, projectID, req.Name, req.Scopes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"key":     key, // Only returned once!
		"api_key": apiKey,
	})
}

// ListAPIKeys lists all API keys for a project
// GET /v1/projects/:id/api-keys
func (h *AuthHandler) ListAPIKeys(c *gin.Context) {
	projectID, ok := parseProjectID(c)
	if !ok {
		return
	}

	// Tenancy: the project must belong to the caller's account.
	// Reported as 404 rather than 403 so the response never confirms
	// that another account's project exists.
	if _, ok := h.requireAccountProject(c, projectID); !ok {
		return
	}

	apiKeys, err := h.authService.ListAPIKeys(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"api_keys": apiKeys})
}

// RevokeAPIKey revokes an API key
// DELETE /v1/projects/:project_id/api-keys/:key_id
func (h *AuthHandler) RevokeAPIKey(c *gin.Context) {
	projectID, ok := parseProjectID(c)
	if !ok {
		return
	}
	if !requireWriteRole(c) {
		return
	}

	keyIDStr := c.Param("key_id")
	keyID, err := uuid.Parse(keyIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid API key ID"})
		return
	}

	// Tenancy: the project must belong to the caller's account.
	// Reported as 404 rather than 403 so the response never confirms
	// that another account's project exists.
	if _, ok := h.requireAccountProject(c, projectID); !ok {
		return
	}

	err = h.authService.RevokeAPIKey(c.Request.Context(), keyID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "API key revoked"})
}
