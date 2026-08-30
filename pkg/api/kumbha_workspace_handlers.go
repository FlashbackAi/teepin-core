// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
)

// Workspace endpoints — how the source Kumbha writes reaches the customer,
// and how the customer's own editable-IDE saves reach back.
//
// Versioned, not overwrite-in-place: every save (agent OR customer)
// creates a new version and moves the "current" pointer to it, so a
// customer edit or an agent step that breaks a working build is always
// something to roll back FROM, never lost data. See migration 025 for the
// storage shape and the console's build/workspace-panel for how history +
// rollback are presented.
//
// Two write paths with deliberately different credentials and different
// created_by values, which is why the auth here is not uniform:
//
//   - The AGENT uploads (PUT), holding a session-scoped token. It may only
//     write to its OWN session: the session_id claim must match the path,
//     so a compromised or prompt-injected agent cannot overwrite another
//     build's source even though the endpoint shape would otherwise allow
//     it. Recorded as created_by=agent.
//   - The CUSTOMER saves (POST), holding an ordinary JWT, scoped by
//     account the same way every other customer-facing write is —
//     the console IDE's Save button. Recorded as created_by=customer.
//
// Reads (GET) and rollback are customer-only, same JWT/account scoping.
//
// Deliberately NOT a git remote: see migration 025's own note — a
// repository URL in a shared org exposes that org's other repositories to
// anyone who follows the link, and a download/version-history UI has no
// such surface.

// workspaceSaveRequest is the shared body shape for both write paths
// (agent PUT and customer POST) — field names match run.py's payload
// builder and the console's Save call.
type workspaceSaveRequest struct {
	Files   []kumbha.WorkspaceFile `json:"files"`
	Skipped []kumbha.SkippedFile   `json:"skipped"`
}

// UploadKumbhaWorkspace saves the agent's current workspace as a new
// version.
// PUT /v1/kumbha/sessions/:id/workspace
func (s *Server) UploadKumbhaWorkspace(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return
	}

	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}

	// The agent's own credential names exactly one session. Requiring it to
	// match the path is what makes this endpoint safe to expose to the
	// least-trusted component in the system: there is no argument the agent
	// can pass that reaches another session's row.
	callerSession, ok := auth.GetSessionID(c)
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "this endpoint requires a Kumbha session credential"})
		return
	}
	if callerSession != sessionID {
		c.JSON(http.StatusForbidden, gin.H{"error": "this credential does not belong to that session"})
		return
	}

	var req workspaceSaveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	version, err := s.kumbha.SaveWorkspaceVersion(c.Request.Context(), sessionID, req.Files, req.Skipped, kumbha.CreatedByAgent)
	if err != nil {
		writeWorkspaceSaveError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"version": version, "files": len(req.Files), "skipped": len(req.Skipped)})
}

// SaveKumbhaWorkspace saves a customer's own edit (from the console's
// editable IDE) as a new version.
// POST /v1/kumbha/sessions/:id/workspace
func (s *Server) SaveKumbhaWorkspace(c *gin.Context) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return
	}

	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}

	// requireScope confirms the caller holds a valid account/project JWT;
	// ownership of THIS session is confirmed below by loading it scoped to
	// that account before trusting the write.
	_, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}
	if _, err := s.kumbha.GetSession(c.Request.Context(), sessionID, accountID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	var req workspaceSaveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	version, err := s.kumbha.SaveWorkspaceVersion(c.Request.Context(), sessionID, req.Files, req.Skipped, kumbha.CreatedByCustomer)
	if err != nil {
		writeWorkspaceSaveError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"version": version, "files": len(req.Files), "skipped": len(req.Skipped)})
}

func writeWorkspaceSaveError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, kumbha.ErrInvalidSnapshot):
		// The payload was wrong, not us — and both callers (the agent and
		// the console) can correct themselves, so the specific reason is
		// worth returning rather than flattening to "bad request".
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, kumbha.ErrTooManyVersions):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// GetKumbhaWorkspace returns the file tree and contents for the current
// version, or a specific one via ?version=N — what the console's code
// viewer renders.
// GET /v1/kumbha/sessions/:id/workspace
func (s *Server) GetKumbhaWorkspace(c *gin.Context) {
	snap, _, ok := s.loadCustomerWorkspace(c)
	if !ok {
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"version":       snap.Version,
		"files":         snap.Files,
		"skipped":       snap.Skipped,
		"file_count":    snap.FileCount,
		"byte_size":     snap.ByteSize,
		"created_by":    snap.CreatedBy,
		"created_at":    snap.CreatedAt,
		"is_checkpoint": snap.IsCheckpoint,
		"is_deployed":   snap.IsDeployed,
	})
}

