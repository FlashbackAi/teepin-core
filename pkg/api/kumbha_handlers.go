// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/build"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
	"github.com/FlashbackAi/teepin-core/pkg/imageinfo"
	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
	"github.com/FlashbackAi/teepin-core/pkg/models"
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
// returns a session — creation, listing, and close. agent_running here is
// the CHEAP proxy (was a pod ever launched and never explicitly torn
// down), not a live cluster read — correct enough for a list of past
// builds, where a per-row live pod-status check would mean one cluster
// round trip per row on every poll. GetKumbhaSession (the single-session
// read the console detail page actually polls) overrides it with a real
// live check — see its own doc comment.
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
		// app_instance_id names the real compute instance this session's
		// deploy(s) produced (see Session.AppInstanceID's own doc
		// comment) — exposed so the console can link straight to
		// /compute/{id} and, on a fresh page load with no event history
		// yet, populate the Preview tab immediately rather than waiting
		// for the agent's own event stream to mention a URL (see
		// build/[id]/page.tsx's extractAppUrl, which this is the
		// complement to).
		"app_instance_id": sess.AppInstanceID,
		"started_at":      sess.StartedAt,
		"ended_at":        sess.EndedAt,
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

// deleteKumbhaSessionsRequest is the body of a bulk-delete call — a POST,
// not a DELETE with a query-string list, because the id list can
// plausibly be large (a customer clearing out most of their history) and
// this keeps the same request shape whether one or many are removed.
type deleteKumbhaSessionsRequest struct {
	IDs []string `json:"ids" binding:"required"`
}

// DeleteKumbhaSessions removes the given sessions from the account's
// "Previous builds" list — best-effort, not all-or-nothing: an id that is
// still open (an agent actively building) is silently skipped rather than
// failing the whole batch, same posture as Store.Delete. The response
// reports both sets explicitly so the console can tell the customer which
// selected rows, if any, are still in progress and could not be removed.
// POST /v1/kumbha/sessions/bulk-delete
func (s *Server) DeleteKumbhaSessions(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return
	}

	var req deleteKumbhaSessionsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids must not be empty"})
		return
	}

	ids := make([]uuid.UUID, 0, len(req.IDs))
	for _, raw := range req.IDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id: " + raw})
			return
		}
		ids = append(ids, id)
	}

	_, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	deleted, err := s.kumbha.DeleteSessions(c.Request.Context(), accountID, ids)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	deletedSet := make(map[uuid.UUID]bool, len(deleted))
	for _, id := range deleted {
		deletedSet[id] = true
	}
	skipped := make([]string, 0, len(ids)-len(deleted))
	for _, id := range ids {
		if !deletedSet[id] {
			skipped = append(skipped, id.String())
		}
	}

	deletedStr := make([]string, len(deleted))
	for i, id := range deleted {
		deletedStr[i] = id.String()
	}

	c.JSON(http.StatusOK, gin.H{"deleted": deletedStr, "skipped": skipped})
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

// workspaceFetchTokenTTL bounds the lifetime of the credential minted for
// the build pod's fetch-workspace initContainer — comfortably past
// build.DefaultConfig's own 15-minute build timeout, so the server (the
// build timing out) governs a stuck build rather than the fetch token
// itself expiring mid-build and turning into a confusing 401 instead of a
// clear timeout.
const workspaceFetchTokenTTL = 20 * time.Minute

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

	imageRef, status, body := s.buildKumbhaImage(c.Request.Context(), sess, accountID, req.DockerfilePath)
	if status != 0 {
		c.JSON(status, body)
		return
	}
	c.JSON(http.StatusOK, gin.H{"image_ref": imageRef})
}

