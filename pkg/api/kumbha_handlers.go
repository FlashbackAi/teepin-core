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
// returns a session — creation and listing (there is no "close" endpoint
// any more, see StopKumbhaAgent's own doc comment). agent_running here is
// the CHEAP proxy (was a pod ever launched and never explicitly torn
// down), not a live cluster read — correct enough for a list of past
// builds, where a per-row live pod-status check would mean one cluster
// round trip per row on every poll. GetKumbhaSession/ListKumbhaSessions
// (see enrichKumbhaAgentRunning) override it with a real live check.
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
		// The outcome of the most recent build/deploy attempt (see
		// migration 030 and Session.LastDeployFailed's own doc comment) —
		// what backs the "Failed" status on the console's "Previous
		// builds" list. Cheap: a stored column, not a live read, so this
		// is safe on the list endpoint too, unlike app_status.
		"last_deploy_failed": sess.LastDeployFailed,
		"last_deploy_error":  sess.LastDeployError,
		"started_at":         sess.StartedAt,
		"ended_at":           sess.EndedAt,
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
		resp := kumbhaSessionResponse(sess)
		// Live agent_running only, not the app-status enrichment
		// GetKumbhaSession also does — see enrichKumbhaAppStatus's own doc
		// comment on why a per-row live cluster read is a cost this list
		// deliberately does not pay.
		s.enrichKumbhaAgentRunning(c.Request.Context(), resp, sess)
		out[i] = resp
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

