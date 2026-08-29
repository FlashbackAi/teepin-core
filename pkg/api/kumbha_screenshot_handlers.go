// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
)

// Screenshot endpoints — what backs the console's Preview tab thumbnail
// (teepin-console's deployment-extras.tsx). One capture per successful
// deploy, overwritten on redeploy (see migration 029) — no history kept.
//
// Two credentials, the same split UploadKumbhaWorkspace/
// DownloadKumbhaWorkspace already use:
//   - The CAPTURE POD uploads (POST), holding a session-scoped token
//     minted by Gateway.CaptureScreenshot. It may only write to its OWN
//     session — the session_id claim must match the path, the same
//     restriction the workspace upload endpoint already enforces for the
//     agent's own credential.
//   - The CUSTOMER reads (GET), holding an ordinary JWT, scoped by
//     account like every other customer-facing read.

// maxScreenshotBytes bounds the upload body — a PNG of one browser
// viewport at typical desktop resolution is a few hundred KB; 5MB is
// generous headroom without letting a misbehaving or compromised capture
// pod push an unbounded body into a database row.
const maxScreenshotBytes = 5 << 20

// UploadKumbhaScreenshot stores the capture pod's PNG as this session's
// current screenshot, overwriting whatever was captured before.
// POST /v1/kumbha/sessions/:id/screenshot
func (s *Server) UploadKumbhaScreenshot(c *gin.Context) {
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

	png, err := io.ReadAll(io.LimitReader(c.Request.Body, maxScreenshotBytes+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}
	if len(png) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "empty screenshot body"})
		return
	}
	if len(png) > maxScreenshotBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "screenshot exceeds the size limit"})
		return
	}

	if err := s.kumbha.SaveScreenshot(c.Request.Context(), sessionID, png); err != nil {
		if errors.Is(err, kumbha.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"stored": true})
}

// GetKumbhaScreenshot serves a session's most recently captured
// screenshot as a PNG. 404 when none has been captured yet — never a
// placeholder image — so the console can tell "not deployed yet"/
// "capture pending" apart from a genuine thumbnail.
// GET /v1/kumbha/sessions/:id/screenshot
func (s *Server) GetKumbhaScreenshot(c *gin.Context) {
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

	png, capturedAt, err := s.kumbha.Screenshot(c.Request.Context(), sessionID, accountID)
	if err != nil {
		if errors.Is(err, kumbha.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(png) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no screenshot has been captured for this session yet"})
		return
	}

	// Always re-validate rather than letting a browser cache the image
	// indefinitely under this same URL — a redeploy overwrites the same
	// row, so a stale cached thumbnail would silently outlive the
	// deployment it was captured from.
	c.Header("Cache-Control", "no-cache")
	c.Header("Last-Modified", capturedAt.UTC().Format(http.TimeFormat))
	c.Data(http.StatusOK, "image/png", png)
}
