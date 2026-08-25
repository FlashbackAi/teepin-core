// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/build"
	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
)

// createKumbhaSessionRequest pre-authorises a build session's spend.
// budget is in dollars, not tokens or credits — see KUMBHA-DESIGN.md's
// "Pricing model" section on why the customer-facing unit stays a plain
// currency figure, never a raw token count.
//
// Prompt is what actually starts the agent — if present, CreateKumbhaSession
// launches it as part of this same call (see LaunchAgent below) so the
// console's flow is a single request: "start building" is one API call,
// not "create a session" then a separate "now start it".
type createKumbhaSessionRequest struct {
	Budget float64 `json:"budget"`
	Label  string  `json:"label,omitempty"`
	Prompt string  `json:"prompt,omitempty"`
}

// kumbhaSessionResponse is the shared JSON shape for every endpoint that
// returns a session — creation, status polling, and close.
func kumbhaSessionResponse(sess *kumbha.Session) gin.H {
	return gin.H{
		"id":              sess.ID,
		"budget":          sess.Budget,
		"spent":           sess.Spent,
		"status":          sess.Status,
		"label":           sess.Label,
		"deploy_approved": sess.DeployApproved,
		// agent_running is derived rather than exposing agent_instance_id
		// directly — the console needs to know whether a build is
		// underway, never the underlying pod identifier (Kumbha's own
		// workload; see pkg/kumbha/agent.go's LaunchAgent doc comment).
		"agent_running": sess.AgentInstanceID != "",
		"started_at":    sess.StartedAt,
		"ended_at":      sess.EndedAt,
	}
}

// CreateKumbhaSession pre-authorises a new Kumbha build session, gated by
// the same payment check compute provisioning already enforces, and —
// when a prompt is supplied — launches the agent in the same call.
// POST /v1/kumbha/sessions
func (s *Server) CreateKumbhaSession(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return
	}

	var req createKumbhaSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	sess, err := s.kumbha.CreateSession(c.Request.Context(), accountID, projectID, req.Budget, req.Label)
	if err != nil {
		switch {
		case errors.Is(err, kumbha.ErrPaymentRequired):
			c.JSON(http.StatusPaymentRequired, gin.H{"error": err.Error(), "code": "payment_method_required"})
		case errors.Is(err, kumbha.ErrGateUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unable to verify billing status, please retry"})
		default:
			// Budget validation errors (non-positive, over the per-session
			// cap) are the only other failure mode from CreateSession, and
			// both are the customer's request being wrong, not the
			// platform's — 400 either way.
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}

	if req.Prompt != "" {
		if err := s.kumbha.LaunchAgent(c.Request.Context(), sess, req.Prompt); err != nil {
			if errors.Is(err, kumbha.ErrAgentNotConfigured) {
				// The session itself is valid and stays open (a customer
				// can still close it, or a future retry could launch the
				// agent once the platform is configured) — but nothing
				// will happen until it does, so this must not read as 201
				// success.
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error": "the Kumbha agent is not available on this deployment yet",
					"code":  "agent_not_configured",
				})
				return
			}
			log.Printf("WARN: kumbha session %s created but agent launch failed: %v", sess.ID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "session created but the agent could not be started: " + err.Error()})
			return
		}
	}

	c.JSON(http.StatusCreated, kumbhaSessionResponse(sess))
}

// ListKumbhaSessions returns the active project's Kumbha build history,
// most recent first — the history view Kumbha has no console page of its
// own for otherwise (KUMBHA-DESIGN.md). Read-only: a way to find and
// revisit a past build, not to resume its conversation.
// GET /v1/kumbha/sessions
func (s *Server) ListKumbhaSessions(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return
	}

	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	sessions, err := s.kumbha.ListSessions(c.Request.Context(), accountID, projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	out := make([]gin.H, len(sessions))
	for i, sess := range sessions {
		out[i] = kumbhaSessionResponse(sess)
	}
	c.JSON(http.StatusOK, gin.H{"sessions": out, "count": len(out)})
}

