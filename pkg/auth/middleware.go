// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package auth

import (
	"context"
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
	SessionIDKey contextKey = "session_id"
)

// SessionChecker answers whether a Kumbha session-scoped credential
// (minted by MintSessionToken) is still usable — i.e. the session it
// names is open. Implemented by kumbha.Store; injected via
// WithSessionChecker rather than imported directly so pkg/auth has no
// dependency on pkg/kumbha, the same ProvisionGate/PricingProvider
// pattern used throughout pkg/api.
type SessionChecker interface {
	// IsSessionOpen reports whether sessionID belongs to accountID and is
	// currently open. A false result covers "not found", "wrong account"
	// and "closed" alike — deliberately indistinguishable, matching the
	// platform's existing "existence must not leak" convention (see
	// requireScope elsewhere in this codebase).
	IsSessionOpen(ctx context.Context, sessionID, accountID uuid.UUID) (bool, error)
}

// apiKeyPrefix identifies an API key from a JWT in the Authorization
// header.
const apiKeyPrefix = "tpk_"

// ProjectHeader carries the active project for a JWT-authenticated
// request. A login JWT identifies a person across their whole account, not
// one project — the console picks a project in its UI after signing in, so
// the credential itself cannot name one. An API key has no use for this
// header: its project is fixed at creation.
const ProjectHeader = "X-Project-ID"

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
	// SessionID is set only for a Kumbha agent credential (MintSessionToken)
	// — nil for every human login or API key.
	SessionID uuid.UUID
}

type Middleware struct {
	authService *Service
	jwtSecret   string
	// sessionChecker validates a Kumbha session-scoped credential's
	// SessionID claim on every request. Nil disables the capability
	// cleanly: a token carrying a SessionID claim is treated as invalid
	// rather than trusted unconditionally, matching how every other
	// optional dependency in this codebase fails closed when unset.
	sessionChecker SessionChecker
}

func NewMiddleware(authService *Service, jwtSecret string) *Middleware {
	return &Middleware{
		authService: authService,
		jwtSecret:   jwtSecret,
	}
}

// WithSessionChecker enables validating Kumbha session-scoped credentials.
// Returns the same *Middleware for chaining, so existing NewMiddleware
// call sites compile unchanged.
func (m *Middleware) WithSessionChecker(checker SessionChecker) *Middleware {
	m.sessionChecker = checker
	return m
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

	// A refresh token is structurally a valid JWT with the same claims as
	// an access token, differing only in ExpiresAt — without this check
	// it authenticated ordinary API calls perfectly well, silently
	// defeating the whole point of a 15-minute access token (found live
	// 2026-08-23). Only /v1/auth/refresh accepts one.
	if claims.TokenType == "refresh" {
		return nil
	}

	// A session-scoped credential (MintSessionToken) is only valid while
	// its session is open — checked on EVERY request, not just at mint
	// time, which is what makes closing the session revoke the credential
	// immediately rather than leaving it live until ttl expiry. A token
	// carrying this claim with no checker wired, or whose session has
	// closed or errors on lookup, is rejected outright: this is the
	// platform's most narrowly-scoped credential and the one most exposed
	// to prompt injection (KUMBHA-DESIGN.md's Topology section), so it
	// fails closed rather than falling back to ordinary JWT trust.
	if claims.SessionID != uuid.Nil {
		if m.sessionChecker == nil {
			return nil
		}
		open, err := m.sessionChecker.IsSessionOpen(c.Request.Context(), claims.SessionID, claims.AccountID)
		if err != nil || !open {
			return nil
		}
	}

	principal := &Principal{
		UserID:    claims.UserID,
		AccountID: claims.AccountID,
		Role:      claims.Role,
		ProjectID: claims.ProjectID,
		SessionID: claims.SessionID,
	}

	// A JWT never carries a project claim (see ProjectHeader) — resolve it
	// from the header instead, IF the caller's account actually owns that
	// project. Never trust the header alone: without this check, any
	// signed-in user could read another account's instances by sending
	// their project UUID. A bad or unrecognised header is not fatal here —
	// it just leaves ProjectID unset, and requireScope's existing "project
	// required" 401 covers it, same as a bare JWT does today.
	if principal.ProjectID == uuid.Nil {
		if headerProject := c.GetHeader(ProjectHeader); headerProject != "" {
			if projectID, err := uuid.Parse(headerProject); err == nil {
				if _, err := m.authService.GetProject(c.Request.Context(), claims.AccountID, projectID); err == nil {
					principal.ProjectID = projectID
				}
			}
		}
	}

	return principal
}

// store publishes the principal into the request context.
func store(c *gin.Context, p *Principal) {
	// Guarded like every other field below: a Kumbha session token
	// (auth.MintSessionToken) never sets Claims.UserID, leaving it at the
	// zero value. Setting the key unconditionally made GetUserID report
	// "present" (ok=true) with value uuid.Nil for such a credential,
	// which GetCurrentUser (auth_handlers.go) would have accepted as a
	// real, if nonexistent, user rather than correctly treating it as
	// "no user on this credential."
	if p.UserID != uuid.Nil {
		c.Set(string(UserIDKey), p.UserID)
	}
	if p.AccountID != uuid.Nil {
		c.Set(string(AccountIDKey), p.AccountID)
	}
	if p.Role != "" {
		c.Set(string(RoleKey), p.Role)
	}
	if p.ProjectID != uuid.Nil {
		c.Set(string(ProjectIDKey), p.ProjectID)
	}
	if p.SessionID != uuid.Nil {
		c.Set(string(SessionIDKey), p.SessionID)
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

// GetSessionID extracts the Kumbha session a credential is scoped to, if
// any — set only for an agent credential minted by MintSessionToken.
func GetSessionID(c *gin.Context) (uuid.UUID, bool) {
	return uuidFromContext(c, SessionIDKey)
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