// ListKumbhaWorkspaceVersions returns every version's metadata (no file
// content), newest first — the console's version history list.
// GET /v1/kumbha/sessions/:id/workspace/versions
func (s *Server) ListKumbhaWorkspaceVersions(c *gin.Context) {
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

	versions, err := s.kumbha.WorkspaceHistory(c.Request.Context(), sessionID, accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"versions": versions})
}

// rollbackWorkspaceRequest is the body of a rollback call.
type rollbackWorkspaceRequest struct {
	Version int `json:"version" binding:"required"`
}

// RollbackKumbhaWorkspace moves the current-version pointer to an
// existing version — the undo for a customer edit or an agent step that
// broke something. Never deletes anything: the version rolled back FROM
// stays in history, so a rollback can itself be undone.
// POST /v1/kumbha/sessions/:id/workspace/rollback
func (s *Server) RollbackKumbhaWorkspace(c *gin.Context) {
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

	var req rollbackWorkspaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	if err := s.kumbha.RollbackWorkspace(c.Request.Context(), sessionID, accountID, req.Version); err != nil {
		if errors.Is(err, kumbha.ErrVersionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "that version does not exist for this session"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"current_version": req.Version})
}

// DownloadKumbhaWorkspace streams the current (or a specific ?version=N)
// version as a ZIP.
// GET /v1/kumbha/sessions/:id/workspace/archive
func (s *Server) DownloadKumbhaWorkspace(c *gin.Context) {
	snap, sessionID, ok := s.loadCustomerWorkspace(c)
	if !ok {
		return
	}

	// Short session prefix + version, matching how the rest of the product
	// names things a human has to recognise at a glance.
	filename := fmt.Sprintf("kumbha-%s-v%d.zip", sessionID.String()[:8], snap.Version)
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Header("Content-Type", "application/zip")
	// The archive is generated as it streams, so its length is not known up
	// front; saying so explicitly stops any middleware buffering the whole
	// thing to compute one.
	c.Header("X-Content-Type-Options", "nosniff")

	if err := snap.WriteZip(c.Writer); err != nil {
		// Headers are already sent by this point, so there is no clean way
		// to turn this into a JSON error — abort the response so the client
		// sees a truncated transfer rather than a silently corrupt archive.
		_ = c.Error(err)
		c.Abort()
		return
	}
}

// loadCustomerWorkspace resolves the shared preamble of the customer-facing
// reads (get + download): gateway configured, valid session id, caller's
// own account, and either the current version or an explicit ?version=N.
func (s *Server) loadCustomerWorkspace(c *gin.Context) (*kumbha.Snapshot, uuid.UUID, bool) {
	if s.kumbha == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "the Kumbha Gateway is not available on this deployment"})
		return nil, uuid.Nil, false
	}

	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return nil, uuid.Nil, false
	}

	_, accountID, ok := s.requireScope(c)
	if !ok {
		return nil, uuid.Nil, false
	}

	var snap *kumbha.Snapshot
	if raw := c.Query("version"); raw != "" {
		version, err := strconv.Atoi(raw)
		if err != nil || version < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid version"})
			return nil, uuid.Nil, false
		}
		snap, err = s.kumbha.WorkspaceVersion(c.Request.Context(), sessionID, accountID, version)
	} else {
		snap, err = s.kumbha.CurrentWorkspace(c.Request.Context(), sessionID, accountID)
	}
	if err != nil {
		if errors.Is(err, kumbha.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return nil, uuid.Nil, false
		}
		if errors.Is(err, kumbha.ErrNoWorkspace) || errors.Is(err, kumbha.ErrVersionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": "no files have been saved for this build yet",
			})
			return nil, uuid.Nil, false
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return nil, uuid.Nil, false
	}
	return snap, sessionID, true
}
