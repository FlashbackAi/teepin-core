// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
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

// agentRequest builds a request carrying a session-scoped credential (the
// agent's own auth shape — SessionIDKey set, no ProjectIDKey) rather than
// kumbhaRequest's ordinary customer JWT shape, since UploadKumbhaWorkspace
// is the one endpoint in this file the agent itself calls.
func agentRequest(handler gin.HandlerFunc, method, path string, params gin.Params, sessionID uuid.UUID, body any) *httptest.ResponseRecorder {
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
	if sessionID != uuid.Nil {
		c.Set(string(auth.SessionIDKey), sessionID)
	}
	handler(c)
	return w
}

// sessionRow returns the mock row shape GetSession's SELECT expects,
// matching the columns kumbha_handlers_test.go's own success tests use.
func sessionRow(sessionID uuid.UUID) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_id", "project_id", "budget", "spent", "status", "label",
		"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
		"last_deploy_failed", "last_deploy_error", "last_deploy_at",
	}).AddRow(sessionID, testAccountID, uuid.New(), 5.0, 0.0, "open", nil, nil, nil, false, nowStub(), nil, false, nil, nil)
}

func TestUploadKumbhaWorkspace_RequiresSessionCredential(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID := uuid.New()
	// No agent credential at all — an ordinary, unauthenticated-for-this-
	// purpose request must not be able to write a workspace.
	w := kumbhaRequest(server.UploadKumbhaWorkspace, http.MethodPut, "/v1/kumbha/sessions/"+sessionID.String()+"/workspace",
		gin.Params{{Key: "id", Value: sessionID.String()}}, uuid.Nil, workspaceSaveRequest{}, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
}

// The tenancy-critical case: an agent's session-scoped credential must
// only ever be able to write ITS OWN session's workspace — the path
// parameter alone must not decide whose row gets overwritten.
func TestUploadKumbhaWorkspace_RejectsMismatchedSession(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	credentialSession := uuid.New()
	pathSession := uuid.New() // a DIFFERENT session than the credential names

	w := agentRequest(server.UploadKumbhaWorkspace, http.MethodPut, "/v1/kumbha/sessions/"+pathSession.String()+"/workspace",
		gin.Params{{Key: "id", Value: pathSession.String()}}, credentialSession, workspaceSaveRequest{})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a session-id mismatch (body: %s)", w.Code, w.Body.String())
	}
}

