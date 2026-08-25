// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestSanitizeWorkspacePath_RejectsAbsolutePath(t *testing.T) {
	if _, err := sanitizeWorkspacePath("/etc/passwd"); !errors.Is(err, ErrInvalidSnapshot) {
		t.Errorf("got %v, want ErrInvalidSnapshot for an absolute path", err)
	}
}

// The Zip Slip guard: a path like this, unpacked by a customer on their
// own machine via the ZIP download, would write outside the archive's own
// directory — and the agent's file list is model-influenced content, not
// trusted input.
func TestSanitizeWorkspacePath_RejectsTraversal(t *testing.T) {
	cases := []string{"../../.ssh/authorized_keys", "js/../../etc/passwd", ".."}
	for _, c := range cases {
		if _, err := sanitizeWorkspacePath(c); !errors.Is(err, ErrInvalidSnapshot) {
			t.Errorf("sanitizeWorkspacePath(%q) = %v, want ErrInvalidSnapshot", c, err)
		}
	}
}

func TestSanitizeWorkspacePath_NormalizesBackslashesBeforeChecking(t *testing.T) {
	// A Windows-style separator must not slip past the traversal check by
	// looking like one long filename instead of a path with ".." segments.
	if _, err := sanitizeWorkspacePath(`..\..\windows\system32`); !errors.Is(err, ErrInvalidSnapshot) {
		t.Errorf("got %v, want ErrInvalidSnapshot for a backslash-separated traversal", err)
	}
}

func TestSanitizeWorkspacePath_AllowsOrdinaryRelativePaths(t *testing.T) {
	got, err := sanitizeWorkspacePath("js/views/dashboard.js")
	if err != nil {
		t.Fatalf("sanitizeWorkspacePath: %v", err)
	}
	if got != "js/views/dashboard.js" {
		t.Errorf("got %q, want the path preserved unchanged", got)
	}
}

func TestSanitizeWorkspacePath_RejectsEmpty(t *testing.T) {
	if _, err := sanitizeWorkspacePath(""); !errors.Is(err, ErrInvalidSnapshot) {
		t.Errorf("got %v, want ErrInvalidSnapshot for an empty path", err)
	}
}

func TestSaveVersion_RejectsTooManyFiles(t *testing.T) {
	store, mock := newMockStore(t)
	files := make([]WorkspaceFile, MaxWorkspaceFiles+1)
	for i := range files {
		files[i] = WorkspaceFile{Path: "f", Content: "x"}
	}

	_, err := store.SaveVersion(context.Background(), uuid.New(), files, nil, CreatedByAgent)
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Errorf("got %v, want ErrInvalidSnapshot", err)
	}
	// The file-count check must reject before ever touching the database —
	// a caller sending an absurd payload should not cost a round trip.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected database interaction: %v", err)
	}
}

func TestSaveVersion_RejectsOversizedFile(t *testing.T) {
	store, mock := newMockStore(t)
	big := strings.Repeat("a", MaxWorkspaceFileBytes+1)

	_, err := store.SaveVersion(context.Background(), uuid.New(),
		[]WorkspaceFile{{Path: "big.txt", Content: big}}, nil, CreatedByAgent)
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Errorf("got %v, want ErrInvalidSnapshot", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected database interaction: %v", err)
	}
}

func TestSaveVersion_RejectsOversizedTotal(t *testing.T) {
	store, mock := newMockStore(t)
	// Individually within MaxWorkspaceFileBytes, but enough of them to push
	// the sum over MaxWorkspaceTotalBytes.
	chunk := strings.Repeat("a", MaxWorkspaceFileBytes)
	var files []WorkspaceFile
	for total := 0; total <= MaxWorkspaceTotalBytes; total += MaxWorkspaceFileBytes {
		files = append(files, WorkspaceFile{Path: uuid.NewString(), Content: chunk})
	}

	_, err := store.SaveVersion(context.Background(), uuid.New(), files, nil, CreatedByAgent)
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Errorf("got %v, want ErrInvalidSnapshot", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected database interaction: %v", err)
	}
}

