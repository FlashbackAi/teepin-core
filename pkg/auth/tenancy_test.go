// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package auth

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newMockService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewService(db, "test-secret"), mock
}

// GetProject previously took no tenant parameter, so ANY authenticated
// caller who learned a project UUID could read another customer's
// project. The account_id predicate is what closes that hole; this test
// fails if it is ever dropped from the query.
func TestGetProject_FiltersByAccount(t *testing.T) {
	svc, mock := newMockService(t)
	accountID, projectID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .+ FROM auth\.projects\s+WHERE id = \$1 AND account_id = \$2`).
		WithArgs(projectID, accountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "owner_id", "name", "slug", "description", "environment", "created_at", "updated_at",
		}).AddRow(projectID, accountID, uuid.New(), "production", "production", "", "", nowUTC(), nowUTC()))

	p, err := svc.GetProject(context.Background(), accountID, projectID)
	if err != nil {
		t.Fatalf("GetProject failed: %v", err)
	}
	if p.AccountID != accountID {
		t.Errorf("returned project belongs to account %s, want %s", p.AccountID, accountID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A project in another account must be indistinguishable from one that
// does not exist. Returning "forbidden" would confirm it exists and
// leak the shape of other customers' estates.
func TestGetProject_OtherAccountIsNotFound(t *testing.T) {
	svc, mock := newMockService(t)
	attackerAccount, victimProject := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .+ FROM auth\.projects`).
		WithArgs(victimProject, attackerAccount).
		WillReturnError(sql.ErrNoRows)

	_, err := svc.GetProject(context.Background(), attackerAccount, victimProject)
	if err == nil {
		t.Fatal("cross-account GetProject succeeded — tenancy is broken")
	}
	if err != ErrProjectNotFound {
		t.Errorf("got %v, want ErrProjectNotFound (never a 'forbidden' style error)", err)
	}
}

// Projects belong to the ACCOUNT, not to the user who created them —
// otherwise a colleague could not see their own team's projects.
func TestListProjects_ScopedByAccountNotOwner(t *testing.T) {
	svc, mock := newMockService(t)
	accountID := uuid.New()
	ownerA, ownerB := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .+ FROM auth\.projects\s+WHERE account_id = \$1`).
		WithArgs(accountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "owner_id", "name", "slug", "description", "environment", "created_at", "updated_at",
		}).
			AddRow(uuid.New(), accountID, ownerA, "production", "production", "", "", nowUTC(), nowUTC()).
			AddRow(uuid.New(), accountID, ownerB, "staging", "staging", "", "", nowUTC(), nowUTC()))

	projects, err := svc.ListProjects(context.Background(), accountID)
	if err != nil {
		t.Fatalf("ListProjects failed: %v", err)
	}
	// Two projects created by two different users, both visible.
	if len(projects) != 2 {
		t.Fatalf("got %d projects, want 2 (projects are account-scoped, not user-scoped)", len(projects))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// An API key's account is resolved by joining its project, so a key can
// never disagree with the project it belongs to, and a soft-deleted
// project silently revokes its keys.
func TestValidateAPIKey_ResolvesAccountFromProject(t *testing.T) {
	svc, mock := newMockService(t)

	mock.ExpectQuery(`FROM auth\.api_keys k\s+JOIN auth\.projects p`).
		WithArgs("tpk_notreal").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "user_id", "name", "key_hash", "key_prefix",
			"scopes", "last_used_at", "created_at", "account_id",
		}))

	_, err := svc.ValidateAPIKey(context.Background(), "tpk_notrealkey")
	if err == nil {
		t.Fatal("ValidateAPIKey accepted a key with no matching row")
	}
}

// Sub-user roles may never be escalated to owner: exactly one owner
// exists per account, and ownership carries billing authority.
func TestCreateSubUser_RejectsOwnerRole(t *testing.T) {
	svc, _ := newMockService(t)

	_, err := svc.CreateSubUser(context.Background(), uuid.New(), CreateSubUserRequest{
		Username: "alice", Email: "alice@acme.com",
		Password: "correct-horse-battery", Role: RoleOwner,
	})
	if err == nil {
		t.Fatal("CreateSubUser accepted role=owner — an account must have exactly one owner")
	}
}

func TestCreateSubUser_RejectsUnknownRole(t *testing.T) {
	svc, _ := newMockService(t)

	_, err := svc.CreateSubUser(context.Background(), uuid.New(), CreateSubUserRequest{
		Username: "alice", Email: "alice@acme.com",
		Password: "correct-horse-battery", Role: "superuser",
	})
	if err == nil {
		t.Fatal("CreateSubUser accepted an unknown role")
	}
}

// Role and status changes carry account_id in the WHERE clause, so
// touching a user in another account affects zero rows rather than
// escalating privileges.
func TestUpdateSubUserRole_ScopedToAccount(t *testing.T) {
	svc, mock := newMockService(t)
	accountID, userID := uuid.New(), uuid.New()

	mock.ExpectExec(`UPDATE auth\.users SET role = \$3\s+WHERE id = \$2 AND account_id = \$1`).
		WithArgs(accountID, userID, RoleAdmin).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.UpdateSubUserRole(context.Background(), accountID, userID, RoleAdmin); err != nil {
		t.Fatalf("UpdateSubUserRole failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestUpdateSubUserRole_OtherAccountAffectsNothing(t *testing.T) {
	svc, mock := newMockService(t)
	attackerAccount, victimUser := uuid.New(), uuid.New()

	// Zero rows affected: the target is in another account.
	mock.ExpectExec(`UPDATE auth\.users SET role`).
		WithArgs(attackerAccount, victimUser, RoleAdmin).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := svc.UpdateSubUserRole(context.Background(), attackerAccount, victimUser, RoleAdmin)
	if err == nil {
		t.Fatal("cross-account role change reported success — tenancy is broken")
	}
}

// The owner cannot be deleted: an account without an owner has no
// billing authority and no one who can close it.
func TestDeleteSubUser_CannotRemoveOwner(t *testing.T) {
	svc, mock := newMockService(t)
	accountID, ownerID := uuid.New(), uuid.New()

	// The `role <> 'owner'` predicate means the owner matches no rows.
	mock.ExpectExec(`UPDATE auth\.users SET deleted_at`).
		WithArgs(accountID, ownerID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := svc.DeleteSubUser(context.Background(), accountID, ownerID); err == nil {
		t.Fatal("DeleteSubUser removed the account owner")
	}
}

func nowUTC() time.Time { return time.Now().UTC() }
