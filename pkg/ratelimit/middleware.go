// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package ratelimit

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/ulule/limiter/v3"
)

// Middleware creates a Gin middleware for rate limiting
type Middleware struct {
	limiter *Limiter
	config  *Config
}

// NewMiddleware creates a new rate limiting middleware
func NewMiddleware(limiter *Limiter, config *Config) *Middleware {
	return &Middleware{
		limiter: limiter,
		config:  config,
	}
}

// Handler returns the Gin middleware handler function
func (m *Middleware) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip if rate limiting is disabled
		if !m.config.Enabled {
			c.Next()
			return
		}

		// Get rate limit key (user ID or IP address)
		key := m.getKey(c)

		// Get user tier (default to "free" if not authenticated)
		tier := m.getTier(c)

		// Get endpoint (method + path)
		endpoint := m.getEndpoint(c)

		// Check rate limit
		allowed, limiterCtx, err := m.limiter.Allow(c.Request.Context(), key, tier, endpoint)

		// If error checking rate limit, fail open (allow request)
		if err != nil {
			// Log error to monitoring system
			c.Header("X-RateLimit-Error", "true")
			c.Next()
			return
		}

		// Add rate limit headers if configured
		if m.config.ReturnHeaders {
			m.setRateLimitHeaders(c, &limiterCtx)
		}

		// If rate limit exceeded, return 429 Too Many Requests
		if !allowed {
			m.handleRateLimitExceeded(c, &limiterCtx, tier, endpoint)
			return
		}

		// Continue to next handler
		c.Next()
	}
}

// getKey extracts the rate limit key from the request
// Priority: User ID (from auth) > API Key > IP Address
func (m *Middleware) getKey(c *gin.Context) string {
	// Try to get authenticated user ID (set by auth middleware)
	if userID, exists := c.Get("user_id"); exists {
		if uid, ok := userID.(string); ok && uid != "" {
			return fmt.Sprintf("user:%s", uid)
		}
	}

	// Try to get API key (set by auth middleware)
	if apiKey, exists := c.Get("api_key"); exists {
		if key, ok := apiKey.(string); ok && key != "" {
			return fmt.Sprintf("apikey:%s", key)
		}
	}

	// Fallback to IP address (less accurate due to NAT, but better than nothing)
	ip := c.ClientIP()
	return fmt.Sprintf("ip:%s", ip)
}

// getTier extracts the user's rate limit tier from the request
func (m *Middleware) getTier(c *gin.Context) string {
	// Try to get tier from context (set by auth middleware)
	if tier, exists := c.Get("rate_limit_tier"); exists {
		if t, ok := tier.(string); ok && t != "" {
			return t
		}
	}

	// If user is authenticated, use "authenticated" tier
	if _, exists := c.Get("user_id"); exists {
		return "authenticated"
	}

	// Default to "free" tier for unauthenticated requests
	return "free"
}

// getEndpoint returns the endpoint key for rate limiting
// Format: "METHOD /path/template"
func (m *Middleware) getEndpoint(c *gin.Context) string {
	// Get the matched route path (template with :id placeholders)
	path := c.FullPath()
	if path == "" {
		// Fallback to raw path if route not matched
		path = c.Request.URL.Path
	}

	method := c.Request.Method
	return fmt.Sprintf("%s %s", method, path)
}

// setRateLimitHeaders sets rate limit information headers
// Following industry standard header names
func (m *Middleware) setRateLimitHeaders(c *gin.Context, limiterCtx *limiter.Context) {
	c.Header("X-RateLimit-Limit", fmt.Sprintf("%d", limiterCtx.Limit))
	c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", limiterCtx.Remaining))
	c.Header("X-RateLimit-Reset", fmt.Sprintf("%d", limiterCtx.Reset))

	// Add retry-after header if limit exceeded
	if limiterCtx.Reached {
		retryAfter := limiterCtx.Reset - limiterCtx.Reset // seconds until reset
		c.Header("Retry-After", fmt.Sprintf("%d", retryAfter))
	}
}

// handleRateLimitExceeded handles the case when rate limit is exceeded
func (m *Middleware) handleRateLimitExceeded(c *gin.Context, limiterCtx *limiter.Context, tier, endpoint string) {
	// Calculate retry after seconds
	retryAfter := limiterCtx.Reset - limiterCtx.Reset

	// Return 429 Too Many Requests with helpful error message
	c.JSON(http.StatusTooManyRequests, gin.H{
		"error": "rate_limit_exceeded",
		"message": fmt.Sprintf(
			"Rate limit exceeded for %s tier. Please retry after %d seconds.",
			tier,
			retryAfter,
		),
		"details": gin.H{
			"tier":           tier,
			"limit":          limiterCtx.Limit,
			"retry_after":    retryAfter,
			"reset_at":       limiterCtx.Reset,
			"documentation": "https://docs.teepin.io/api/rate-limits",
		},
	})

	c.Abort()
}

// SkipRateLimitForPaths returns a middleware that skips rate limiting for specific paths
func SkipRateLimitForPaths(paths ...string) gin.HandlerFunc {
	pathSet := make(map[string]bool)
	for _, path := range paths {
		pathSet[path] = true
	}

	return func(c *gin.Context) {
		if pathSet[c.Request.URL.Path] {
			// Skip rate limiting for this path
			c.Set("skip_rate_limit", true)
		}
		c.Next()
	}
}

// CustomTier allows setting a custom tier for specific routes
// Example: router.Use(ratelimit.CustomTier("enterprise"))
func CustomTier(tier string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("rate_limit_tier", tier)
		c.Next()
	}
}

// extractPathTemplate attempts to get the route template from Gin
// e.g., /v1/compute/instances/:id instead of /v1/compute/instances/inst-abc123
func extractPathTemplate(c *gin.Context) string {
	// Gin stores the matched route path in FullPath()
	path := c.FullPath()
	if path != "" {
		return path
	}

	// Fallback: manually normalize common patterns
	rawPath := c.Request.URL.Path

	// Replace common ID patterns with template placeholders
	// This is a simple heuristic; in production you'd use Gin's routing info
	replacements := []struct {
		pattern string
		replace string
	}{
		{"/inst-", "/:id"},       // instance IDs
		{"/proj-", "/:id"},       // project IDs
		{"/key-", "/:key_id"},    // API key IDs
		{"/inv-", "/:id"},        // invoice IDs
	}

	normalized := rawPath
	for _, r := range replacements {
		if idx := strings.Index(normalized, r.pattern); idx != -1 {
			// Replace the ID portion with template
			end := idx + len(r.pattern)
			// Find the next slash or end of string
			slashIdx := strings.Index(normalized[end:], "/")
			if slashIdx == -1 {
				normalized = normalized[:idx] + r.replace
			} else {
				normalized = normalized[:idx] + r.replace + normalized[end+slashIdx:]
			}
		}
	}

	return normalized
}