// buildKumbhaImage is the shared build step behind BuildKumbhaSession
// (image only) and DeployKumbhaSession (image + real running instance):
// confirms a workspace exists, mints the fetch-workspace credential, and
// runs the Kaniko build. Returns status == 0 on success; otherwise status
// and body are exactly what the caller should write via c.JSON and
// return — kept here once so both callers give the customer an identically
// worded explanation for the same underlying failure.
func (s *Server) buildKumbhaImage(ctx context.Context, sess *kumbha.Session, accountID uuid.UUID, dockerfilePath string) (imageRef string, status int, body gin.H) {
	// Confirms there is something to build BEFORE minting a fetch token or
	// touching Harbor/Kaniko — a session with no saved version yet (the
	// agent hasn't written anything, or a build was triggered too early)
	// gets a clear, cheap 409 instead of a Kaniko pod that fails to unzip
	// an empty/missing archive.
	if _, err := s.kumbha.CurrentWorkspace(ctx, sess.ID, accountID); err != nil {
		if errors.Is(err, kumbha.ErrNoWorkspace) {
			return "", http.StatusConflict, gin.H{"error": "nothing has been saved for this build yet"}
		}
		return "", http.StatusInternalServerError, gin.H{"error": err.Error()}
	}

	// The build pod fetches the CURRENT workspace version over HTTP
	// (fetch-workspace initContainer) rather than mounting the agent's own
	// PVC — see MintWorkspaceFetchToken's doc comment for why: it's what
	// makes a customer's IDE edit or a rollback actually reach the image.
	fetchToken, archiveURL, err := s.kumbha.MintWorkspaceFetchToken(sess, workspaceFetchTokenTTL)
	if err != nil {
		if errors.Is(err, kumbha.ErrAgentNotConfigured) {
			return "", http.StatusNotFound, gin.H{"error": "the Kumbha build pipeline is not available on this deployment"}
		}
		return "", http.StatusInternalServerError, gin.H{"error": err.Error()}
	}

	var logTail []string
	onLogLine := func(line string) {
		logTail = append(logTail, line)
		if len(logTail) > 200 {
			logTail = logTail[len(logTail)-200:] // keep only the most recent lines
		}
	}

	result, err := s.kumbhaBuild.Build(ctx, build.Request{
		ProjectID: sess.ProjectID,
		// A synthetic, stable name rather than the account's real project
		// name: this handler has no project-name lookup wired, and Harbor
		// project uniqueness already comes from the project ID suffix
		// generateHarborProjectName appends, not from this label — it is
		// purely a Harbor-UI display convenience.
		ProjectName:         "project-" + sess.ProjectID.String()[:8],
		WorkspaceArchiveURL: archiveURL,
		WorkspaceToken:      fetchToken,
		DockerfilePath:      dockerfilePath,
		Tag:                 sess.ID.String()[:8],
	}, onLogLine)
	if err != nil {
		tail := joinTail(logTail, buildLogTailLimit)
		return "", http.StatusUnprocessableEntity, gin.H{"error": err.Error(), "log": tail}
	}
	return result.ImageRef, 0, nil
}

// detectKumbhaPorts reads imageRef's own manifest (the image just built
// and pushed) for its declared EXPOSE ports, so a deploy that didn't
// explicitly ask for any still ends up with a real public endpoint —
// found live 2026-08-26: an agent's deploy call routinely omitted
// `ports`, and asked directly, the customer's own — correct — reaction
// was that this shouldn't need to be something the agent (or a human)
// remembers to specify by hand when the built image already declares
// it, the same way `docker build`/`docker run` itself resolves EXPOSE
// through the image's layers, including ones inherited from FROM rather
// than restated in the customer's own Dockerfile (Tempo's own
// `FROM nginx:1.27-alpine` never re-declared EXPOSE 80 itself — nginx's
// own base image already does).
//
// Best-effort: any failure (registry unreachable, credential lookup
// failure, an image with genuinely no ExposedPorts) degrades to an empty
// result, same as today's behaviour before this existed — never blocks
// or fails the deploy itself over a convenience default.
func (s *Server) detectKumbhaPorts(ctx context.Context, projectID uuid.UUID, imageRef string) []models.PortMapping {
	if s.kumbhaBuild == nil {
		return nil
	}
	username, password, err := s.kumbhaBuild.ImageAuth(ctx, projectID)
	if err != nil {
		log.Printf("WARN: could not resolve registry credentials to auto-detect deploy ports for %s: %v", imageRef, err)
		return nil
	}
	found, err := imageinfo.ResolvePortsWithAuth(ctx, imageRef, username, password)
	if err != nil || len(found) == 0 {
		if err != nil {
			log.Printf("WARN: could not auto-detect deploy ports for %s: %v", imageRef, err)
		}
		return nil
	}
	ports := make([]models.PortMapping, len(found))
	for i, p := range found {
		ports[i] = models.PortMapping{Container: p.Port, Protocol: p.Protocol}
	}
	return ports
}

