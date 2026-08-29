// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
)

// screenshotRow builds a SELECT screenshot, screenshot_captured_at result
// — png == nil models "no capture has succeeded yet" (both columns NULL).
func screenshotRow(png []byte) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"screenshot", "screenshot_captured_at"})
	if png == nil {
		return rows.AddRow(nil, nil)
	}
	return rows.AddRow(png, time.Now())
}

// rawSessionRequest builds a request carrying a session-scoped credential
// (the capture pod's own auth shape, same as agentRequest in
// kumbha_workspace_handlers_test.go) with a raw binary body instead of a
// JSON one — UploadKumbhaScreenshot reads the request body directly as
// PNG bytes, not a JSON envelope.
func rawSessionRequest(handler gin.HandlerFunc, method, path string, params gin.Params, sessionID uuid.UUID, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "image/png")
	c.Params = params
	if sessionID != uuid.Nil {
		c.Set(string(auth.SessionIDKey), sessionID)
	}
	handler(c)
	return w
}

func TestUploadKumbhaScreenshot_RequiresSessionCredential(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID := uuid.New()
	w := rawSessionRequest(server.UploadKumbhaScreenshot, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/screenshot",
		gin.Params{{Key: "id", Value: sessionID.String()}}, uuid.Nil, []byte("fake-png-bytes"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
}

// The tenancy-critical case, same reasoning as
// TestUploadKumbhaWorkspace_RejectsMismatchedSession: a capture pod's
// credential must only ever be able to write ITS OWN session's
// screenshot — the path parameter alone must not decide whose row gets
// overwritten.
func TestUploadKumbhaScreenshot_RejectsMismatchedSession(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	credentialSession := uuid.New()
	pathSession := uuid.New()

	w := rawSessionRequest(server.UploadKumbhaScreenshot, http.MethodPost, "/v1/kumbha/sessions/"+pathSession.String()+"/screenshot",
		gin.Params{{Key: "id", Value: pathSession.String()}}, credentialSession, []byte("fake-png-bytes"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
}

func TestUploadKumbhaScreenshot_RejectsEmptyBody(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID := uuid.New()
	w := rawSessionRequest(server.UploadKumbhaScreenshot, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/screenshot",
		gin.Params{{Key: "id", Value: sessionID.String()}}, sessionID, []byte{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
}

func TestUploadKumbhaScreenshot_StoresBytes(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID := uuid.New()
	png := []byte("fake-png-bytes")

	mock.ExpectExec(`UPDATE billing\.inference_sessions\s+SET screenshot`).
		WithArgs(sessionID, png).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := rawSessionRequest(server.UploadKumbhaScreenshot, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/screenshot",
		gin.Params{{Key: "id", Value: sessionID.String()}}, sessionID, png)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestGetKumbhaScreenshot_ReturnsStoredPNG(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	png := []byte("fake-png-bytes")

	mock.ExpectQuery(`SELECT screenshot, screenshot_captured_at`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(screenshotRow(png))

	w := kumbhaRequest(server.GetKumbhaScreenshot, http.MethodGet, "/v1/kumbha/sessions/"+sessionID.String()+"/screenshot",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if w.Body.String() != string(png) {
		t.Errorf("body = %q, want %q", w.Body.String(), string(png))
	}
}

// TestGetKumbhaScreenshot_NoCaptureYetIs404 covers a session that has
// deployed but whose capture pod hasn't finished (or Kumbha screenshots
// were never configured) — must be a 404, never a placeholder image, so
// the console can tell "capture pending" apart from a genuine thumbnail.
func TestGetKumbhaScreenshot_NoCaptureYetIs404(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT screenshot, screenshot_captured_at`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(screenshotRow(nil))

	w := kumbhaRequest(server.GetKumbhaScreenshot, http.MethodGet, "/v1/kumbha/sessions/"+sessionID.String()+"/screenshot",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}
