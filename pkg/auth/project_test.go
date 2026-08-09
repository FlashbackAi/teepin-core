// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func projectRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_id", "owner_id", "name", "slug",
		"description", "environment", "created_at", "updated_at",
	})
}

// TestUpdateProject_FiltersByAccount is the tenancy guard. Without the
// account_id predicate, learning a project UUID would be enough to
// rename another customer's project.
func TestUpdateProject_FiltersByAccount(t *testing.T) {
	service, mock := newMockService(t)

	accountID, projectID := uuid.New(), uuid.New()
	name := "renamed"

	mock.ExpectQuery(`UPDATE auth\.projects`).
		WithArgs(projectID, accountID, &name, nil, nil).
		WillReturnRows(projectRows().AddRow(
			projectID, accountID, uuid.New(), "renamed", "production",
			"", "prod", time.Now(), time.Now(),
		))

	project, err := service.UpdateProject(context.Background(), accountID, projectID,
		ProjectUpdate{Name: &name})
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if project.Name != "renamed" {
		t.Errorf("Name = %q, want renamed", project.Name)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestUpdateProject_SlugIsNotRenamed pins a deliberate decision.
//
// The slug appears in API keys, registry paths and customer scripts.
// Following a rename would silently break things the customer cannot see
// from the settings screen — the name is a label, the slug is an
// identifier.
func TestUpdateProject_SlugIsNotRenamed(t *testing.T) {
	service, mock := newMockService(t)

	accountID, projectID := uuid.New(), uuid.New()
	name := "new name"

	mock.ExpectQuery(`UPDATE auth\.projects`).
		WithArgs(projectID, accountID, &name, nil, nil).
		WillReturnRows(projectRows().AddRow(
			projectID, accountID, uuid.New(), "new name", "original-slug",
			"", "", time.Now(), time.Now(),
		))

	project, err := service.UpdateProject(context.Background(), accountID, projectID,
		ProjectUpdate{Name: &name})
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if project.Slug != "original-slug" {
		t.Errorf("Slug = %q; renaming must not change the slug", project.Slug)
	}
}

func TestUpdateProject_RejectsUnknownEnvironment(t *testing.T) {
	service, _ := newMockService(t)

	bad := "producton" // typo
	_, err := service.UpdateProject(context.Background(), uuid.New(), uuid.New(),
		ProjectUpdate{Environment: &bad})

	// Caught before the database: the CHECK constraint would also reject
	// it, but a typo that stored silently would show NO badge on a
	// production project, which is the failure the badge exists to
	// prevent.
	if !errors.Is(err, ErrInvalidEnvironment) {
		t.Errorf("error = %v, want ErrInvalidEnvironment", err)
	}
}

func TestUpdateProject_AcceptsEmptyEnvironment(t *testing.T) {
	service, mock := newMockService(t)

	accountID, projectID := uuid.New(), uuid.New()
	empty := ""

	mock.ExpectQuery(`UPDATE auth\.projects`).
		WillReturnRows(projectRows().AddRow(
			projectID, accountID, uuid.New(), "p", "p", "", "", time.Now(), time.Now(),
		))

	// "Not declared" is a legitimate state — a project need not be
	// labelled at all.
	if _, err := service.UpdateProject(context.Background(), accountID, projectID,
		ProjectUpdate{Environment: &empty}); err != nil {
		t.Errorf("empty environment should be allowed, got %v", err)
	}
}

// TestDeleteProject_RefusesWithRunningInstances is the important one.
//
// Deleting a project is an organisational decision; destroying running
// GPU workloads is not. Cascading would let a single confirmation end a
// training run that has been going for hours.
func TestDeleteProject_RefusesWithRunningInstances(t *testing.T) {
	service, mock := newMockService(t)

	accountID, projectID := uuid.New(), uuid.New()

	// GetProject: the project exists in this account.
	mock.ExpectQuery(`SELECT .+ FROM auth\.projects`).
		WillReturnRows(projectRows().AddRow(
			projectID, accountID, uuid.New(), "production", "production",
			"", "prod", time.Now(), time.Now(),
		))

	// Two instances still running.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM compute\.instances`).
		WithArgs(projectID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	err := service.DeleteProject(context.Background(), accountID, projectID)

	if !errors.Is(err, ErrProjectHasInstances) {
		t.Fatalf("error = %v, want ErrProjectHasInstances", err)
	}
	// No UPDATE was expected: nothing may be deleted or revoked while
	// the refusal stands.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("delete must touch nothing when refused: %v", err)
	}
}

func TestDeleteProject_RevokesKeysAndSoftDeletes(t *testing.T) {
	service, mock := newMockService(t)

	accountID, projectID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .+ FROM auth\.projects`).
		WillReturnRows(projectRows().AddRow(
			projectID, accountID, uuid.New(), "scratch", "scratch",
			"", "dev", time.Now(), time.Now(),
		))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM compute\.instances`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	mock.ExpectBegin()
	// Keys must be revoked with the project, or a deleted project's
	// credentials keep authenticating.
	mock.ExpectExec(`UPDATE auth\.api_keys SET revoked_at`).
		WithArgs(projectID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Soft delete: billing history references this project, and a hard
	// delete would orphan usage records behind a real invoice.
	mock.ExpectExec(`UPDATE auth\.projects SET deleted_at`).
		WithArgs(projectID, accountID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := service.DeleteProject(context.Background(), accountID, projectID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestDeleteProject_OtherAccountIsNotFound: a project in another account
// must be indistinguishable from one that does not exist, or the API
// confirms the existence of other customers' projects.
func TestDeleteProject_OtherAccountIsNotFound(t *testing.T) {
	service, mock := newMockService(t)

	mock.ExpectQuery(`SELECT .+ FROM auth\.projects`).
		WillReturnRows(projectRows()) // no match in this account

	err := service.DeleteProject(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("error = %v, want ErrProjectNotFound", err)
	}
}
