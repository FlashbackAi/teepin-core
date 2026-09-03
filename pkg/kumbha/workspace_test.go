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
	mock.ExpectQuery(`SELECT v\.version, v\.is_checkpoint, v\.file_count`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "is_checkpoint", "file_count"}).AddRow(nil, nil, nil))
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

// TestSaveVersion_StripsInternalScratchFiles is the regression test for
// the AGENTS.md-at-workspace-root gap flagged 2026-09-01 ("why is the
// agent writing its own AGENTS.md in the app codebase instead of keeping
// it hidden in the backend"): anything the agent (or, in principle, a
// customer save) submits under InternalScratchDir must never reach the
// database at all — not just be hidden by the console later. The
// INSERT's own expected file_count (2, not 3) and byte total (excluding
// the scratch file's content) are the actual assertion: the mock would
// simply not match if stripping did not happen before validation/counting.
func TestSaveVersion_StripsInternalScratchFiles(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT v\.version, v\.is_checkpoint, v\.file_count`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "is_checkpoint", "file_count"}).AddRow(nil, nil, nil))
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
		{Path: InternalScratchDir + "/AGENTS.md", Content: "private notes the agent should never publish"},
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

// TestStripInternalScratchFiles_Cases exercises the filter directly for
// path-matching edge cases the store-level test above cannot cheaply
// cover on its own.
func TestStripInternalScratchFiles_Cases(t *testing.T) {
	in := []WorkspaceFile{
		{Path: "index.html", Content: "kept"},
		{Path: InternalScratchDir + "/AGENTS.md", Content: "stripped: direct child"},
		{Path: InternalScratchDir + "/notes/scratch.txt", Content: "stripped: nested"},
		{Path: "./" + InternalScratchDir + "/leading-dot-slash.md", Content: "stripped: leading ./"},
		{Path: "src/" + InternalScratchDir + "/not-at-root.md", Content: "kept: not workspace-root-anchored"},
		{Path: InternalScratchDir, Content: "stripped: bare dir entry, no trailing slash"},
	}
	out := stripInternalScratchFiles(in)

	wantPaths := map[string]bool{
		"index.html":                          true,
		"src/" + InternalScratchDir + "/not-at-root.md": true,
	}
	if len(out) != len(wantPaths) {
		t.Fatalf("got %d files after stripping, want %d: %+v", len(out), len(wantPaths), out)
	}
	for _, f := range out {
		if !wantPaths[f.Path] {
			t.Errorf("unexpected file survived stripping: %q", f.Path)
		}
	}
}

// TestSaveVersion_CustomerSaveIsNotCheckpointedImmediately is the
// regression test for a live 2026-08-31 product change: a customer's own
// edit-and-save used to be checkpointed the instant it was saved,
// showing up in "Version history" whether or not it was ever deployed.
// It must now behave exactly like the agent's own auto-save — an
// uncheckpointed draft — so History only ever shows real, deployed
// versions. CheckpointCurrentVersion (called at a REAL deploy) is the
// only thing that still flips is_checkpoint.
func TestSaveVersion_CustomerSaveIsNotCheckpointedImmediately(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT v\.version, v\.is_checkpoint, v\.file_count`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "is_checkpoint", "file_count"}).AddRow(nil, nil, nil))
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM billing\.kumbha_workspace_versions`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO billing\.kumbha_workspace_versions`).
		WithArgs(sessionID, 1, sqlmock.AnyArg(), sqlmock.AnyArg(), 1, int64(len("hi")), string(CreatedByCustomer)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET current_workspace_version`).
		WithArgs(1, sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if _, err := store.SaveVersion(context.Background(), sessionID,
		[]WorkspaceFile{{Path: "a.txt", Content: "hi"}}, nil, CreatedByCustomer); err != nil {
		t.Fatalf("SaveVersion: %v", err)
	}
	// The INSERT's own hardcoded `false` for is_checkpoint (verified by
	// the exact arg list above having no 8th bool argument) is the actual
	// assertion here — mock.ExpectExec would fail to match otherwise.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestSaveVersion_CustomerSaveReusesUncheckpointedDraftInPlace proves a
// SECOND customer save against a still-undeployed draft updates the SAME
// row (like the agent's own repeated auto-saves) rather than piling up a
// new History-eligible entry per save.
func TestSaveVersion_CustomerSaveReusesUncheckpointedDraftInPlace(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT v\.version, v\.is_checkpoint, v\.file_count`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "is_checkpoint", "file_count"}).AddRow(3, false, 1))
	mock.ExpectExec(`UPDATE billing\.kumbha_workspace_versions`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), 1, int64(len("hi again")), sessionID, int64(3), string(CreatedByCustomer)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	version, err := store.SaveVersion(context.Background(), sessionID,
		[]WorkspaceFile{{Path: "a.txt", Content: "hi again"}}, nil, CreatedByCustomer)
	if err != nil {
		t.Fatalf("SaveVersion: %v", err)
	}
	if version != 3 {
		t.Errorf("got version %d, want the reused draft version 3", version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSaveVersion_IncrementsFromExistingMax(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT v\.version, v\.is_checkpoint, v\.file_count`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "is_checkpoint", "file_count"}).AddRow(4, true, 1))
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
	mock.ExpectQuery(`SELECT v\.version, v\.is_checkpoint, v\.file_count`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "is_checkpoint", "file_count"}).AddRow(nil, nil, nil))
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM billing\.kumbha_workspace_versions`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(MaxWorkspaceVersions))

	_, err := store.SaveVersion(context.Background(), sessionID,
		[]WorkspaceFile{{Path: "a.txt", Content: "hi"}}, nil, CreatedByAgent)
	if !errors.Is(err, ErrTooManyVersions) {
		t.Errorf("got %v, want ErrTooManyVersions", err)
	}
}

func TestSaveVersion_AgentReusesUncheckpointedDraftInPlace(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT v\.version, v\.is_checkpoint, v\.file_count`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "is_checkpoint", "file_count"}).AddRow(3, false, 1))
	mock.ExpectExec(`UPDATE billing\.kumbha_workspace_versions`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), 1, int64(len("hi")), sessionID, int64(3), string(CreatedByAgent)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	version, err := store.SaveVersion(context.Background(), sessionID,
		[]WorkspaceFile{{Path: "a.txt", Content: "hi"}}, nil, CreatedByAgent)
	if err != nil {
		t.Fatalf("SaveVersion: %v", err)
	}
	if version != 3 {
		t.Errorf("got version %d, want the reused draft version 3", version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestSaveVersion_EmptyAgentSaveDoesNotClobberRealDraft is the regression
// test for a live 2026-08-31 incident: run.py calls upload_workspace()
// unconditionally at the end of EVERY run, including one that never
// touched a file. Before this guard, that automatic final save silently
// overwrote a real, already-deployed build's only saved draft with zero
// files the moment a relaunch found an already-empty PVC (a separate PVC-
// wipe bug, fixed the same day) and did nothing else — destroying the
// last remaining copy of the actual source in the database too. An empty
// save must be a no-op when the current draft already has real content.
func TestSaveVersion_EmptyAgentSaveDoesNotClobberRealDraft(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT v\.version, v\.is_checkpoint, v\.file_count`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "is_checkpoint", "file_count"}).AddRow(3, false, 4))
	// No UPDATE expected — an empty save against a non-empty draft must be
	// refused, not silently applied.
	mock.ExpectCommit()

	version, err := store.SaveVersion(context.Background(), sessionID, nil, nil, CreatedByAgent)
	if err != nil {
		t.Fatalf("SaveVersion: %v", err)
	}
	if version != 3 {
		t.Errorf("got version %d, want the existing draft version 3 unchanged", version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestSaveVersion_EmptyAgentSaveOverEmptyDraftIsFine proves the guard is
// specifically about REGRESSING real content, not about rejecting empty
// saves outright: a session with no files yet saving nothing is the
// ordinary case (a fresh session's first automatic save before the agent
// has written anything), not something to refuse.
func TestSaveVersion_EmptyAgentSaveOverEmptyDraftIsFine(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT v\.version, v\.is_checkpoint, v\.file_count`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "is_checkpoint", "file_count"}).AddRow(3, false, 0))
	mock.ExpectExec(`UPDATE billing\.kumbha_workspace_versions`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), 0, int64(0), sessionID, int64(3), string(CreatedByAgent)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	version, err := store.SaveVersion(context.Background(), sessionID, nil, nil, CreatedByAgent)
	if err != nil {
		t.Fatalf("SaveVersion: %v", err)
	}
	if version != 3 {
		t.Errorf("got version %d, want 3", version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSaveVersion_AgentStartsNewDraftAfterCurrentIsCheckpointed(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT v\.version, v\.is_checkpoint, v\.file_count`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "is_checkpoint", "file_count"}).AddRow(3, true, 1))
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM billing\.kumbha_workspace_versions`).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(3))
	mock.ExpectExec(`INSERT INTO billing\.kumbha_workspace_versions`).
		WithArgs(sessionID, 4, sqlmock.AnyArg(), sqlmock.AnyArg(), 1, int64(len("hi")), string(CreatedByAgent)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET current_workspace_version`).
		WithArgs(4, sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	version, err := store.SaveVersion(context.Background(), sessionID,
		[]WorkspaceFile{{Path: "a.txt", Content: "hi"}}, nil, CreatedByAgent)
	if err != nil {
		t.Fatalf("SaveVersion: %v", err)
	}
	if version != 4 {
		t.Errorf("got version %d, want a new draft at 4 (not overwriting the checkpointed version 3)", version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestCheckpointCurrentVersion_FlipsOnlyTheCurrentDraft(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID := uuid.New()

	mock.ExpectExec(`UPDATE billing\.kumbha_workspace_versions v\s+SET is_checkpoint = true`).
		WithArgs(sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.inference_sessions\s+SET last_deployed_version = current_workspace_version`).
		WithArgs(sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.CheckpointCurrentVersion(context.Background(), sessionID); err != nil {
		t.Fatalf("CheckpointCurrentVersion: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
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
		WillReturnRows(sqlmock.NewRows([]string{"version", "files", "skipped", "file_count", "byte_size", "created_by", "created_at", "is_checkpoint", "is_deployed"}).
			AddRow(3, `[{"path":"a.txt","content":"hi"}]`, `[]`, 1, 2, "agent", time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC), true, true))

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

// TestGetCurrentVersion_CheckpointedButUndeployedCustomerSaveIsNotDeployed
// is the regression test for a live 2026-08-31 incident: a customer
// edited a file in the console IDE and saved. SaveVersion checkpoints a
// customer save immediately (so it shows in History right away), but that
// is NOT the same fact as "this content is running" — before this fix,
// the console's Deploy button read is_checkpoint alone and disabled
// itself with "Already deployed" for a version that had never been
// deployed. is_deployed must stay false until a real deploy stamps
// last_deployed_version, independent of is_checkpoint.
func TestGetCurrentVersion_CheckpointedButUndeployedCustomerSaveIsNotDeployed(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID, accountID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT current_workspace_version FROM billing\.inference_sessions`).
		WithArgs(sessionID, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"current_workspace_version"}).AddRow(3))
	mock.ExpectQuery(`FROM billing\.kumbha_workspace_versions v`).
		WithArgs(sessionID, accountID, 3).
		WillReturnRows(sqlmock.NewRows([]string{"version", "files", "skipped", "file_count", "byte_size", "created_by", "created_at", "is_checkpoint", "is_deployed"}).
			// is_checkpoint=true (a customer save, checkpointed on arrival)
			// but is_deployed=false (version 3 != last_deployed_version,
			// still pointing at an earlier version — e.g. 1).
			AddRow(3, `[{"path":"a.txt","content":"edited"}]`, `[]`, 1, 7, "customer", time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), true, false))

	snap, err := store.GetCurrentVersion(context.Background(), sessionID, accountID)
	if err != nil {
		t.Fatalf("GetCurrentVersion: %v", err)
	}
	if !snap.IsCheckpoint {
		t.Error("IsCheckpoint = false, want true (a customer save is checkpointed on arrival)")
	}
	if snap.IsDeployed {
		t.Error("IsDeployed = true, want false — this version has never actually been deployed")
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
		WillReturnRows(sqlmock.NewRows([]string{"version", "file_count", "byte_size", "created_by", "created_at", "is_current", "is_deployed"}).
			AddRow(2, 3, 100, "customer", time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC), true, false).
			AddRow(1, 3, 90, "agent", time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC), false, true))

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