// deployKumbhaSessionRequest is what the console IDE's Deploy button
// sends. DockerfilePath aside, this is deliberately the same shape
// POST /v1/compute/instances itself accepts (Name/CPUUnits/MemoryGB/
// StorageGB/Ports/Env) — sizing the resulting app is the customer's own
// choice, not something Kumbha decides on their behalf.
type deployKumbhaSessionRequest struct {
	DockerfilePath string               `json:"dockerfile_path,omitempty"`
	Name           string               `json:"name,omitempty"`
	CPUUnits       int                  `json:"cpu_units,omitempty"`
	MemoryGB       int                  `json:"memory_gb,omitempty"`
	StorageGB      int                  `json:"storage_gb,omitempty"`
	Ports          []models.PortMapping `json:"ports,omitempty"`
	Env            map[string]string    `json:"env,omitempty"`
}

// DeployKumbhaSession builds the session's current workspace and deploys
// it as a real, customer-facing compute instance — the console IDE's
// Deploy button, and the thing that finally makes an IDE edit or a
// version rollback actually reachable at a URL, not just buildable.
//
// The FIRST deploy of a session reuses CreateInstance UNCHANGED via
// invokeInternally (see its own doc comment) rather than re-implementing
// GPU/home placement, the payment gate, endpoint provisioning, or the
// compute.instances billing row a second time — the resulting instance
// must be indistinguishable from one the customer created by hand, and
// duplicated logic is one drifted line away from silently not being
// that. Every deploy AFTER the first — sess.AppInstanceID already
// set — instead swaps the EXISTING instance's pod in place
// (redeployKumbhaInstance, cluster.Client.UpdateInstance): same
// instance ID, same hostname, same TLS cert, same compute.instances row,
// only the running code changes. Found live 2026-08-29: the prior
// create-a-new-instance-then-delete-the-old-one behaviour churned a
// fresh hostname and billing row on every single click, which is neither
// what "redeploy" means to a customer nor how any comparable platform
// (Vercel, Render, Fly) behaves.
// POST /v1/kumbha/sessions/:id/deploy
func (s *Server) DeployKumbhaSession(c *gin.Context) {
	if s.kumbha == nil || s.kumbhaBuild == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha build pipeline is not available on this deployment"})
		return
	}

	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}

	var req deployKumbhaSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.DockerfilePath == "" {
		req.DockerfilePath = "Dockerfile"
	}
	if req.CPUUnits <= 0 {
		req.CPUUnits = 1
	}
	if req.MemoryGB <= 0 {
		req.MemoryGB = 1
	}
	if req.Name == "" {
		req.Name = "kumbha-" + sessionID.String()[:8]
	}

	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}
	userID, _ := auth.GetUserID(c)

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

	imageRef, status, body := s.buildKumbhaImage(c.Request.Context(), sess, accountID, req.DockerfilePath)
	if status != 0 {
		c.JSON(status, body)
		return
	}

	ports := req.Ports
	if len(ports) == 0 {
		ports = s.detectKumbhaPorts(c.Request.Context(), sess.ProjectID, imageRef)
	}
	if status, body := validatePorts(ports); status != 0 {
		c.JSON(status, body)
		return
	}
	if status, body := validateStorageGB(req.StorageGB); status != 0 {
		c.JSON(status, body)
		return
	}

	if sess.AppInstanceID != "" {
		s.redeployKumbhaInstance(c, sessionID, sess, projectID, accountID, imageRef, ports, req.Env)
		return
	}

	createReq := models.CreateInstanceRequest{
		Name:      req.Name,
		Image:     imageRef,
		CPUUnits:  req.CPUUnits,
		Memory:    fmt.Sprintf("%dGB", req.MemoryGB),
		StorageGB: req.StorageGB,
		Ports:     ports,
		Env:       req.Env,
	}
	createStatus, createBody := s.invokeInternally(s.CreateInstance, http.MethodPost, "/v1/compute/instances", nil, accountID, projectID, userID, createReq)
	if createStatus != http.StatusCreated {
		// CreateInstance's own error already explains a payment gate,
		// invalid ports, or GPU/home capacity far better than a generic
		// message here would — surfaced verbatim rather than re-worded.
		c.Data(createStatus, "application/json", createBody)
		return
	}

	var created models.Instance
	if err := json.Unmarshal(createBody, &created); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "instance was created but its response could not be read"})
		return
	}

	// sess.AppInstanceID is always empty here — the branch above already
	// routed any session with an existing instance to
	// redeployKumbhaInstance instead — so this is unconditionally the
	// FIRST instance this session has ever created, never a replacement.
	if err := s.kumbha.SetAppInstanceID(c.Request.Context(), sessionID, created.ID); err != nil {
		// The instance is real and running; only OUR bookkeeping of which
		// one belongs to this session failed. Not worth failing the
		// customer's deploy over — logged so an operator can reconcile —
		// the worst outcome is the NEXT deploy failing to find it and
		// creating a second instance instead of redeploying this one, not
		// the customer losing the app they just got.
		log.Printf("WARN: deployed instance %s for Kumbha session %s but failed to record it: %v", created.ID, sessionID, err)
	}

	// The customer-visible "Version history" only ever gains an entry
	// here — a successful deploy — not on every intermediate agent write
	// (see CheckpointWorkspace/SaveVersion). Best-effort, same posture as
	// SetAppInstanceID just above: the app is real and running; failing to
	// flip a bookkeeping flag is not worth failing the customer's deploy
	// over, only worth logging so it can be reconciled.
	if err := s.kumbha.CheckpointWorkspace(c.Request.Context(), sessionID); err != nil {
		log.Printf("WARN: deployed Kumbha session %s but failed to checkpoint its workspace version: %v", sessionID, err)
	}

	c.JSON(http.StatusOK, gin.H{
		"image_ref":      imageRef,
		"instance_id":    created.ID,
		"endpoint":       created.Endpoint,
		"status":         created.Status,
		"price_per_hour": created.PricePerHour,
	})
}

