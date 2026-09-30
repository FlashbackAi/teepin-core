// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Every route the agent, the screenshot pod and the Kaniko init container
// legitimately call must stay reachable; this pins the list so removing one
// by accident fails here rather than as a broken build in production.
func TestSessionMayCall_AllowsTheAgentsOwnRoutes(t *testing.T) {
	allowed := []struct{ method, pattern string }{
		{"POST", "/v1/kumbha/chat/completions"},
		{"GET", "/v1/kumbha/sessions/:id"},
		{"GET", "/v1/kumbha/sessions/:id/messages/poll"},
		{"PUT", "/v1/kumbha/sessions/:id/workspace"},
		{"GET", "/v1/kumbha/sessions/:id/workspace/archive"},
		{"POST", "/v1/kumbha/sessions/:id/screenshot"},
		{"POST", "/v1/kumbha/sessions/:id/deploy"},
		{"GET", "/v1/billing/pricing"},
		{"POST", "/v1/compute/instances"},
		{"GET", "/v1/compute/instances/:id"},
	}
	for _, r := range allowed {
		if !SessionMayCall(r.method, r.pattern) {
			t.Errorf("%s %s should be reachable with a session token", r.method, r.pattern)
		}
	}
}

// The routes a prompt-injected agent must never reach. approve-deploy and
// budget are the two that let it lift its own spending limits.
func TestSessionMayCall_DeniesEverythingElse(t *testing.T) {
	denied := []struct{ method, pattern string }{
		{"POST", "/v1/kumbha/sessions/:id/approve-deploy"},
		{"PATCH", "/v1/kumbha/sessions/:id/budget"},
		{"POST", "/v1/kumbha/sessions"},
		{"GET", "/v1/kumbha/sessions"},
		{"POST", "/v1/kumbha/sessions/bulk-delete"},
		{"POST", "/v1/kumbha/sessions/:id/stop"},
		{"POST", "/v1/kumbha/sessions/:id/build"},
		{"POST", "/v1/kumbha/sessions/:id/messages"},
		{"POST", "/v1/kumbha/sessions/:id/workspace"},
		{"POST", "/v1/kumbha/sessions/:id/workspace/rollback"},
		{"POST", "/v1/kumbha/attachments"},
		// Secrets must never pass through the agent.
		{"PUT", "/v1/kumbha/sessions/:id/secrets/:name"},
		{"GET", "/v1/kumbha/sessions/:id/secrets"},
		{"DELETE", "/v1/kumbha/sessions/:id/secrets/:name"},
		{"DELETE", "/v1/compute/instances/:id"},
		{"POST", "/v1/compute/instances/:id/exec"},
		{"GET", "/v1/compute/instances"},
		{"POST", "/v1/billing/credits/topups"},
		{"GET", "/v1/billing/credits"},
		{"POST", "/v1/billing/invoices"},
		{"POST", "/v1/chat/completions"},
		{"PUT", "/v1/storage/buckets/:bucket/object"},
		// Right path, wrong verb: the allowlist is per method.
		{"DELETE", "/v1/kumbha/sessions/:id"},
		{"POST", "/v1/kumbha/sessions/:id/messages/poll"},
		// A route gin did not match has an empty pattern.
		{"GET", ""},
	}
	for _, r := range denied {
		if SessionMayCall(r.method, r.pattern) {
			t.Errorf("%s %q must NOT be reachable with a session token", r.method, r.pattern)
		}
	}
}

// End to end through the real middleware on gin route patterns: the same
// session token gets through on an allowed route and a 403 on a denied one,
// and the denied handler never runs.
func TestRequireAuth_SessionTokenRouteAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := newMockService(t)
	m := NewMiddleware(svc, "test-secret").WithSessionChecker(fakeSessionChecker{open: true})

	var handlerRan bool
	ok := func(c *gin.Context) { handlerRan = true; c.Status(http.StatusOK) }

	r := gin.New()
	v1 := r.Group("/v1", m.RequireAuth())
	v1.POST("/kumbha/chat/completions", ok)
	v1.GET("/kumbha/sessions/:id", ok)
	v1.POST("/kumbha/sessions", ok)
	v1.POST("/kumbha/sessions/:id/approve-deploy", ok)
	v1.PATCH("/kumbha/sessions/:id/budget", ok)
	v1.POST("/compute/instances", ok)
	v1.DELETE("/compute/instances/:id", ok)

	sessionToken, err := MintSessionToken(uuid.New(), uuid.New(), uuid.New(), time.Hour, "test-secret")
	if err != nil {
		t.Fatalf("MintSessionToken: %v", err)
	}
	sid := uuid.NewString()

	cases := []struct {
		method, path string
		want         int
	}{
		{"POST", "/v1/kumbha/chat/completions", http.StatusOK},
		{"GET", "/v1/kumbha/sessions/" + sid, http.StatusOK},
		{"POST", "/v1/compute/instances", http.StatusOK},
		{"POST", "/v1/kumbha/sessions/" + sid + "/approve-deploy", http.StatusForbidden},
		{"PATCH", "/v1/kumbha/sessions/" + sid + "/budget", http.StatusForbidden},
		{"POST", "/v1/kumbha/sessions", http.StatusForbidden},
		{"DELETE", "/v1/compute/instances/some-instance", http.StatusForbidden},
	}
	for _, tc := range cases {
		handlerRan = false
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+sessionToken)
		r.ServeHTTP(w, req)

		if w.Code != tc.want {
			t.Errorf("%s %s = %d, want %d (body: %s)", tc.method, tc.path, w.Code, tc.want, w.Body.String())
		}
		if tc.want == http.StatusForbidden && handlerRan {
			t.Errorf("%s %s: the handler ran even though the token was refused", tc.method, tc.path)
		}
	}
}

// The allowlist is for session tokens only. A signed-in human must still
// reach approve-deploy, which is the whole point of the approval gate, and
// an API key must be untouched by it.
func TestRequireAuth_HumanCredentialsAreNotRestricted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := newMockService(t)
	m := NewMiddleware(svc, "test-secret").WithSessionChecker(fakeSessionChecker{open: true})

	r := gin.New()
	r.POST("/v1/kumbha/sessions/:id/approve-deploy", m.RequireAuth(), func(c *gin.Context) { c.Status(http.StatusOK) })

	user := &User{ID: uuid.New(), AccountID: uuid.New(), Email: "a@b.com", Role: RoleOwner}
	access, _, err := GenerateJWT(user, "acme", "test-secret")
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/kumbha/sessions/"+uuid.NewString()+"/approve-deploy", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("a signed-in user was refused on approve-deploy: status %d", w.Code)
	}
}

// OptionalAuth must not become a side door: a session token on a route it
// guards is refused too, instead of being silently treated as anonymous.
func TestOptionalAuth_SessionTokenRouteAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := newMockService(t)
	m := NewMiddleware(svc, "test-secret").WithSessionChecker(fakeSessionChecker{open: true})

	r := gin.New()
	r.POST("/v1/kumbha/sessions/:id/approve-deploy", m.OptionalAuth(), func(c *gin.Context) { c.Status(http.StatusOK) })

	token, err := MintSessionToken(uuid.New(), uuid.New(), uuid.New(), time.Hour, "test-secret")
	if err != nil {
		t.Fatalf("MintSessionToken: %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/kumbha/sessions/"+uuid.NewString()+"/approve-deploy", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}