func TestListVersions_QueryExcludesUncheckpointedDrafts(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID, accountID := uuid.New(), uuid.New()

	// Asserts the query text itself carries the is_checkpoint filter
	// (rather than relying on the fake row set to prove it, since sqlmock
	// returns whatever rows are stubbed regardless of the real WHERE
	// clause) — regression coverage for the split introduced by migration
	// 028: an agent's in-progress draft must never surface here.
	mock.ExpectQuery(`WHERE v\.session_id = \$1 AND s\.account_id = \$2 AND v\.is_checkpoint`).
		WithArgs(sessionID, accountID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "file_count", "byte_size", "created_by", "created_at", "is_current"}))

	if _, err := store.ListVersions(context.Background(), sessionID, accountID); err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
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

func TestSetCurrentVersion_QueryRequiresCheckpointedTarget(t *testing.T) {
	store, mock := newMockStore(t)
	sessionID, accountID := uuid.New(), uuid.New()

	// An agent's in-progress draft (is_checkpoint=false) must never be a
	// valid rollback target — asserted against the query text itself,
	// same reasoning as TestListVersions_QueryExcludesUncheckpointedDrafts.
	mock.ExpectExec(`WHERE session_id = \$2 AND version = \$1 AND is_checkpoint`).
		WithArgs(2, sessionID, accountID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.SetCurrentVersion(context.Background(), sessionID, accountID, 2); err != nil {
		t.Fatalf("SetCurrentVersion: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
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