// redeployKumbhaInstance is DeployKumbhaSession's path for every deploy
// after the session's first: sess.AppInstanceID already names a real,
// running compute instance, so this swaps its pod for one running
// imageRef via cluster.Client.UpdateInstance instead of creating (and
// then tearing down) a second instance. Same instance ID, same
// Service/Ingress/hostname/TLS cert, same compute.instances row — a
// customer watching the Preview tab sees the same URL start serving new
// code, not a URL that just changed out from under them.
//
// CPU/Memory/StorageGB are read from the EXISTING record rather than
// req — a redeploy changes what code runs, not how big the instance is;
// resizing an existing instance is a different, unbuilt feature (and one
// the console's own Deploy button has never offered a control for).
func (s *Server) redeployKumbhaInstance(c *gin.Context, sessionID uuid.UUID, sess *kumbha.Session, projectID, accountID uuid.UUID, imageRef string, ports []models.PortMapping, env map[string]string) {
	if s.store == nil {
		// Persistence is what remembers an existing instance's sizing and
		// what UpdateImage reconciles afterward — without it there is no
		// safe way to redeploy in place at all (see the CPU/Memory comment
		// above). Standalone mode never sets AppInstanceID in the first
		// place (SetAppInstanceID is itself a no-op without a store), so
		// this should be unreachable outside a misconfiguration.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot redeploy: no instance store configured"})
		return
	}

	existing, err := s.store.Get(c.Request.Context(), sess.AppInstanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if existing == nil || existing.AccountID != accountID {
		// Another tenant's instance is indistinguishable from a missing
		// one, same as GetInstance/DeleteInstance elsewhere in this file —
		// existence must not leak. In practice this session's own
		// AppInstanceID can only ever have been set by ITS OWN earlier
		// deploy (SetAppInstanceID, above), so a mismatch here means the
		// instance was deleted out from under the session some other way
		// (e.g. by hand in the Compute page) rather than a real tenancy
		// violation — surfaced as 404 either way, since "redeploy" has
		// nothing left to target.
		c.JSON(http.StatusNotFound, gin.H{"error": "this session's previously deployed instance no longer exists"})
		return
	}

	req := models.CreateInstanceRequest{
		Name:      existing.Name,
		Image:     imageRef,
		CPUUnits:  existing.CPUUnits,
		Memory:    fmt.Sprintf("%dGB", existing.MemoryGB),
		StorageGB: existing.StorageGB,
		Ports:     ports,
		Env:       env,
	}
	spec := s.instanceSpec(existing.ID, endpointUUIDFor(existing.ID), projectID, accountID, &req, nil)

	result, err := s.cluster.UpdateInstance(c.Request.Context(), scopeFor(projectID), spec)
	if err != nil {
		if errors.Is(err, cluster.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "this session's previously deployed instance no longer exists in the cluster"})
			return
		}
		if errors.Is(err, cluster.ErrClusterUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "no GPU capacity is reachable right now; the existing instance is unaffected",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to redeploy instance: %v", err)})
		return
	}

	containerPort := 0
	if len(ports) > 0 {
		containerPort = ports[0].Container
	}
	if err := s.store.UpdateImage(c.Request.Context(), existing.ID, imageRef, result.PodName, containerPort); err != nil {
		// The pod is real and running the new code; only OUR record of
		// which image/pod it is now on failed to update. Not worth
		// failing the customer's redeploy over — logged so an operator
		// can reconcile — the instance is still reachable at its
		// unchanged hostname regardless.
		log.Printf("WARN: redeployed instance %s for Kumbha session %s but failed to update its record: %v", existing.ID, sessionID, err)
	}

	if err := s.kumbha.CheckpointWorkspace(c.Request.Context(), sessionID); err != nil {
		log.Printf("WARN: redeployed Kumbha session %s but failed to checkpoint its workspace version: %v", sessionID, err)
	}

	c.JSON(http.StatusOK, gin.H{
		"image_ref":   imageRef,
		"instance_id": existing.ID,
		// The endpoint is UNCHANGED by a redeploy — same Service/Ingress,
		// same hostname — so it is read back from the existing record
		// rather than from `result`, which (being a pod-only replace)
		// never re-provisions or re-reports it.
		"endpoint":       existing.Endpoint,
		"status":         compute.StatusPending,
		"price_per_hour": 0.0, // Kumbha apps are CPU-only; see CreateInstance's own note that this is only ever non-zero for a GPU allocation.
	})
}

