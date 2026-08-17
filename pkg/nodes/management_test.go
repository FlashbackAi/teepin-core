// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nodes

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// DeleteNode removes a node with no active instances.
func TestDeleteNode_NoInstances(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()
	id := uuid.New()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM compute\.instances\s+WHERE node_id = \$1 AND terminated_at IS NULL`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`DELETE FROM compute\.nodes WHERE id = \$1`).
		WithArgs(id).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.DeleteNode(context.Background(), id); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// DeleteNode refuses when active instances remain — no DELETE runs.
func TestDeleteNode_WithActiveInstances(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()
	id := uuid.New()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM compute\.instances`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	if err := s.DeleteNode(context.Background(), id); !errors.Is(err, ErrNodeHasInstances) {
		t.Fatalf("err = %v, want ErrNodeHasInstances", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (a DELETE should NOT have run): %v", err)
	}
}

// A missing node is ErrNotFound.
func TestDeleteNode_NotFound(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()
	id := uuid.New()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM compute\.instances`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`DELETE FROM compute\.nodes`).
		WithArgs(id).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := s.DeleteNode(context.Background(), id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// RenameNode updates the node_name.
func TestRenameNode(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()
	id := uuid.New()

	mock.ExpectExec(`UPDATE compute\.nodes SET node_name = \$1`).
		WithArgs("new-name", id).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.RenameNode(context.Background(), id, "new-name"); err != nil {
		t.Fatalf("RenameNode: %v", err)
	}
}

// An empty name is rejected before any query.
func TestRenameNode_EmptyRejected(t *testing.T) {
	s, _, done := newMock(t)
	defer done()
	if err := s.RenameNode(context.Background(), uuid.New(), "  "); err == nil {
		t.Fatal("empty name accepted")
	}
}
