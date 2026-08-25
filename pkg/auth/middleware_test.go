// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func newTestContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/compute/instances", nil)
	return c
}

// A login JWT carries no project claim (see jwt.go) — it identifies a
// person, not a project. When the console sends the X-Project-ID header for
// a project the caller's account actually owns, the middleware resolves it
// onto the Principal, so a JWT-only request can reach the compute API
// without a project API key.
func TestAuthenticate_JWTWithOwnedProjectHeader(t *testing.T) {
	svc, mock := newMockService(t)
	m := NewMiddleware(svc, "test-secret")

	user := &User{ID: uuid.New(), AccountID: uuid.New(), Email: "a@b.com", Role: RoleOwner}
	access, _, err := GenerateJWT(user, "acme", "test-secret")
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	projectID := uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM auth\.projects\s+WHERE id = \$1 AND account_id = \$2`).
		WithArgs(projectID, user.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "owner_id", "name", "slug", "description", "environment", "created_at", "updated_at",
		}).AddRow(projectID, user.AccountID, user.ID, "production", "production", "", "", nowUTC(), nowUTC()))

	c := newTestContext(t)
	c.Request.Header.Set("Authorization", "Bearer "+access)
	c.Request.Header.Set(ProjectHeader, projectID.String())

	p := m.authenticate(c)
	if p == nil {
		t.Fatal("authenticate returned nil for a valid JWT")
	}
	if p.ProjectID != projectID {
		t.Errorf("ProjectID = %s, want %s (header project was not resolved)", p.ProjectID, projectID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The header names a project belonging to a DIFFERENT account. The
// ownership check must fail closed: ProjectID stays unset rather than
// trusting the header, or any signed-in user could read another account's
// instances by sending its project UUID.
func TestAuthenticate_JWTWithOtherAccountsProjectHeader(t *testing.T) {
	svc, mock := newMockService(t)
	m := NewMiddleware(svc, "test-secret")

	user := &User{ID: uuid.New(), AccountID: uuid.New(), Email: "a@b.com", Role: RoleOwner}
	access, _, err := GenerateJWT(user, "acme", "test-secret")
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	othersProject := uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM auth\.projects`).
		WithArgs(othersProject, user.AccountID).
		WillReturnError(sql.ErrNoRows)

	c := newTestContext(t)
	c.Request.Header.Set("Authorization", "Bearer "+access)
	c.Request.Header.Set(ProjectHeader, othersProject.String())

	p := m.authenticate(c)
	if p == nil {
		t.Fatal("authenticate returned nil — the JWT itself is valid, only the project should be rejected")
	}
	if p.ProjectID != uuid.Nil {
		t.Errorf("ProjectID = %s, want Nil — a project owned by another account must never be resolved", p.ProjectID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A malformed header (not a UUID) must not panic or otherwise abort
// authentication — it just leaves ProjectID unset, same as no header.
func TestAuthenticate_JWTWithMalformedProjectHeader(t *testing.T) {
	svc, _ := newMockService(t)
	m := NewMiddleware(svc, "test-secret")

	user := &User{ID: uuid.New(), AccountID: uuid.New(), Email: "a@b.com", Role: RoleOwner}
	access, _, err := GenerateJWT(user, "acme", "test-secret")
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	c := newTestContext(t)
	c.Request.Header.Set("Authorization", "Bearer "+access)
	c.Request.Header.Set(ProjectHeader, "not-a-uuid")

	p := m.authenticate(c)
	if p == nil {
		t.Fatal("authenticate returned nil for a valid JWT with only a malformed project header")
	}
	if p.ProjectID != uuid.Nil {
		t.Errorf("ProjectID = %s, want Nil for an unparseable header", p.ProjectID)
	}
}

// fakeSessionChecker is a minimal auth.SessionChecker for testing the
// session-scoped credential path without a real kumbha.Store.
type fakeSessionChecker struct {
	open bool
	err  error
}

func (f fakeSessionChecker) IsSessionOpen(_ context.Context, _, _ uuid.UUID) (bool, error) {
	return f.open, f.err
}

// A session-scoped credential (MintSessionToken) for an OPEN session must
// authenticate successfully and carry SessionID on the Principal — this is
// the agent's own credential path, distinct from every human/API-key path
// tested above.
func TestAuthenticate_SessionTokenForOpenSession(t *testing.T) {
	svc, _ := newMockService(t)
	m := NewMiddleware(svc, "test-secret").WithSessionChecker(fakeSessionChecker{open: true})

	accountID, projectID, sessionID := uuid.New(), uuid.New(), uuid.New()
	token, err := MintSessionToken(accountID, projectID, sessionID, time.Hour, "test-secret")
	if err != nil {
		t.Fatalf("MintSessionToken: %v", err)
	}

	c := newTestContext(t)
	c.Request.Header.Set("Authorization", "Bearer "+token)

	p := m.authenticate(c)
	if p == nil {
		t.Fatal("authenticate returned nil for a session token whose session is open")
	}
	if p.SessionID != sessionID {
		t.Errorf("SessionID = %s, want %s", p.SessionID, sessionID)
	}
	if p.ProjectID != projectID {
		t.Errorf("ProjectID = %s, want %s (carried directly on the session token, not header-resolved)", p.ProjectID, projectID)
	}
}

// The single most important negative case: a session token must stop
// authenticating the instant its session closes — not just at token
// expiry. This is what makes the credential revocable without a
// blocklist (see MintSessionToken's doc comment).
func TestAuthenticate_SessionTokenForClosedSessionIsRejected(t *testing.T) {
	svc, _ := newMockService(t)
	m := NewMiddleware(svc, "test-secret").WithSessionChecker(fakeSessionChecker{open: false})

	token, err := MintSessionToken(uuid.New(), uuid.New(), uuid.New(), time.Hour, "test-secret")
	if err != nil {
		t.Fatalf("MintSessionToken: %v", err)
	}

	c := newTestContext(t)
	c.Request.Header.Set("Authorization", "Bearer "+token)

	if p := m.authenticate(c); p != nil {
		t.Errorf("authenticate succeeded for a closed session's token: %+v", p)
	}
}

// A checker error must fail closed (reject), not fail open — an
// unreachable DB must never be treated as "session still valid".
func TestAuthenticate_SessionTokenCheckerErrorFailsClosed(t *testing.T) {
	svc, _ := newMockService(t)
	m := NewMiddleware(svc, "test-secret").WithSessionChecker(fakeSessionChecker{err: errors.New("db unreachable")})

	token, err := MintSessionToken(uuid.New(), uuid.New(), uuid.New(), time.Hour, "test-secret")
	if err != nil {
		t.Fatalf("MintSessionToken: %v", err)
	}

	c := newTestContext(t)
	c.Request.Header.Set("Authorization", "Bearer "+token)

	if p := m.authenticate(c); p != nil {
		t.Errorf("authenticate succeeded despite a session-checker error: %+v", p)
	}
}

// A deployment with no session checker wired must reject a session token
// outright rather than silently trusting it as an ordinary JWT — this is
// the platform's most narrowly-scoped, most injection-exposed credential,
// so an unconfigured checker must not be a bypass.
func TestAuthenticate_SessionTokenWithNoCheckerWiredIsRejected(t *testing.T) {
	svc, _ := newMockService(t)
	m := NewMiddleware(svc, "test-secret") // no WithSessionChecker call

	token, err := MintSessionToken(uuid.New(), uuid.New(), uuid.New(), time.Hour, "test-secret")
	if err != nil {
		t.Fatalf("MintSessionToken: %v", err)
	}

	c := newTestContext(t)
	c.Request.Header.Set("Authorization", "Bearer "+token)

	if p := m.authenticate(c); p != nil {
		t.Errorf("authenticate succeeded for a session token with no checker configured: %+v", p)
	}
}

// An ordinary login JWT (no SessionID claim) must never touch the session
// checker at all — confirmed by using a checker that fails the test if
// called.
func TestAuthenticate_OrdinaryJWTNeverConsultsSessionChecker(t *testing.T) {
	svc, mock := newMockService(t)
	calledChecker := false
	m := NewMiddleware(svc, "test-secret").WithSessionChecker(
		fakeSessionCheckerFunc(func() { calledChecker = true }))

	user := &User{ID: uuid.New(), AccountID: uuid.New(), Email: "a@b.com", Role: RoleOwner}
	access, _, err := GenerateJWT(user, "acme", "test-secret")
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	c := newTestContext(t)
	c.Request.Header.Set("Authorization", "Bearer "+access)

	p := m.authenticate(c)
	if p == nil {
		t.Fatal("authenticate returned nil for an ordinary login JWT")
	}
	if calledChecker {
		t.Error("session checker was consulted for a token with no SessionID claim")
	}
	_ = mock
}

// A refresh token is structurally a valid JWT with the same claims as an
// access token — before TokenType existed, it authenticated ordinary API
// calls perfectly well for its full 7-day life, silently defeating the
// point of a 15-minute access token (found live 2026-08-23). Only
// /v1/auth/refresh may accept one.
func TestAuthenticate_RefreshTokenRejectedAsAccessToken(t *testing.T) {
	svc, mock := newMockService(t)
	m := NewMiddleware(svc, "test-secret")

	user := &User{ID: uuid.New(), AccountID: uuid.New(), Email: "a@b.com", Role: RoleOwner}
	_, refresh, err := GenerateJWT(user, "acme", "test-secret")
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	c := newTestContext(t)
	c.Request.Header.Set("Authorization", "Bearer "+refresh)

	if p := m.authenticate(c); p != nil {
		t.Error("authenticate accepted a refresh token as a bearer credential")
	}
	_ = mock
}

// fakeSessionCheckerFunc lets TestAuthenticate_OrdinaryJWTNeverConsultsSessionChecker
// observe whether IsSessionOpen was ever called, without needing a second
// struct type.
type fakeSessionCheckerFunc func()

func (f fakeSessionCheckerFunc) IsSessionOpen(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	f()
	return true, nil
}