// invokeInternally calls a gin.HandlerFunc directly, as if it had received
// a real HTTP request, using a synthetic context carrying the given
// identity — the exact technique this package's own tests already use to
// drive a handler without a live server. Used ONLY so DeployKumbhaSession
// can reuse CreateInstance/DeleteInstance's real logic (GPU/home
// placement, the payment gate, endpoint provisioning, the billing row)
// rather than re-implementing any of it: an app instance a Kumbha deploy
// creates must be indistinguishable from one the customer created by hand,
// and a second copy of that logic is one drifted line away from silently
// not being that. userID is optional (zero value omitted) — CreateInstance
// itself treats a missing one as "no attribution", the same as a normal
// request whose JWT happens to carry none.
func (s *Server) invokeInternally(handler gin.HandlerFunc, method, path string, params gin.Params, accountID, projectID, userID uuid.UUID, body any) (status int, respBody []byte) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	var bodyReader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	c.Request = httptest.NewRequest(method, path, bodyReader)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = params
	c.Set(string(auth.AccountIDKey), accountID)
	c.Set(string(auth.ProjectIDKey), projectID)
	if userID != uuid.Nil {
		c.Set(string(auth.UserIDKey), userID)
	}

	handler(c)
	return w.Code, w.Body.Bytes()
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
// deploy_approved, and (unlike every other endpoint returning a session —
// see kumbhaSessionResponse's own comment) a LIVE read of whether the
// agent is actually still running and what state its deployed app is
// actually in, not the cheap "was one ever launched" proxies. This is the
// endpoint the console's build detail page polls every few seconds (see
// budget-meter.tsx), and by the Kumbha MCP tool server
// (cmd/kumbha-mcp-server) to check deploy_approved before any
// provisioning verb makes a real API call — worth one extra cluster read
// or two here for a UI that needs "Building" to stop being true the
// moment the agent pod actually exits, and "Deployed"/"Error" to reflect
// the app's real status rather than staying frozen at whatever
// buildKumbhaImage last reported. Found live 2026-08-29: a session's
// status stayed "Building" (agent_running derived only from
// agent_instance_id != "") long after the agent pod had exited, and the
// deployed app's own health was not visible anywhere in this response at
// all.
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

	projectID, accountID, ok := s.requireScope(c)
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

	resp := kumbhaSessionResponse(sess)

	// Live agent status, not the "was one ever launched and never torn
	// down" proxy the shared response defaults to. Best-effort: a cluster
	// read failure degrades to the cheap proxy already in resp rather than
	// failing the whole session read over a UI nicety.
	if sess.AgentInstanceID != "" {
		if running, err := s.kumbha.IsAgentRunning(c.Request.Context(), sess); err == nil {
			resp["agent_running"] = running
		}
	}

	// The deployed app's own live status — "pending"/"running"/"failed"/
	// "terminated", cluster's own vocabulary (see compute.Status* and
	// statusToInstance) — is what actually answers "did the last deploy
	// work", which agent_running alone cannot: the agent can finish (or
	// never have been running this whole time, on a page reload) while
	// the app it deployed is crash-looping. Empty when no deploy has
	// happened yet, or the read failed — never guessed.
	if sess.AppInstanceID != "" && s.cluster != nil {
		if st, err := s.cluster.GetInstanceStatus(c.Request.Context(), scopeFor(projectID), sess.AppInstanceID); err == nil {
			resp["app_status"] = st.Status
			resp["app_status_message"] = st.Message
			// The endpoint too, straight off the same live read — this is
			// what lets the console populate its Preview tab and "open the
			// instance" link the moment a deploy succeeds, whether it was
			// the agent's own deploy call or the console IDE's Deploy
			// button, without waiting for (or parsing) an event summary.
			resp["app_endpoint"] = st.EndpointURL
		}
	}

	c.JSON(http.StatusOK, resp)
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

