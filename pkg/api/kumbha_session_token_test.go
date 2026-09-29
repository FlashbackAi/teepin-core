// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
)

// The agent's session token is readable from the agent's own terminal, so
// every check that stops it spending money has to hold on the server, not
// in the MCP tool. These tests cover the handler-level halves of that; the
// route allowlist half is in pkg/auth/session_routes_test.go.

// approvedSessionRow is sessionRow with deploy_approved = true.
func approvedSessionRow(sessionID uuid.UUID) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_id", "project_id", "budget", "spent", "status", "label",
		"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
		"last_deploy_failed", "last_deploy_error", "last_deploy_at", "model_alias",
	}).AddRow(sessionID, testAccountID, uuid.New(), 5.0, 0.0, "open", nil, nil, nil, true, nowStub(), nil, false, nil, nil, "teepin/fast")
}

// createInstanceAsSession calls CreateInstance carrying a session credential.
func createInstanceAsSession(server *Server, sessionID uuid.UUID) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"name":"t","image":"nginx","cpu_units":1,"memory":"1GB"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(auth.ProjectIDKey), uuid.New())
	c.Set(string(auth.AccountIDKey), testAccountID)
	c.Set(string(auth.SessionIDKey), sessionID)
	server.CreateInstance(c)
	return w
}

// Before the customer approves the plan, a session credential cannot create
// an instance even by calling the API directly and skipping the MCP tool.
func TestCreateInstance_SessionTokenNeedsDeployApproval(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, nil, allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	fc := newFakeCluster()
	server := (&Server{store: cStore, cluster: fc}).WithKumbha(gw)

	sessionID := uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(sessionRow(sessionID)) // deploy_approved = false

	w := createInstanceAsSession(server, sessionID)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not_approved") {
		t.Errorf("body missing machine-readable code: %s", w.Body.String())
	}
	if fc.lastSpec.InstanceID != "" {
		t.Error("the cluster must never be reached for an unapproved session")
	}
}

// An unreadable session must deny, not allow: the approval check is the
// gate on real spend, so a database error cannot open it.
func TestCreateInstance_SessionTokenFailsClosedWhenSessionUnreadable(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, nil, allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	fc := newFakeCluster()
	server := (&Server{store: cStore, cluster: fc}).WithKumbha(gw)

	sessionID := uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnError(errors.New("db unreachable"))

	w := createInstanceAsSession(server, sessionID)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", w.Code, w.Body.String())
	}
	if fc.lastSpec.InstanceID != "" {
		t.Error("the cluster must never be reached when approval could not be verified")
	}
}

// A human or API-key caller has no session claim and is not subject to the
// session approval gate: nothing is looked up for them.
func TestCreateInstance_NonSessionCallerSkipsApprovalLookup(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, nil, allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore, cluster: newFakeCluster()}).WithKumbha(gw)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"t","image":"nginx","cpu_units":1,"memory":"1GB"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(auth.ProjectIDKey), uuid.New())
	c.Set(string(auth.AccountIDKey), testAccountID)
	// The insert is expected; a session SELECT would not be, so sqlmock
	// (ordered) fails the test if one happens.
	mock.ExpectQuery(`INSERT INTO compute\.instances`).WillReturnError(errors.New("stop here"))
	server.CreateInstance(c)

	if strings.Contains(w.Body.String(), "not_approved") || w.Code == http.StatusServiceUnavailable && strings.Contains(w.Body.String(), "approval") {
		t.Errorf("a non-session caller must not hit the approval gate: %d %s", w.Code, w.Body.String())
	}
}

// One agent must not be able to bill another session's budget by naming it
// in the X-Teepin-Session header while holding its own token.
func TestKumbhaChatCompletions_TokenForAnotherSessionIs403(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, kumbha.StaticModels{{Route: "teepin/fast"}}, allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)

	tokenSession, headerSession := uuid.New(), uuid.New()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/kumbha/chat/completions",
		strings.NewReader(`{"model":"teepin/fast","messages":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Teepin-Session", headerSession.String())
	c.Set(string(auth.ProjectIDKey), uuid.New())
	c.Set(string(auth.AccountIDKey), testAccountID)
	c.Set(string(auth.SessionIDKey), tokenSession)
	server.KumbhaChatCompletions(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
}