// CloseKumbhaSession explicitly ends a session and settles its ledger —
// one usage_records line per (route, direction) the session touched,
// drawn against the account's credits.
// POST /v1/kumbha/sessions/:id/close
func (s *Server) CloseKumbhaSession(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
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

	sess, err := s.kumbha.CloseSession(c.Request.Context(), sessionID, accountID, "closed")
	if sess == nil {
		// Only store.Close itself failing returns a nil session — a
		// settlement error below still returns the (now-closed) session,
		// handled separately below.
		if errors.Is(err, kumbha.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		// A settlement error (RecordUsage/ConsumeCredit failing for one or
		// more route lines) does not make the session any less closed —
		// logged loudly as a billing-integrity gap needing operator
		// attention, but not surfaced as a customer-facing failure.
		log.Printf("WARN: kumbha session %s closed but settlement had errors: %v", sessionID, err)
	}

	c.JSON(http.StatusOK, kumbhaSessionResponse(sess))
}

// buildKumbhaSessionRequest names the Dockerfile to build and (via the
// session id itself) which workspace to build it from.
type buildKumbhaSessionRequest struct {
	DockerfilePath string `json:"dockerfile_path"`
}

// buildLogTailLimit bounds how much of a failed build's Kaniko output is
// echoed back in the tool result — enough to diagnose a broken Dockerfile,
// not an unbounded dump.
const buildLogTailLimit = 4000

// BuildKumbhaSession runs a Kaniko build of the session's agent workspace
// and pushes the result to the project's Harbor registry — what the
// "deploy" MCP verb calls once the customer has approved the deployment
// plan. The approval gate is enforced HERE too, not only in
// teepin-mcp-server: this is the actual provisioning boundary, and a
// compromised or buggy MCP process must not be able to trigger a real
// build by skipping its own client-side check.
// POST /v1/kumbha/sessions/:id/build
func (s *Server) BuildKumbhaSession(c *gin.Context) {
	if s.kumbha == nil || s.kumbhaBuild == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha build pipeline is not available on this deployment"})
		return
	}

	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}

	var req buildKumbhaSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.DockerfilePath == "" {
		req.DockerfilePath = "Dockerfile"
	}

	_, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	sess, err := s.kumbha.GetSession(c.Request.Context(), sessionID, accountID)
	if err != nil {
		if errors.Is(err, kumbha.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !sess.DeployApproved {
		c.JSON(http.StatusForbidden, gin.H{"error": "the customer has not approved the deployment plan yet", "code": "not_approved"})
		return
	}
	if sess.AgentInstanceID == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "this session has no agent running yet"})
		return
	}

	var logTail []string
	onLogLine := func(line string) {
		logTail = append(logTail, line)
		if len(logTail) > 200 {
			logTail = logTail[len(logTail)-200:] // keep only the most recent lines
		}
	}

	result, err := s.kumbhaBuild.Build(c.Request.Context(), build.Request{
		ProjectID: sess.ProjectID,
		// A synthetic, stable name rather than the account's real project
		// name: this handler has no project-name lookup wired, and Harbor
		// project uniqueness already comes from the project ID suffix
		// generateHarborProjectName appends, not from this label — it is
		// purely a Harbor-UI display convenience.
		ProjectName:     "project-" + sess.ProjectID.String()[:8],
		AgentInstanceID: sess.AgentInstanceID,
		DockerfilePath:  req.DockerfilePath,
		Tag:             sessionID.String()[:8],
	}, onLogLine)
	if err != nil {
		tail := joinTail(logTail, buildLogTailLimit)
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": err.Error(),
			"log":   tail,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"image_ref": result.ImageRef})
}

// joinTail joins log lines with newlines, trimmed from the FRONT to at
// most limit characters — the end of a build log (where the actual error
// usually is) matters more than the beginning.
func joinTail(lines []string, limit int) string {
	joined := ""
	for _, l := range lines {
		if joined != "" {
			joined += "\n"
		}
		joined += l
	}
	if len(joined) > limit {
		joined = joined[len(joined)-limit:]
	}
	return joined
}