// StopKumbhaAgent interrupts a session's currently-running agent pod
// immediately — replaces the old "Close session" button, which bundled
// this with settling billing (now continuous, see Gateway.Complete's own
// doc comment) and permanently blocking further chat (removed: nothing
// about stopping a run should stop the customer from sending another
// message afterward — DeliverMessage's own relaunch path already handles
// starting a fresh agent turn from wherever the workspace was left). See
// Gateway.StopAgent's own doc comment for why this is a hard kill, not a
// graceful mid-turn pause.
// POST /v1/kumbha/sessions/:id/stop
func (s *Server) StopKumbhaAgent(c *gin.Context) {
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

	if err := s.kumbha.StopAgent(c.Request.Context(), sess); err != nil {
		if errors.Is(err, kumbha.ErrAgentNotRunning) {
			c.JSON(http.StatusConflict, gin.H{"error": "nothing is currently running for this session", "code": "not_running"})
			return
		}
		if errors.Is(err, kumbha.ErrAgentNotConfigured) {
			c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha agent is not available on this deployment"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"stopped": true})
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
		s.recordDeployOutcome(c.Request.Context(), sessionID, errorFromBody(body))
		c.JSON(status, body)
		return
	}
	s.recordDeployOutcome(c.Request.Context(), sessionID, "")
	c.JSON(http.StatusOK, gin.H{"image_ref": imageRef})
}

// recordDeployOutcome persists the outcome of a build/deploy attempt on
// the session (see kumbha.Store.SetLastDeployStatus) — errMsg empty means
// success, clearing any earlier failure so a stale "Failed" from days ago
// never lingers once the customer has since shipped successfully.
// Best-effort: a failure to persist this bookkeeping is logged, never
// surfaced to the customer or allowed to mask the real build/deploy
// result already being returned in the response this call sits next to.
func (s *Server) recordDeployOutcome(ctx context.Context, sessionID uuid.UUID, errMsg string) {
	if err := s.kumbha.SetLastDeployStatus(ctx, sessionID, errMsg); err != nil {
		log.Printf("WARN: could not record deploy outcome for Kumbha session %s: %v", sessionID, err)
	}
}

// errorFromBody extracts the customer-facing message from a handler's own
// gin.H{"error": ...} response body — every failure path in this file
// builds its body that way, so this is a safe, narrow way to reuse that
// exact string as the persisted last_deploy_error rather than a second,
// possibly-drifted description of the same failure.
func errorFromBody(body gin.H) string {
	if msg, ok := body["error"].(string); ok {
		return msg
	}
	return "deploy failed"
}

// errorFromJSONBody is errorFromBody for a raw JSON []byte response body
// (invokeInternally's shape) rather than a gin.H — same narrow purpose:
// reuse CreateInstance's own already-worded error as the persisted
// last_deploy_error instead of a second, possibly-drifted description.
func errorFromJSONBody(body []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Error == "" {
		return "deploy failed"
	}
	return parsed.Error
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

// errNoResolvablePort is returned when a deploy has no explicit port, no
// image-declared EXPOSE port, and (on a redeploy) no previously-known
// port to fall back to either — see the two call sites' own doc comments
// for the live incident this closes. Worded for BOTH readers: a human in
// the console and the Kumbha agent itself reading this same text back as
// an MCP tool-result error it needs to act on.
const errNoResolvablePort = "could not determine which port your app listens on: the built image declares no EXPOSE port and none was specified explicitly. Add an EXPOSE line to the Dockerfile for the port your app listens on (e.g. \"EXPOSE 3000\"), or pass it explicitly as \"ports\" in the deploy request."

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
		} else {
			// resolveConfigPorts (pkg/imageinfo) deliberately collapses
			// "pulled fine, zero EXPOSE ports declared" and "pull itself
			// failed" into the same silent (nil, nil) — correct for its
			// OTHER caller (an arbitrary customer-typed image string,
			// where neither outcome is noteworthy), but for a Kumbha
			// deploy this image was built by OUR OWN pipeline seconds
			// ago, so a failed pull here would be a real, unexpected
			// problem worth seeing. Found live 2026-08-31 while chasing a
			// silently-broken screenshot capture back to a deploy with no
			// resolvable port at all and NOTHING in the logs to explain
			// why — the caller (DeployKumbhaSession/redeployKumbhaInstance)
			// now turns this into a real error reaching the customer/agent,
			// but an operator watching logs should see it happened too.
			log.Printf("WARN: auto-detect found no EXPOSE ports for %s (either the image declares none, or the pull itself failed silently)", imageRef)
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
// deployFlowTimeout bounds the detached context DeployKumbhaSession uses
// for its own work (see the doc comment on that context below) —
// comfortably past build.DefaultConfig().Timeout (15 minutes) plus the
// redeploy/checkpoint steps after it, same margin reasoning as
// cmd/kumbha-mcp-server's own deployHTTPTimeout.
const deployFlowTimeout = 18 * time.Minute

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

	// Detached from c.Request.Context() from here on: the build+redeploy
	// flow below can legitimately run for minutes (buildKumbhaImage's own
	// 15-minute ceiling), and tying it to the incoming HTTP connection
	// meant ANY client-side disconnect — a page refresh, a closed tab, an
	// impatient navigate-away while watching a bare loading spinner with
	// no progress feedback — silently abandoned the deploy mid-flight:
	// the Kaniko pod kept building in the cluster regardless, but nothing
	// on the control plane was left to swap the instance's image,
	// checkpoint the workspace, or clear last_deploy_failed once it
	// finished. Found live 2026-08-31: a build whose own Kaniko logs
	// showed it completed and pushed successfully, yet the instance's
	// stored image never updated and last_deploy_failed stayed stuck on
	// an unrelated timeout from 23 minutes earlier — exactly this
	// abandonment. A fresh context.Background() here means the flow runs
	// to completion (bounded by deployFlowTimeout) even if nobody is left
	// listening for the HTTP response — c is still used below for
	// everything that only needs the ORIGINAL request (param/body
	// parsing, already done above, and c.JSON/c.Data to answer it if the
	// connection is still alive).
	ctx, cancel := context.WithTimeout(context.Background(), deployFlowTimeout)
	defer cancel()

	sess, err := s.kumbha.GetSession(ctx, sessionID, accountID)
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

	imageRef, status, body := s.buildKumbhaImage(ctx, sess, accountID, req.DockerfilePath)
	if status != 0 {
		s.recordDeployOutcome(ctx, sessionID, errorFromBody(body))
		c.JSON(status, body)
		return
	}

	ports := req.Ports
	if len(ports) == 0 {
		ports = s.detectKumbhaPorts(ctx, sess.ProjectID, imageRef)
	}
	if status, body := validatePorts(ports); status != 0 {
		s.recordDeployOutcome(ctx, sessionID, errorFromBody(body))
		c.JSON(status, body)
		return
	}
	if status, body := validateStorageGB(req.StorageGB); status != 0 {
		s.recordDeployOutcome(ctx, sessionID, errorFromBody(body))
		c.JSON(status, body)
		return
	}

	if sess.AppInstanceID != "" {
		s.redeployKumbhaInstance(ctx, c, sessionID, sess, projectID, accountID, imageRef, ports, req.Env)
		return
	}

	// A first deploy with zero resolvable ports used to succeed silently,
	// producing an instance with no endpoint at all — unreachable, with
	// nothing in the response or the logs to say why (detectKumbhaPorts
	// degrades a "pulled the image fine, it just declares no EXPOSE" case
	// to an empty result with NO log line at all, indistinguishable from
	// every other best-effort no-op it also swallows). Root-caused live
	// 2026-08-31 chasing a silently-broken screenshot capture back to
	// exactly this: an instance deployed with no endpoint, no error, no
	// trace of why. Erroring out here, in the SAME call the deploy MCP
	// tool makes, reaches the agent as a real tool-result error it can
	// act on directly (add an EXPOSE line to the Dockerfile, or pass an
	// explicit port) — far more reliable than trying to prompt the model
	// into always remembering EXPOSE, and it stops a human customer's
	// deploy from ever producing an unreachable app silently, too.
	if len(ports) == 0 {
		s.recordDeployOutcome(ctx, sessionID, errNoResolvablePort)
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": errNoResolvablePort})
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
	// The actual root of the whole endpoint saga, one level deeper than
	// redeployKumbhaInstance's own NodeClass/ProviderID fix above: THIS
	// first-deploy request never set node_class either, so CreateInstance's
	// handler (server.go) never entered its home-placement branch — meaning
	// spec.ProviderID/spec.NodeClass were never set on the ORIGINAL create,
	// and compute.instances.provider_id was persisted empty from day one,
	// for every Kumbha app ever deployed, not just this one instance.
	// Confirmed directly from this incident's own earlier diagnostic query
	// (`provider_id` came back blank for inst-5ed29952) — which is exactly
	// why the redeploy fix above, correct on its own terms, could never
	// fire: existing.ProviderID had nothing to read. The pod-placement
	// itself still landed on the home node regardless (createOrReplace's
	// dispatch falls back to registry.Any() with an empty ProviderID,
	// harmless with exactly one connected provider) — same reason this
	// stayed invisible: it broke endpoint SYNTHESIS, never the deploy
	// itself. Kumbha's own agent-pod launch (pkg/kumbha/agent.go's
	// LaunchAgent) has this identical gap and relies on the same Any()
	// fallback — nothing in Kumbha's code has ever explicitly requested
	// home placement. Guarded on s.nodePlacer != nil (the same guard
	// CreateInstance's own handler already enforces) so this cannot turn
	// into a NEW failure mode on a deployment without home compute
	// configured — it only starts requesting what was always the de facto
	// reality everywhere home compute already exists.
	if s.nodePlacer != nil {
		createReq.NodeClass = "home"
	}
	createStatus, createBody := s.invokeInternally(s.CreateInstance, http.MethodPost, "/v1/compute/instances", nil, accountID, projectID, userID, sessionID, createReq)
	if createStatus != http.StatusCreated {
		s.recordDeployOutcome(ctx, sessionID, errorFromJSONBody(createBody))
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
	if err := s.kumbha.SetAppInstanceID(ctx, sessionID, created.ID); err != nil {
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
	if err := s.kumbha.CheckpointWorkspace(ctx, sessionID); err != nil {
		log.Printf("WARN: deployed Kumbha session %s but failed to checkpoint its workspace version: %v", sessionID, err)
	}

	s.triggerGithubPush(sessionID, accountID, imageRef)
	s.triggerScreenshotCapture(sessionID, sess, created.Endpoint)
	s.recordDeployOutcome(ctx, sessionID, "")

	c.JSON(http.StatusOK, gin.H{
		"image_ref":      imageRef,
		"instance_id":    created.ID,
		"endpoint":       created.Endpoint,
		"status":         created.Status,
		"price_per_hour": created.PricePerHour,
	})
}

// triggerScreenshotCapture kicks off a best-effort deployment thumbnail
// capture in the background — called right after a deploy or redeploy
// succeeds, from both DeployKumbhaSession and redeployKumbhaInstance. Runs
// detached from the request context (which is about to be torn down the
// moment this handler returns) with its own bounded timeout, and never
// blocks or fails the deploy response: a missing thumbnail is cosmetic,
// see Gateway.CaptureScreenshot's own doc comment. A no-op when
// targetURL is empty (no exposed port — nothing to screenshot) or the
// Gateway isn't configured at all.
func (s *Server) triggerScreenshotCapture(sessionID uuid.UUID, sess *kumbha.Session, targetURL string) {
	if s.kumbha == nil || targetURL == "" {
		return
	}
	go func() {
		// Was a separate hardcoded 90*time.Second, itself already shorter
		// than kumbha.CaptureTimeoutDefault's OWN prior 45s value would
		// suggest was intended — and since a nested context.WithTimeout
		// always resolves to the earlier deadline, this outer value
		// silently capped CaptureScreenshot's own internal timeout no
		// matter how high that one was raised. Found live 2026-08-31: a
		// real capture pod was still pulling its image at 157s, past BOTH
		// the old 45s inner bound and this old 90s outer one. Deriving
		// from the exported constant (plus a margin for this goroutine's
		// own dispatch/scheduling overhead) means there is exactly one
		// number to reason about, not two that can silently drift apart.
		ctx, cancel := context.WithTimeout(context.Background(), kumbha.CaptureTimeoutDefault+10*time.Second)
		defer cancel()
		if err := s.kumbha.CaptureScreenshot(ctx, sess, targetURL); err != nil {
			log.Printf("WARN: screenshot capture failed for Kumbha session %s: %v", sessionID, err)
		}
	}()
}

// triggerGithubPush kicks off a best-effort push of the session's current
// workspace snapshot to its Teepin-owned GitHub repo (pkg/githubstore) —
// called right after a deploy or redeploy succeeds, mirroring
// triggerScreenshotCapture's own shape and reasoning: a slow or
// unreachable third-party API call must never add latency to the
// deploy response itself, and a failure here is cosmetic (a missing
// backup copy, not a failed deployment) — same posture
// CheckpointWorkspace's own call sites already use for their bookkeeping
// failures. A no-op when s.githubStore is nil (feature not configured).
//
// Provisions the repo lazily, on the session's FIRST push, and persists
// it (kumbha.SetGithubRepo) so every later push reuses it rather than
// re-checking with GitHub every time — see pkg/githubstore.ProvisionRepo
// and Store.GetGithubRepo/SetGithubRepo's own doc comments for why this
// is deliberately not a Session field.
func (s *Server) triggerGithubPush(sessionID, accountID uuid.UUID, imageRef string) {
	if s.githubStore == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		snap, err := s.kumbha.CurrentWorkspace(ctx, sessionID, accountID)
		if err != nil {
			log.Printf("WARN: could not load workspace to push to GitHub storage for Kumbha session %s: %v", sessionID, err)
			return
		}

		repo, err := s.kumbha.GetGithubRepo(ctx, sessionID)
		if err != nil {
			log.Printf("WARN: could not check GitHub storage repo for Kumbha session %s: %v", sessionID, err)
			return
		}
		if repo == "" {
			repo, err = s.githubStore.ProvisionRepo(ctx, sessionID)
			if err != nil {
				log.Printf("WARN: could not provision GitHub storage repo for Kumbha session %s: %v", sessionID, err)
				return
			}
			if err := s.kumbha.SetGithubRepo(ctx, sessionID, repo); err != nil {
				log.Printf("WARN: provisioned GitHub storage repo for Kumbha session %s but failed to record it: %v", sessionID, err)
			}
		}

		if err := s.githubStore.PushSnapshot(ctx, sessionID, snap.Files, "Deploy: "+imageRef); err != nil {
			log.Printf("WARN: could not push Kumbha session %s to GitHub storage: %v", sessionID, err)
		}
	}()
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
// ctx is the detached, DeployKumbhaSession-owned context (see its own doc
// comment) — used for every piece of real work below; c is kept only to
// answer the original HTTP request (c.JSON), which may or may not still
// have anyone listening by the time this returns.
func (s *Server) redeployKumbhaInstance(ctx context.Context, c *gin.Context, sessionID uuid.UUID, sess *kumbha.Session, projectID, accountID uuid.UUID, imageRef string, ports []models.PortMapping, env map[string]string) {
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

	existing, err := s.store.Get(ctx, sess.AppInstanceID)
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

	// A redeploy's own doc comment above promises "same instance ID, same
	// hostname, same TLS cert ... only the running code changes" — but
	// detectKumbhaPorts is best-effort (registry auth hiccup, image not
	// yet fully propagated, any transient failure silently returns nil,
	// see its own doc comment) and DeployKumbhaSession re-runs it on
	// EVERY redeploy, not just the first. An empty result here reaches
	// cluster.UpdateInstance as spec.Ports == nil, which skips endpoint
	// synthesis entirely (agent.go's createOrReplace, home-class path)
	// and seeds the status cache with an all-empty endpoint — which the
	// reconciler then persists over the instance's previously-correct
	// endpoint/dns_name in compute.instances. Found live 2026-08-31:
	// inst-5ed29952 lost its endpoint this way after a routine redeploy,
	// silently breaking the screenshot service (triggerScreenshotCapture
	// no-ops on an empty target URL) with no error surfaced anywhere.
	// Falling back to the instance's own already-known port when
	// detection comes back empty keeps a redeploy from ever re-deriving
	// (and risking the loss of) an endpoint that already exists.
	if len(ports) == 0 && existing.ContainerPort > 0 {
		ports = []models.PortMapping{{Container: existing.ContainerPort, Protocol: "tcp"}}
	}

	// Neither a fresh detection NOR the existing instance's own record has
	// a port to offer — silently proceeding here would redeploy with no
	// endpoint at all, exactly the incident this function's fix above
	// closes for the common case. This is the case that fix cannot cover
	// on its own: an instance that never had a known-good port to fall
	// back to in the first place (e.g. one whose compute.instances row was
	// reconstructed by hand after going missing, rather than through a
	// normal deploy). Surfacing this as a real error — reaching the Kumbha
	// agent verbatim as its own deploy tool's result — is what actually
	// lets it get fixed (an EXPOSE line, or an explicit port), rather than
	// silently repeating the same unreachable deploy forever.
	if len(ports) == 0 {
		s.recordDeployOutcome(ctx, sessionID, errNoResolvablePort)
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": errNoResolvablePort})
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

	// instanceSpec alone never sets NodeClass/NodeName/ProviderID — on a
	// FIRST deploy, CreateInstance's own handler sets all three itself
	// from a fresh placement decision (server.go: "if homePlacement != nil
	// { spec.NodeClass = "home"; spec.NodeName = ...; spec.ProviderID =
	// ... }"), immediately after calling this same instanceSpec. A
	// redeploy must NOT re-run placement — the instance stays on the SAME
	// node/provider it already lives on — so instead of a fresh decision,
	// this reads the already-persisted values back from the existing
	// record (compute.instances.provider_id/node_name, populated at the
	// original create). Without this, EVERY Kumbha redeploy sent
	// spec.NodeClass == "" (the zero value) — meaning createOrReplace's
	// home-class endpoint-synthesis gate (agent.go: "if spec.NodeClass ==
	// "home" && len(spec.Ports) > 0") was structurally UNREACHABLE from
	// this function, regardless of whether ports resolved correctly. The
	// underlying pod-replace command still dispatched fine (ProviderID
	// empty just falls back to registry.Any(), harmless with a single
	// connected provider), which is exactly why this was invisible until
	// an instance's own database endpoint needed to be RECOVERED by a
	// redeploy rather than merely left unchanged — result.EndpointURL was
	// unconditionally empty on every redeploy, making the existing.
	// Endpoint/result.EndpointURL fallback above unable to ever actually
	// help until this is fixed too. Found live 2026-08-31, the true root
	// of the whole inst-5ed29952 endpoint saga: home-class placement is
	// (deliberately, per CreateInstance's own comment) opt-in only and
	// unreachable without it, so ProviderID != "" is the same signal
	// ResolveProvider (cmd/api-server/adapters.go) already uses elsewhere
	// to mean "this is a home-class instance".
	if existing.ProviderID != "" {
		spec.NodeClass = "home"
		spec.ProviderID = existing.ProviderID
		// spec.NodeName is deliberately NOT threaded through here:
		// existing.NodeName is a separate, pre-existing gap (unrelated to
		// this fix) — compute.instances has no plain node_name column at
		// all (Create resolves it into a node_id FK via a sub-select), and
		// nothing reads it back out, so existing.NodeName is always "".
		// Harmless for THIS deployment (single home node — the scheduler
		// has no other choice regardless) but worth fixing properly before
		// a multi-node home provider exists, where a redeploy landing on a
		// different node than the original create could break node-local
		// storage affinity. createOrReplace's own dispatch only routes on
		// ProviderID, never NodeName, so this does not affect the fix
		// above.
	}

	result, err := s.cluster.UpdateInstance(ctx, scopeFor(projectID), spec)
	if err != nil {
		var errMsg string
		switch {
		case errors.Is(err, cluster.ErrNotFound):
			errMsg = "this session's previously deployed instance no longer exists in the cluster"
			c.JSON(http.StatusNotFound, gin.H{"error": errMsg})
		case errors.Is(err, cluster.ErrClusterUnavailable):
			errMsg = "no GPU capacity is reachable right now; the existing instance is unaffected"
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsg})
		default:
			errMsg = fmt.Sprintf("failed to redeploy instance: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsg})
		}
		// The PREVIOUS image is still running unaffected (UpdateInstance
		// failing never tears down what was already there) — this only
		// records that the LATEST attempt failed, matching what the
		// "Failed" status on the console's build list is actually meant
		// to mean: not "nothing is running", but "your last action here
		// didn't work".
		s.recordDeployOutcome(ctx, sessionID, errMsg)
		return
	}

	containerPort := 0
	if len(ports) > 0 {
		containerPort = ports[0].Container
	}
	if err := s.store.UpdateImage(ctx, existing.ID, imageRef, result.PodName, containerPort); err != nil {
		// The pod is real and running the new code; only OUR record of
		// which image/pod it is now on failed to update. Not worth
		// failing the customer's redeploy over — logged so an operator
		// can reconcile. UpdateImage itself now clears terminated_at as
		// part of this same call (see its own doc comment) precisely so
		// this really is just a record-keeping failure and not, as it
		// used to be, the reason the instance stays permanently
		// unreachable at the edge despite a genuinely live pod.
		log.Printf("WARN: redeployed instance %s for Kumbha session %s but failed to update its record: %v", existing.ID, sessionID, err)
	}

	if err := s.kumbha.CheckpointWorkspace(ctx, sessionID); err != nil {
		log.Printf("WARN: redeployed Kumbha session %s but failed to checkpoint its workspace version: %v", sessionID, err)
	}

	// The endpoint is normally UNCHANGED by a redeploy — same Service/
	// Ingress, same hostname — so existing.Endpoint (read from the
	// database before this redeploy even started) is the right source in
	// the common case, not `result` (which, being a pod-only replace,
	// never re-provisions anything). But existing.Endpoint can genuinely
	// be empty here — exactly the state inst-5ed29952's row was in for
	// this whole incident — and in THAT case `result.EndpointUrl` is not
	// nothing: createOrReplace (agent.go) synthesizes it fresh, in this
	// SAME call, the instant spec.Ports is non-empty (which it now always
	// is, given the port-resolution fixes above). Found live 2026-08-31:
	// even after container_port and terminated_at were both fixed and the
	// instance was genuinely reachable again, this response — and, via
	// the exact same stale value, triggerScreenshotCapture's targetURL —
	// still reported an empty endpoint, because existing.Endpoint had been
	// read into a local variable BEFORE this redeploy's own cluster call
	// had a chance to fix it, and nothing here ever looked at what that
	// call actually returned. The reconciler will eventually converge
	// compute.instances.endpoint to the same value on its next pass (up to
	// a minute later) regardless — this just stops the SAME request that
	// fixed it from reporting the stale answer back, and stops the
	// screenshot from missing its own trigger for another full redeploy
	// cycle.
	redeployedEndpoint := existing.Endpoint
	if redeployedEndpoint == "" {
		redeployedEndpoint = result.EndpointURL
	}

	s.triggerGithubPush(sessionID, accountID, imageRef)
	s.triggerScreenshotCapture(sessionID, sess, redeployedEndpoint)
	s.recordDeployOutcome(ctx, sessionID, "")

	c.JSON(http.StatusOK, gin.H{
		"image_ref":      imageRef,
		"instance_id":    existing.ID,
		"endpoint":       redeployedEndpoint,
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
func (s *Server) invokeInternally(handler gin.HandlerFunc, method, path string, params gin.Params, accountID, projectID, userID, kumbhaSessionID uuid.UUID, body any) (status int, respBody []byte) {
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
	// Set so CreateInstance's own auth.GetSessionID(c) tags the resulting
	// instance with this session — same mechanism a real Kumbha session
	// token uses on the create_instance MCP path, so every instance this
	// handler provisions is tracked identically regardless of which route
	// created it (see migration 032's own doc comment).
	if kumbhaSessionID != uuid.Nil {
		c.Set(string(auth.SessionIDKey), kumbhaSessionID)
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
	s.enrichKumbhaAgentRunning(c.Request.Context(), resp, sess)
	s.enrichKumbhaAppStatus(c.Request.Context(), resp, sess, projectID)

	c.JSON(http.StatusOK, resp)
}

// enrichKumbhaAgentRunning replaces the cheap "was one ever launched and
// never torn down" agent_running proxy with a live cluster read — cheap
// enough (an in-memory cache lookup in agent-mode topology, the actual
// production one) to run on every session in ListKumbhaSessions too, not
// only the single-session GetKumbhaSession read; both call this. Found
// live 2026-08-29: the cheap proxy alone left the "Previous builds" list
// showing an animated, ongoing-looking "Building" status for a session
// whose agent had long since finished.
func (s *Server) enrichKumbhaAgentRunning(ctx context.Context, resp gin.H, sess *kumbha.Session) {
	if sess.AgentInstanceID == "" {
		return
	}
	if running, err := s.kumbha.IsAgentRunning(ctx, sess); err == nil {
		resp["agent_running"] = running
	}
}

// enrichKumbhaAppStatus adds the deployed app's own live status —
// "pending"/"running"/"failed"/"terminated", cluster's own vocabulary
// (see compute.Status* and statusToInstance), plus its endpoint — to an
// already-built kumbhaSessionResponse. This is what actually answers "did
// the last deploy work", which agent_running alone cannot: the agent can
// finish (or never have been running this whole time, on a page reload)
// while the app it deployed is crash-looping. Empty when no deploy has
// happened yet, or the read failed — never guessed.
//
// Deliberately called ONLY from GetKumbhaSession, not
// ListKumbhaSessions: a per-row live cluster read for every session in a
// list, on every poll, is a cost that list deliberately does not pay
// (see build/page.tsx's own SessionStatusPill comment) — the list shows
// "Deployed" (has shipped at least once) rather than this endpoint's
// finer running/failed distinction, which stays specific to the detail
// page a customer has actually opened.
func (s *Server) enrichKumbhaAppStatus(ctx context.Context, resp gin.H, sess *kumbha.Session, projectID uuid.UUID) {
	if sess.AppInstanceID != "" && s.cluster != nil {
		if st, err := s.cluster.GetInstanceStatus(ctx, scopeFor(projectID), sess.AppInstanceID); err == nil {
			resp["app_status"] = st.Status
			resp["app_status_message"] = st.Message
			// The endpoint comes from the STORED record (same resolution
			// GetInstance uses), not st.EndpointURL directly — that field is
			// only ever populated by DirectClient; AgentClient's cached
			// status (the actual topology this platform runs on, home-node
			// placement) never carries it, so trusting it here left the
			// Preview tab empty for a real, running, reachable app. See
			// resolveEndpoint's own doc comment (found live 2026-08-29).
			var record *compute.InstanceRecord
			if s.store != nil {
				record, _ = s.store.Get(ctx, sess.AppInstanceID)
			}
			endpoint, _, _, _ := resolveEndpoint(record, s.endpointDomain)
			resp["app_endpoint"] = endpoint
		}
	}
}

// kumbhaSessionInstance is one row of ListKumbhaSessionInstances' response
// — deliberately built from the stored record alone (no live cluster
// read): unlike the single "app" instance enrichKumbhaAppStatus enriches,
// a session can have several of these (found live 2026-08-30/31: a broken
// deploy workaround produced two), so a per-row live status call here
// would repeat the exact per-row cluster-read cost ListKumbhaSessions'
// own doc comment already explains avoiding. The stored status is what
// the reconciler last observed, same staleness bound as the plain Compute
// list between reconciler ticks.
type kumbhaSessionInstance struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Image        string     `json:"image"`
	Status       string     `json:"status"`
	Endpoint     string     `json:"endpoint"`
	IsApp        bool       `json:"is_app"`
	CreatedAt    time.Time  `json:"created_at"`
	TerminatedAt *time.Time `json:"terminated_at,omitempty"`
}

// ListKumbhaSessionInstances lists every compute instance this session has
// ever created, regardless of which tool created it (deploy's own
// bookkeeping vs. a raw create_instance call) — the console surfaces this
// so nothing a Kumbha agent provisions can go unseen and unbilled-for
// awareness again (see migration 032's own doc comment for the live
// incident this closes). Deletion reuses the existing DeleteInstance
// endpoint; this is read-only.
// GET /v1/kumbha/sessions/:id/instances
func (s *Server) ListKumbhaSessionInstances(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return
	}
	if s.store == nil {
		c.JSON(http.StatusOK, gin.H{"instances": []kumbhaSessionInstance{}})
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

	records, err := s.store.ListByKumbhaSession(c.Request.Context(), sessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	out := make([]kumbhaSessionInstance, 0, len(records))
	for _, r := range records {
		out = append(out, kumbhaSessionInstance{
			ID:           r.ID,
			Name:         r.Name,
			Image:        r.Image,
			Status:       r.Status,
			Endpoint:     r.Endpoint,
			IsApp:        r.ID == sess.AppInstanceID,
			CreatedAt:    r.CreatedAt,
			TerminatedAt: r.TerminatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"instances": out})
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
// previous pod already exited, whether from its own idle timeout or an
// explicit Stop) — see its own doc comment for why the latter is an
// honest, bounded degradation rather than real persistence. Deliberately
// NOT gated on session.Status any more: there is no longer a customer
// action that puts a session into a state where sending a message should
// be refused (see StopKumbhaAgent's own doc comment on why Close was
// removed) — a session that genuinely no longer exists 404s from
// GetSession below either way.
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