func TestSaveVersion_RejectsPathEscapingWorkspaceRoot(t *testing.T) {
	store, mock := newMockStore(t)
	_, err := store.SaveVersion(context.Background(), uuid.New(),
		[]WorkspaceFile{{Path: "../../etc/passwd", Content: "x"}}, nil, CreatedByAgent)
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Errorf("got %v, want ErrInvalidSnapshot", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected database interaction: %v", err)
	}
}

func TestSaveVersion_InsertsVersionOneWhenNoneExistAndMovesPointer(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM billing\.kumbha_workspace_versions`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO billing\.kumbha_workspace_versions`).
		WithArgs(sessionID, 1, sqlmock.AnyArg(), sqlmock.AnyArg(), 2, int64(len("hello")+len("world")), string(CreatedByAgent)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET current_workspace_version`).
		WithArgs(1, sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	version, err := store.SaveVersion(context.Background(), sessionID, []WorkspaceFile{
		{Path: "a.txt", Content: "hello"},
		{Path: "b.txt", Content: "world"},
	}, nil, CreatedByAgent)
	if err != nil {
		t.Fatalf("SaveVersion: %v", err)
	}
	if version != 1 {
		t.Errorf("got version %d, want 1", version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSaveVersion_IncrementsFromExistingMax(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM billing\.kumbha_workspace_versions`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(4))
	mock.ExpectExec(`INSERT INTO billing\.kumbha_workspace_versions`).
		WithArgs(sessionID, 5, sqlmock.AnyArg(), sqlmock.AnyArg(), 1, int64(len("hi")), string(CreatedByCustomer)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET current_workspace_version`).
		WithArgs(5, sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	version, err := store.SaveVersion(context.Background(), sessionID,
		[]WorkspaceFile{{Path: "a.txt", Content: "hi"}}, nil, CreatedByCustomer)
	if err != nil {
		t.Fatalf("SaveVersion: %v", err)
	}
	if version != 5 {
		t.Errorf("got version %d, want 5", version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSaveVersion_RefusesPastVersionLimit(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM billing\.kumbha_workspace_versions`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(MaxWorkspaceVersions))

	_, err := store.SaveVersion(context.Background(), sessionID,
		[]WorkspaceFile{{Path: "a.txt", Content: "hi"}}, nil, CreatedByAgent)
	if !errors.Is(err, ErrTooManyVersions) {
		t.Errorf("got %v, want ErrTooManyVersions", err)
	}
}

func TestGetCurrentVersion_ScopesToOwningAccountAndFollowsPointer(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID, accountID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT current_workspace_version FROM billing\.inference_sessions`).
		WithArgs(sessionID, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"current_workspace_version"}).AddRow(3))
	mock.ExpectQuery(`FROM billing\.kumbha_workspace_versions v`).
		WithArgs(sessionID, accountID, 3).
		WillReturnRows(sqlmock.NewRows([]string{"version", "files", "skipped", "file_count", "byte_size", "created_by", "created_at"}).
			AddRow(3, `[{"path":"a.txt","content":"hi"}]`, `[]`, 1, 2, "agent", time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)))

	snap, err := store.GetCurrentVersion(context.Background(), sessionID, accountID)
	if err != nil {
		t.Fatalf("GetCurrentVersion: %v", err)
	}
	if snap.Version != 3 || len(snap.Files) != 1 || snap.Files[0].Path != "a.txt" {
		t.Errorf("got %+v, want version 3 with one file a.txt", snap)
	}
	if snap.CreatedBy != CreatedByAgent {
		t.Errorf("got created_by %q, want %q", snap.CreatedBy, CreatedByAgent)
	}
}

func TestGetCurrentVersion_NullPointerIsErrNoWorkspace(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID, accountID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT current_workspace_version FROM billing\.inference_sessions`).
		WithArgs(sessionID, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"current_workspace_version"}).AddRow(nil))

	_, err := store.GetCurrentVersion(context.Background(), sessionID, accountID)
	if !errors.Is(err, ErrNoWorkspace) {
		t.Errorf("got %v, want ErrNoWorkspace", err)
	}
}