// GetKumbhaSession returns a session's current status — budget, spend,
// and deploy_approved. Read by the console for the live budget meter, and
// by the Kumbha MCP tool server (cmd/kumbha-mcp-server) to check
// deploy_approved before any provisioning verb makes a real API call.
// GET /v1/kumbha/sessions/:id
func (s *Server) GetKumbhaSession(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
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

	sess, err := s.kumbha.GetSession(c.Request.Context(), sessionID, accountID)
	if err != nil {
		if errors.Is(err, kumbha.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, kumbhaSessionResponse(sess))
}

// ApproveKumbhaDeploy flips a session's pre-deploy cost-approval gate.
// The console calls this when the customer approves the itemised
// Deployment Plan (KUMBHA-DESIGN.md); the MCP tool server's provisioning
// verbs check this flag server-side before making any real teepin.* API
// call — a hard backend gate, not a UI-only confirmation a prompt-
// injected agent could talk its way past.
// POST /v1/kumbha/sessions/:id/approve-deploy
func (s *Server) ApproveKumbhaDeploy(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
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

	if err := s.kumbha.ApproveDeploy(c.Request.Context(), sessionID, accountID); err != nil {
		if errors.Is(err, kumbha.ErrSessionNotFound) {
			// Covers "not found", "wrong account", and "not open" alike —
			// approving a closed session's spend has nothing to apply to.
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found or no longer open"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	sess, err := s.kumbha.GetSession(c.Request.Context(), sessionID, accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, kumbhaSessionResponse(sess))
}

// CreateKumbhaEventTicket issues a short-lived, single-use ticket for the
// event-relay WebSocket attach step (GET .../events/attach) — the
// console's live activity feed. Mirrors CreateExecSession's shape
// exactly: resolve identity via normal auth, verify the session belongs
// to the caller, mint a ticket the browser presents on the (necessarily
// unauthenticated) WS handshake.
// POST /v1/kumbha/sessions/:id/events
func (s *Server) CreateKumbhaEventTicket(c *gin.Context) {
	if s.kumbha == nil || s.kumbhaEventTickets == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
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

	sess, err := s.kumbha.GetSession(c.Request.Context(), sessionID, accountID)
	if err != nil {
		if errors.Is(err, kumbha.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if sess.AgentInstanceID == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "this session has no agent running yet"})
		return
	}

	ticketID, secret, err := s.kumbhaEventTickets.Issue(kumbha.EventTicket{
		SessionID:       sess.ID,
		ProjectID:       sess.ProjectID,
		AccountID:       sess.AccountID,
		AgentInstanceID: sess.AgentInstanceID,
	})
	if err != nil {
		if errors.Is(err, kumbha.ErrEventTicketStoreFull) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "too many activity streams are starting right now; try again shortly"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ticket_id":     ticketID,
		"ticket_secret": secret,
		"attach_path":   "/v1/kumbha/sessions/" + sess.ID.String() + "/events/attach",
		"expires_in":    30,
	})
}

// parseCompletionRequest decodes an OpenAI-shaped chat-completions body
// into an inference.Request, preserving every field this package does not
// itself model (tool schemas, response_format, seed, ...) as Extra rather
// than silently dropping them — see pkg/inference's own doc comment on why
// Messages/Extra stay raw JSON.
func parseCompletionRequest(body []byte) (inference.Request, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return inference.Request{}, fmt.Errorf("invalid JSON body: %w", err)
	}

	var req inference.Request
	if v, ok := raw["model"]; ok {
		if err := json.Unmarshal(v, &req.Model); err != nil {
			return inference.Request{}, fmt.Errorf("invalid model: %w", err)
		}
	}
	if v, ok := raw["messages"]; ok {
		if err := json.Unmarshal(v, &req.Messages); err != nil {
			return inference.Request{}, fmt.Errorf("invalid messages: %w", err)
		}
	}
	if v, ok := raw["max_tokens"]; ok {
		if err := json.Unmarshal(v, &req.MaxTokens); err != nil {
			return inference.Request{}, fmt.Errorf("invalid max_tokens: %w", err)
		}
	}
	if v, ok := raw["stream"]; ok {
		if err := json.Unmarshal(v, &req.Stream); err != nil {
			return inference.Request{}, fmt.Errorf("invalid stream: %w", err)
		}
	}

	for _, known := range []string{"model", "messages", "max_tokens", "stream"} {
		delete(raw, known)
	}
	if len(raw) > 0 {
		req.Extra = raw
	}
	return req, nil
}

// KumbhaChatCompletions is the Kumbha Gateway's OpenAI-compatible chat
// completions endpoint — the request lifecycle specified in
// KUMBHA-DESIGN.md. Authenticate and resolve-session (stages 1-2) are
// transport concerns and live here; check-budget through accrue (stages
// 3-9) are pkg/kumbha.Gateway.Complete's job.
// POST /v1/kumbha/chat/completions
func (s *Server) KumbhaChatCompletions(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return
	}

	_, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	sessionIDStr := c.GetHeader("X-Teepin-Session")
	if sessionIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Teepin-Session header is required"})
		return
	}
	sessionID, err := uuid.Parse(sessionIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Teepin-Session is not a valid session id"})
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}
	req, err := parseCompletionRequest(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}
	if len(req.Messages) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "messages must not be empty"})
		return
	}
	if req.Stream {
		// Streaming lands after non-streaming is proven end to end, per the
		// Gateway's own build order (KUMBHA-DESIGN.md) — an explicit 400
		// beats silently buffering a streaming request into one blocking
		// response, matching VLLMProvider.Stream's "explicit error over
		// silent fallback" precedent.
		c.JSON(http.StatusBadRequest, gin.H{"error": "streaming is not yet available on this endpoint"})
		return
	}

	sess, err := s.kumbha.GetSession(c.Request.Context(), sessionID, accountID)
	if err != nil {
		if errors.Is(err, kumbha.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	result, err := s.kumbha.Complete(c.Request.Context(), sess, req)
	if err != nil {
		switch {
		case errors.Is(err, kumbha.ErrSessionClosed):
			c.JSON(http.StatusConflict, gin.H{"error": "session is closed", "code": "session_closed"})
		case errors.Is(err, kumbha.ErrBudgetExhausted):
			// The harness is expected to surface this to the customer as
			// "this run needs more budget — continue?" (KUMBHA-DESIGN.md),
			// which is why spent/budget ride the body, not just the status.
			c.JSON(http.StatusPaymentRequired, gin.H{
				"error": "session budget exhausted", "code": "budget_exhausted",
				"spent": sess.Spent, "budget": sess.Budget,
			})
		case errors.Is(err, inference.ErrUnknownModel):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "unknown_model"})
		case errors.Is(err, inference.ErrContextTooLarge):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "context_too_large"})
		case errors.Is(err, inference.ErrProviderUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "the model backend is temporarily unavailable"})
		case errors.Is(err, inference.ErrProviderRejected):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			log.Printf("WARN: kumbha completion failed for session %s: %v", sessionID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "completion failed"})
		}
		return
	}

	c.Header("X-Teepin-Cost", fmt.Sprintf("%.6f", result.Cost))
	c.Header("X-Teepin-Session-Spent", fmt.Sprintf("%.6f", result.Spent))
	c.Data(http.StatusOK, "application/json", result.Response.Body)
}