func TestUploadKumbhaWorkspace_Success(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT v\.version, v\.is_checkpoint, v\.file_count`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "is_checkpoint", "file_count"}).AddRow(nil, nil, nil))
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM billing\.kumbha_workspace_versions`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO billing\.kumbha_workspace_versions`).
		WithArgs(sessionID, 1, sqlmock.AnyArg(), sqlmock.AnyArg(), 1, int64(len("<h1>hi</h1>")), string(kumbha.CreatedByAgent), false).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET current_workspace_version`).
		WithArgs(1, sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	req := workspaceSaveRequest{Files: []kumbha.WorkspaceFile{{Path: "index.html", Content: "<h1>hi</h1>"}}}
	w := agentRequest(server.UploadKumbhaWorkspace, http.MethodPut, "/v1/kumbha/sessions/"+sessionID.String()+"/workspace",
		gin.Params{{Key: "id", Value: sessionID.String()}}, sessionID, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Version != 1 {
		t.Errorf("got version %d, want 1", resp.Version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestUploadKumbhaWorkspace_InvalidPayloadIs400(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID := uuid.New()
	req := workspaceSaveRequest{Files: []kumbha.WorkspaceFile{{Path: "../../etc/passwd", Content: "x"}}}
	w := agentRequest(server.UploadKumbhaWorkspace, http.MethodPut, "/v1/kumbha/sessions/"+sessionID.String()+"/workspace",
		gin.Params{{Key: "id", Value: sessionID.String()}}, sessionID, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a path-escaping payload (body: %s)", w.Code, w.Body.String())
	}
}

// SaveKumbhaWorkspace is the console IDE's Save button — a customer JWT,
// not a session credential, and it must confirm the session belongs to
// the caller's own account before writing a version.
func TestSaveKumbhaWorkspace_Success(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sessionRow(sessionID))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM billing\.kumbha_workspace_versions`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(1))
	mock.ExpectExec(`INSERT INTO billing\.kumbha_workspace_versions`).
		WithArgs(sessionID, 2, sqlmock.AnyArg(), sqlmock.AnyArg(), 1, int64(len("<h1>edited</h1>")), string(kumbha.CreatedByCustomer), true).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET current_workspace_version`).
		WithArgs(2, sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	req := workspaceSaveRequest{Files: []kumbha.WorkspaceFile{{Path: "index.html", Content: "<h1>edited</h1>"}}}
	w := kumbhaRequest(server.SaveKumbhaWorkspace, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/workspace",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, req, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Version != 2 {
		t.Errorf("got version %d, want 2", resp.Version)
	}
}

func TestSaveKumbhaWorkspace_UnknownSessionIs404(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnError(sqlNoRows())

	req := workspaceSaveRequest{Files: []kumbha.WorkspaceFile{{Path: "index.html", Content: "hi"}}}
	w := kumbhaRequest(server.SaveKumbhaWorkspace, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/workspace",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, req, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for another account's session (body: %s)", w.Code, w.Body.String())
	}
}

func TestGetKumbhaWorkspace_NoWorkspaceIs404(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT current_workspace_version FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnError(sqlNoRows())

	w := kumbhaRequest(server.GetKumbhaWorkspace, http.MethodGet, "/v1/kumbha/sessions/"+sessionID.String()+"/workspace",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

func TestGetKumbhaWorkspace_Success(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT current_workspace_version FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{"current_workspace_version"}).AddRow(1))
	mock.ExpectQuery(`FROM billing\.kumbha_workspace_versions v`).
		WithArgs(sessionID, testAccountID, 1).
		WillReturnRows(sqlmock.NewRows([]string{"version", "files", "skipped", "file_count", "byte_size", "created_by", "created_at", "is_checkpoint", "is_deployed"}).
			AddRow(1, `[{"path":"index.html","content":"<h1>hi</h1>"}]`, `[]`, 1, 11, "agent", time.Now(), true, true))

	w := kumbhaRequest(server.GetKumbhaWorkspace, http.MethodGet, "/v1/kumbha/sessions/"+sessionID.String()+"/workspace",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Version      int                    `json:"version"`
		Files        []kumbha.WorkspaceFile `json:"files"`
		IsCheckpoint bool                   `json:"is_checkpoint"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Version != 1 || len(resp.Files) != 1 || resp.Files[0].Path != "index.html" {
		t.Errorf("got %+v, want version 1 with one file index.html", resp)
	}
	if !resp.IsCheckpoint {
		t.Error("is_checkpoint did not reach the response — the console's Deploy button needs it to detect \"nothing changed since last deploy\"")
	}
}

func TestListKumbhaWorkspaceVersions_Success(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`FROM billing\.kumbha_workspace_versions v`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "file_count", "byte_size", "created_by", "created_at", "is_current", "is_deployed"}).
			AddRow(2, 1, 20, "customer", time.Now(), true, false).
			AddRow(1, 1, 11, "agent", time.Now(), false, true))

	w := kumbhaRequest(server.ListKumbhaWorkspaceVersions, http.MethodGet, "/v1/kumbha/sessions/"+sessionID.String()+"/workspace/versions",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Versions []kumbha.VersionInfo `json:"versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Versions) != 2 || resp.Versions[0].Version != 2 || !resp.Versions[0].Current {
		t.Errorf("got %+v, want version 2 first and marked current", resp.Versions)
	}
}

func TestRollbackKumbhaWorkspace_Success(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectExec(`UPDATE billing\.inference_sessions`).
		WithArgs(1, sessionID, testAccountID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := kumbhaRequest(server.RollbackKumbhaWorkspace, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/workspace/rollback",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]int{"version": 1}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}

func TestRollbackKumbhaWorkspace_UnknownVersionIs404(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectExec(`UPDATE billing\.inference_sessions`).
		WithArgs(99, sessionID, testAccountID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := kumbhaRequest(server.RollbackKumbhaWorkspace, http.MethodPost, "/v1/kumbha/sessions/"+sessionID.String()+"/workspace/rollback",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, map[string]int{"version": 99}, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

func TestDownloadKumbhaWorkspace_StreamsAValidZip(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.NewRouter(nil), allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	sessionID, projectID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT current_workspace_version FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{"current_workspace_version"}).AddRow(1))
	mock.ExpectQuery(`FROM billing\.kumbha_workspace_versions v`).
		WithArgs(sessionID, testAccountID, 1).
		WillReturnRows(sqlmock.NewRows([]string{"version", "files", "skipped", "file_count", "byte_size", "created_by", "created_at", "is_checkpoint", "is_deployed"}).
			AddRow(1, `[{"path":"index.html","content":"<h1>hi</h1>"}]`, `[]`, 1, 11, "agent", time.Now(), true, true))

	w := kumbhaRequest(server.DownloadKumbhaWorkspace, http.MethodGet, "/v1/kumbha/sessions/"+sessionID.String()+"/workspace/archive",
		gin.Params{{Key: "id", Value: sessionID.String()}}, projectID, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", got)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("response body is not a valid zip: %v", err)
	}
	if len(zr.File) != 1 || zr.File[0].Name != "index.html" {
		t.Errorf("got %d entries, want one named index.html", len(zr.File))
	}
}