// updateKumbhaBudgetRequest is the console's "raise budget" control on
// the live budget meter — the replacement for the composer's old
// up-front budget picker (see build/page.tsx and the composer's own
// removed BUDGET_PRESETS).
type updateKumbhaBudgetRequest struct {
	Budget float64 `json:"budget" binding:"required"`
}

// UpdateKumbhaBudget raises an open session's pre-authorised spend cap.
// A raise only — see Gateway.IncreaseBudget's own doc comment for why a
// lower or equal value is rejected rather than silently accepted as a
// no-op.
// PATCH /v1/kumbha/sessions/:id/budget
func (s *Server) UpdateKumbhaBudget(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return
	}

	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}

	var req updateKumbhaBudgetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	_, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	if err := s.kumbha.IncreaseBudget(c.Request.Context(), sessionID, accountID, req.Budget); err != nil {
		switch {
		case errors.Is(err, kumbha.ErrSessionNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		case errors.Is(err, kumbha.ErrSessionClosed):
			c.JSON(http.StatusConflict, gin.H{"error": "this session is no longer open"})
		case errors.Is(err, kumbha.ErrBudgetNotIncreased):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, kumbha.ErrPaymentRequired):
			c.JSON(http.StatusPaymentRequired, gin.H{"error": err.Error(), "code": "payment_method_required"})
		case errors.Is(err, kumbha.ErrGateUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unable to verify billing status, please retry"})
		default:
			// A cap-exceeded rejection from the store's own defence-in-depth
			// check lands here — a plain 400, since it is the customer's
			// requested value that is wrong, not a server problem.
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
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

// sendKumbhaMessageRequest is the console chat input's body.
type sendKumbhaMessageRequest struct {
	Content string `json:"content"`
}

// SendKumbhaMessage is "chat + resume" from the customer's side — the
// console's chat input, sending a follow-up instruction after the
// session's initial prompt. Gateway.DeliverMessage decides whether that
// reaches the SAME still-running agent process (queued for its own poll
// loop, full conversation memory intact) or launches a fresh one (the
// previous pod already exited) — see its own doc comment for why the
// latter is an honest, bounded degradation rather than real persistence.
// POST /v1/kumbha/sessions/:id/messages
func (s *Server) SendKumbhaMessage(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return
	}

	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}

	var req sendKumbhaMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
	if sess.Status != "open" {
		c.JSON(http.StatusConflict, gin.H{"error": "session is closed", "code": "session_closed"})
		return
	}

	relaunched, err := s.kumbha.DeliverMessage(c.Request.Context(), sess, req.Content)
	if err != nil {
		switch {
		case errors.Is(err, kumbha.ErrEmptyMessage), errors.Is(err, kumbha.ErrMessageTooLong):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, kumbha.ErrAgentNotConfigured):
			c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha agent is not available on this deployment"})
		case errors.Is(err, kumbha.ErrSessionClosed):
			c.JSON(http.StatusConflict, gin.H{"error": "session is closed", "code": "session_closed"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"delivered": true, "relaunched": relaunched})
}

// PollKumbhaMessages is the agent's own side of "chat + resume" —
// run.py's wait_for_next_instruction poll loop. Session-scoped credential
// only (auth.GetSessionID), matching UploadKumbhaWorkspace's own auth
// shape: the agent may only ever poll its OWN session, never argue its way
// into another one via the path parameter.
// GET /v1/kumbha/sessions/:id/messages/poll
func (s *Server) PollKumbhaMessages(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return
	}

	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}

	callerSession, ok := auth.GetSessionID(c)
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "this endpoint requires a Kumbha session credential"})
		return
	}
	if callerSession != sessionID {
		c.JSON(http.StatusForbidden, gin.H{"error": "this credential does not belong to that session"})
		return
	}

	messages, err := s.kumbha.PollMessages(c.Request.Context(), sessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	out := make([]gin.H, len(messages))
	for i, m := range messages {
		out[i] = gin.H{"id": m.ID, "content": m.Content, "created_at": m.CreatedAt}
	}
	c.JSON(http.StatusOK, gin.H{"messages": out})
}
