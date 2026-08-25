// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package auth

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// Refresh existed as an unused field until 2026-08-23: a refresh token was
// minted and stored client-side at login, but nothing ever redeemed it, so
// the console signed customers out every 15 minutes (the access token's
// TTL) instead of refreshing silently. These tests cover the endpoint that
// closes that gap.

func TestRefresh_ValidTokenIssuesNewPair(t *testing.T) {
	svc, mock := newMockService(t)

	user := &User{ID: uuid.New(), AccountID: uuid.New(), Email: "a@b.com", Role: RoleOwner, Status: "active"}
	_, refresh, err := GenerateJWT(user, "acme", "test-secret")
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	mock.ExpectQuery(`SELECT .+ FROM auth\.users\s+WHERE id = \$1`).
		WithArgs(user.ID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "email", "username", "role", "status",
			"password_hash", "full_name", "email_verified", "created_at", "updated_at",
		}).AddRow(user.ID, user.AccountID, user.Email, "", user.Role, "active",
			"hash", "", false, nowUTC(), nowUTC()))
	mock.ExpectQuery(`SELECT alias FROM auth\.accounts`).
		WithArgs(user.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{"alias"}).AddRow("acme"))

	access, newRefresh, err := svc.Refresh(context.Background(), refresh)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if access == "" || newRefresh == "" {
		t.Error("Refresh returned an empty token")
	}
	if newRefresh == refresh {
		t.Error("Refresh did not rotate the refresh token")
	}

	claims, err := VerifyJWT(access, "test-secret")
	if err != nil {
		t.Fatalf("VerifyJWT(access): %v", err)
	}
	if claims.TokenType != "access" {
		t.Errorf("issued token_type = %q, want \"access\"", claims.TokenType)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The one behaviour this whole feature exists to prevent: an access token
// must not work as a refresh token either. Without the TokenType check
// this would have quietly succeeded.
func TestRefresh_RejectsAccessTokenInPlaceOfRefreshToken(t *testing.T) {
	svc, _ := newMockService(t)

	user := &User{ID: uuid.New(), AccountID: uuid.New(), Email: "a@b.com", Role: RoleOwner}
	access, _, err := GenerateJWT(user, "acme", "test-secret")
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	if _, _, err := svc.Refresh(context.Background(), access); err == nil {
		t.Error("Refresh accepted an access token in place of a refresh token")
	}
}

func TestRefresh_RejectsGarbageToken(t *testing.T) {
	svc, _ := newMockService(t)
	if _, _, err := svc.Refresh(context.Background(), "not-a-jwt"); err == nil {
		t.Error("Refresh accepted a malformed token")
	}
}

// An account disabled after the refresh token was issued must stop
// minting new access tokens immediately — the same check Login itself
// makes, applied here so a 7-day-old refresh token cannot outlive a
// disabled account by up to 7 days.
func TestRefresh_RejectsDisabledAccount(t *testing.T) {
	svc, mock := newMockService(t)

	user := &User{ID: uuid.New(), AccountID: uuid.New(), Email: "a@b.com", Role: RoleOwner}
	_, refresh, err := GenerateJWT(user, "acme", "test-secret")
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	mock.ExpectQuery(`SELECT .+ FROM auth\.users\s+WHERE id = \$1`).
		WithArgs(user.ID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "email", "username", "role", "status",
			"password_hash", "full_name", "email_verified", "created_at", "updated_at",
		}).AddRow(user.ID, user.AccountID, user.Email, "", user.Role, "disabled",
			"hash", "", false, nowUTC(), nowUTC()))

	if _, _, err := svc.Refresh(context.Background(), refresh); err == nil {
		t.Error("Refresh issued tokens for a disabled account")
	}
}
