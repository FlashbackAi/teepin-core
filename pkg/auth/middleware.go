// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type contextKey string

const (
	UserIDKey    contextKey = "user_id"
	AccountIDKey contextKey = "account_id"
	RoleKey      contextKey = "role"
	ProjectIDKey contextKey = "project_id"
)

// apiKeyPrefix identifies an API key from a JWT in the Authorization
// header.
const apiKeyPrefix = "tpk_"

// Principal is the authenticated caller, however they authenticated.
// Both JWTs and API keys resolve to this, so handlers never need to know
// which was used.
type Principal struct {
	UserID    uuid.UUID
	AccountID uuid.UUID
	Role      string
	// ProjectID is set when the credential is scoped to one project —
	// always for API keys, optionally for JWTs.
	ProjectID uuid.UUID
}

type Middleware struct {
	authService *Service
	jwtSecret   string
}

func NewMiddleware(authService *Service, jwtSecret string) *Middleware {
	return &Middleware{
		authService: authService,
		jwtSecret:   jwtSecret,
	}
}

// authenticate resolves the Authorization header to a Principal.
// Returns nil when the header is absent or the credential is invalid;
// callers decide whether that is fatal.
func (m *Middleware) authenticate(c *gin.Context) *Principal {
	authHeader := c.GetHeader("Authorization")
	if authHeader == "" {
		return nil
	}

	scheme, token, found := strings.Cut(authHeader, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return nil
	}

	if strings.HasPrefix(token, apiKeyPrefix) {
		apiKey, err := m.authService.ValidateAPIKey(c.Request.Context(), token)
		if err != nil {
			return nil
		}
		return &Principal{
			UserID:    apiKey.UserID,
			AccountID: apiKey.AccountID,
			// API keys act with write access within their project; finer
			// scoping lives in apiKey.Scopes.
			Role:      RoleMember,
			ProjectID: apiKey.ProjectID,
		}
	}

	claims, err := VerifyJWT(token, m.jwtSecret)
	if err != nil {
		return nil
	}
	return &Principal{
		UserID:    claims.UserID,
		AccountID: claims.AccountID,
		Role:      claims.Role,
		ProjectID: claims.ProjectID,
	}
}

// store publishes the principal into the request context.
func store(c *gin.Context, p *Principal) {
	c.Set(string(UserIDKey), p.UserID)
	if p.AccountID != uuid.Nil {
		c.Set(string(AccountIDKey), p.AccountID)
	}
	if p.Role != "" {
		c.Set(string(RoleKey), p.Role)
	}
	if p.ProjectID != uuid.Nil {
		c.Set(string(ProjectIDKey), p.ProjectID)
	}
}

// RequireAuth rejects requests without a valid JWT or API key.
func (m *Middleware) RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := m.authenticate(c)
		if p == nil {
			// Deliberately uniform: distinguishing "missing" from
			// "invalid" tells an attacker whether a token was recognised.
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		store(c, p)
		c.Next()
	}
}

// OptionalAuth attaches the principal when credentials are present and
// valid, and otherwise proceeds anonymously.
func (m *Middleware) OptionalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if p := m.authenticate(c); p != nil {
			store(c, p)
		}
		c.Next()
	}
}

// RequireRole rejects callers whose role is not in the allowed set.
// Must run after RequireAuth.
func (m *Middleware) RequireRole(roles ...string) gin.HandlerFunc {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}

	return func(c *gin.Context) {
		role, _ := GetRole(c)
		if !allowed[role] {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "insufficient permissions for this operation",
			})
			return
		}
		c.Next()
	}
}

// GetUserID extracts the authenticated user's ID.
func GetUserID(c *gin.Context) (uuid.UUID, bool) {
	return uuidFromContext(c, UserIDKey)
}

// GetAccountID extracts the caller's account — the tenant boundary.
// Every query touching customer data must filter on it.
func GetAccountID(c *gin.Context) (uuid.UUID, bool) {
	return uuidFromContext(c, AccountIDKey)
}

// GetProjectID extracts the project the credential is scoped to, if any.
func GetProjectID(c *gin.Context) (uuid.UUID, bool) {
	return uuidFromContext(c, ProjectIDKey)
}

// GetRole extracts the caller's role within their account.
func GetRole(c *gin.Context) (string, bool) {
	v, exists := c.Get(string(RoleKey))
	if !exists {
		return "", false
	}
	role, ok := v.(string)
	return role, ok
}

func uuidFromContext(c *gin.Context, key contextKey) (uuid.UUID, bool) {
	v, exists := c.Get(string(key))
	if !exists {
		return uuid.Nil, false
	}
	id, ok := v.(uuid.UUID)
	return id, ok
}

// RequireProject ensures the request is scoped to a project.
func (m *Middleware) RequireProject() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, exists := GetProjectID(c); !exists {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "project_id required in context or request",
			})
			return
		}
		c.Next()
	}
}
