// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package auth

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

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
