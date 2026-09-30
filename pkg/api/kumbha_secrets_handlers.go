// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
)

// Secrets a customer enters for the app a Kumbha build is making (an API key,
// a database URL). The value is typed into a secure field in the console,
// sent here once, stored sealed, and injected into the deployed app's
// environment. The build agent never sees it: these routes are not on the
// session-token allowlist (pkg/auth/session_routes.go), and each handler also
// refuses an agent credential itself, so the protection does not rest on one
// list staying correct.

// maxSecretRequestBytes caps the request body: a value is at most
// kumbha.MaxSecretValueBytes, plus JSON framing.
const maxSecretRequestBytes = kumbha.MaxSecretValueBytes + 1024

type putKumbhaSecretRequest struct {
	Value string `json:"value"`
}

type kumbhaSecretResponse struct {
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
}

// refuseAgentCredential answers 403 and returns true when the caller is a
// Kumbha agent's session token rather than a person.
func refuseAgentCredential(c *gin.Context) bool {
	if _, isAgent := auth.GetSessionID(c); isAgent {
		c.JSON(http.StatusForbidden, gin.H{"error": "this credential is not permitted to manage secrets"})
		return true
	}
	return false
}

func writeSecretError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, kumbha.ErrSecretsNotConfigured):
		c.JSON(http.StatusNotFound, gin.H{"error": "saving secrets is not available on this deployment"})
	case errors.Is(err, kumbha.ErrSessionNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
	case errors.Is(err, kumbha.ErrInvalidSecret):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, kumbha.ErrTooManySecrets):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "too_many_secrets"})
	default:
		// Deliberately the same body whatever the cause: the message could
		// otherwise carry fragments of a query that touched a secret.
		log.Printf("api: kumbha secret request failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not complete the request"})
	}
}

// PutKumbhaSecret stores or replaces one secret for a session.
// PUT /v1/kumbha/sessions/:id/secrets/:name
//
// The value is never logged, echoed, or returned.
func (s *Server) PutKumbhaSecret(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return
	}
	if refuseAgentCredential(c) {
		return
	}
	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}
	_, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSecretRequestBytes)
	var req putKumbhaSecretRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// err can quote the offending JSON, which here is a secret: say only
		// that the body was unusable.
		c.JSON(http.StatusBadRequest, gin.H{"error": "the request body must be JSON with a \"value\" field of reasonable size"})
		return
	}

	name := c.Param("name")
	if err := s.kumbha.SaveSecret(c.Request.Context(), sessionID, accountID, name, req.Value); err != nil {
		writeSecretError(c, err)
		return
	}
	c.JSON(http.StatusOK, kumbhaSecretResponse{Name: name, UpdatedAt: time.Now().UTC()})
}

// ListKumbhaSecrets lists the names of a session's saved secrets, never the
// values. GET /v1/kumbha/sessions/:id/secrets
func (s *Server) ListKumbhaSecrets(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return
	}
	if refuseAgentCredential(c) {
		return
	}
	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}
	_, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}
	infos, err := s.kumbha.ListSecrets(c.Request.Context(), sessionID, accountID)
	if err != nil {
		writeSecretError(c, err)
		return
	}
	out := make([]kumbhaSecretResponse, 0, len(infos))
	for _, i := range infos {
		out = append(out, kumbhaSecretResponse{Name: i.Name, UpdatedAt: i.UpdatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"secrets": out})
}

// DeleteKumbhaSecret removes one secret.
// DELETE /v1/kumbha/sessions/:id/secrets/:name
func (s *Server) DeleteKumbhaSecret(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return
	}
	if refuseAgentCredential(c) {
		return
	}
	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}
	_, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}
	if err := s.kumbha.DeleteSecret(c.Request.Context(), sessionID, accountID, c.Param("name")); err != nil {
		writeSecretError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// withKumbhaSecrets returns env with the session's saved secrets added, for
// the spec of an app being created or redeployed. A saved secret wins over
// a same-named value in env: the agent cannot know a secret's value, so
// anything it put under that name is a placeholder.
//
// This is the only place secret values leave storage, and they go straight
// into the customer's own container spec — never into a response, a log line
// or anything the agent can read.
func (s *Server) withKumbhaSecrets(ctx context.Context, sessionID, accountID uuid.UUID, env map[string]string) (map[string]string, error) {
	if s.kumbha == nil || sessionID == uuid.Nil {
		return env, nil
	}
	secrets, err := s.kumbha.SecretEnv(ctx, sessionID, accountID)
	if err != nil {
		return nil, err
	}
	if len(secrets) == 0 {
		return env, nil
	}
	merged := make(map[string]string, len(env)+len(secrets))
	for k, v := range env {
		merged[k] = v
	}
	for k, v := range secrets {
		merged[k] = v
	}
	return merged, nil
}
