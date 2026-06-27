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
	ProjectIDKey contextKey = "project_id"
)

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

// RequireAuth validates JWT token or API key
func (m *Middleware) RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
			c.Abort()
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header format"})
			c.Abort()
			return
		}

		token := parts[1]

		// Check if it's an API key (starts with tpk_)
		if strings.HasPrefix(token, "tpk_") {
			apiKey, err := m.authService.ValidateAPIKey(c.Request.Context(), token)
			if err != nil {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
				c.Abort()
				return
			}

			// Set user and project in context
			c.Set(string(UserIDKey), apiKey.UserID)
			c.Set(string(ProjectIDKey), apiKey.ProjectID)
			c.Next()
			return
		}

		// Otherwise, treat as JWT
		claims, err := VerifyJWT(token, m.jwtSecret)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			c.Abort()
			return
		}

		// Set user in context
		c.Set(string(UserIDKey), claims.UserID)
		if claims.ProjectID != uuid.Nil {
			c.Set(string(ProjectIDKey), claims.ProjectID)
		}
		c.Next()
	}
}

// OptionalAuth validates token if present but doesn't require it
func (m *Middleware) OptionalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.Next()
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			c.Next()
			return
		}

		token := parts[1]

		// Check if it's an API key
		if strings.HasPrefix(token, "tpk_") {
			apiKey, err := m.authService.ValidateAPIKey(c.Request.Context(), token)
			if err == nil {
				c.Set(string(UserIDKey), apiKey.UserID)
				c.Set(string(ProjectIDKey), apiKey.ProjectID)
			}
			c.Next()
			return
		}

		// Try JWT
		claims, err := VerifyJWT(token, m.jwtSecret)
		if err == nil {
			c.Set(string(UserIDKey), claims.UserID)
			if claims.ProjectID != uuid.Nil {
				c.Set(string(ProjectIDKey), claims.ProjectID)
			}
		}
		c.Next()
	}
}

// GetUserID extracts user ID from context
func GetUserID(c *gin.Context) (uuid.UUID, bool) {
	userID, exists := c.Get(string(UserIDKey))
	if !exists {
		return uuid.Nil, false
	}
	id, ok := userID.(uuid.UUID)
	return id, ok
}

// GetProjectID extracts project ID from context
func GetProjectID(c *gin.Context) (uuid.UUID, bool) {
	projectID, exists := c.Get(string(ProjectIDKey))
	if !exists {
		return uuid.Nil, false
	}
	id, ok := projectID.(uuid.UUID)
	return id, ok
}

// RequireProject middleware ensures a project ID is present
func (m *Middleware) RequireProject() gin.HandlerFunc {
	return func(c *gin.Context) {
		_, exists := GetProjectID(c)
		if !exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "project_id required in context or request"})
			c.Abort()
			return
		}
		c.Next()
	}
}