func TestGetCurrentVersion_UnknownSessionIsErrSessionNotFound(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID, accountID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT current_workspace_version FROM billing\.inference_sessions`).
		WithArgs(sessionID, accountID).
		WillReturnError(sql.ErrNoRows)

	_, err := store.GetCurrentVersion(context.Background(), sessionID, accountID)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("got %v, want ErrSessionNotFound", err)
	}
}

func TestGetVersion_UnknownVersionIsErrVersionNotFound(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID, accountID := uuid.New(), uuid.New()

	mock.ExpectQuery(`FROM billing\.kumbha_workspace_versions v`).
		WithArgs(sessionID, accountID, 9).
		WillReturnError(sql.ErrNoRows)

	_, err := store.GetVersion(context.Background(), sessionID, accountID, 9)
	if !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("got %v, want ErrVersionNotFound", err)
	}
}

func TestListVersions_ReturnsNewestFirstWithCurrentFlagged(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID, accountID := uuid.New(), uuid.New()

	mock.ExpectQuery(`FROM billing\.kumbha_workspace_versions v`).
		WithArgs(sessionID, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "file_count", "byte_size", "created_by", "created_at", "is_current"}).
			AddRow(2, 3, 100, "customer", time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC), true).
			AddRow(1, 3, 90, "agent", time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC), false))

	versions, err := store.ListVersions(context.Background(), sessionID, accountID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("got %d versions, want 2", len(versions))
	}
	if versions[0].Version != 2 || !versions[0].Current {
		t.Errorf("got %+v, want version 2 marked current", versions[0])
	}
	if versions[1].Version != 1 || versions[1].Current {
		t.Errorf("got %+v, want version 1 not current", versions[1])
	}
}

func TestSetCurrentVersion_MovesPointerWhenVersionExists(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID, accountID := uuid.New(), uuid.New()

	mock.ExpectExec(`UPDATE billing\.inference_sessions`).
		WithArgs(2, sessionID, accountID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.SetCurrentVersion(context.Background(), sessionID, accountID, 2); err != nil {
		t.Fatalf("SetCurrentVersion: %v", err)
	}
}

func TestSetCurrentVersion_UnknownVersionIsErrVersionNotFound(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID, accountID := uuid.New(), uuid.New()

	mock.ExpectExec(`UPDATE billing\.inference_sessions`).
		WithArgs(99, sessionID, accountID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := store.SetCurrentVersion(context.Background(), sessionID, accountID, 99)
	if !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("got %v, want ErrVersionNotFound", err)
	}
}

func TestSnapshot_WriteZip_ProducesAReadableArchiveWithExactContent(t *testing.T) {
	snap := &Snapshot{Files: []WorkspaceFile{
		{Path: "index.html", Content: "<h1>hi</h1>"},
		{Path: "js/app.js", Content: "console.log('hi')"},
	}}

	var buf bytes.Buffer
	if err := snap.WriteZip(&buf); err != nil {
		t.Fatalf("WriteZip: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("archive is not a valid zip: %v", err)
	}
	if len(zr.File) != 2 {
		t.Fatalf("got %d entries, want 2", len(zr.File))
	}

	byName := map[string]*zip.File{}
	for _, f := range zr.File {
		byName[f.Name] = f
	}
	for _, want := range snap.Files {
		f, ok := byName[want.Path]
		if !ok {
			t.Errorf("archive missing entry %q", want.Path)
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %q: %v", want.Path, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %q: %v", want.Path, err)
		}
		if string(got) != want.Content {
			t.Errorf("entry %q content = %q, want %q", want.Path, got, want.Content)
		}
	}
}
