// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Limits on what one version may carry. The agent enforces its own copy
// of these before uploading (so it never wastes a round trip on a payload
// that will be refused), and the console enforces them before an editable
// save — but they are re-checked here regardless, because the agent is
// the one component in this system most exposed to prompt injection, and
// a limit only the caller enforces is not a limit.
const (
	// MaxWorkspaceFiles bounds the file count. A generated web app is tens
	// of files; a runaway build that somehow produced thousands is a bug
	// worth refusing rather than storing.
	MaxWorkspaceFiles = 2000
	// MaxWorkspaceFileBytes bounds a single file. Comfortably past any
	// hand-written source file, well short of a bundled binary.
	MaxWorkspaceFileBytes = 512 * 1024
	// MaxWorkspaceTotalBytes bounds one version's total size. See
	// migration 025's own note on moving to S3 if this ever becomes the
	// binding constraint — versioning multiplies storage per session, so
	// this matters more here than it did for the original flat design.
	MaxWorkspaceTotalBytes = 16 * 1024 * 1024
	// MaxWorkspaceVersions caps how many CHECKPOINTED versions one session
	// may accumulate (see SaveVersion/CheckpointCurrentVersion — an
	// agent's in-progress draft reuses one row in place and does not count
	// against this until it is checkpointed at a successful deploy).
	// Refusing new checkpoints past this (rather than silently pruning old
	// ones, which would break "roll back to version 3") forces a real
	// decision if a session is ever this active.
	MaxWorkspaceVersions = 500
)

// InternalScratchDir is the one workspace directory name SaveVersion
// always strips from what actually gets persisted, checkpointed,
// ZIP-exported, or shown in the console file browser — see LaunchAgent's
// internalScratchPathInstruction for the agent-facing half of this (the
// prompt telling it where to put its own working notes/memory instead of
// the workspace root, which is the customer's actual deployed site).
// This is the hard guarantee: even a prompt the agent ignores cannot
// leak internal notes into a customer's ZIP download or version
// history, matching this file's own "a limit only the caller enforces is
// not a limit" reasoning above. Deferred 2026-09-01 ("why is the agent
// writing its own AGENTS.md in the app codebase instead of keeping it
// hidden in the backend"), addressed 2026-09-02. Does not by itself keep
// it out of the BUILT image — that still depends on the agent's own
// Dockerfile not copying it in (same as the AGENTS.md-at-root case
// tonight), which the prompt instruction also covers.
const InternalScratchDir = ".teepin-internal"

// stripInternalScratchFiles removes anything under InternalScratchDir
// from files before SaveVersion ever validates, counts, or persists it —
// internal notes should not eat into a customer's own workspace
// size/file-count budget either.
func stripInternalScratchFiles(files []WorkspaceFile) []WorkspaceFile {
	prefix := InternalScratchDir + "/"
	out := make([]WorkspaceFile, 0, len(files))
	for _, f := range files {
		p := strings.TrimPrefix(f.Path, "./")
		if p == InternalScratchDir || strings.HasPrefix(p, prefix) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// WorkspaceFile is one text file from the agent's or customer's edit of
// the workspace.
type WorkspaceFile struct {
	// Path is workspace-relative and always forward-slashed ("js/app.js").
	Path string `json:"path"`
	// Content is the file's text. Binary files are never stored — they are
	// reported in Snapshot.Skipped instead.
	Content string `json:"content"`
}

// SkippedFile records something deliberately not stored, so the console
// can say what is missing rather than presenting a partial tree as
// complete.
type SkippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// CreatedBy identifies who produced a version — shown in the console's
// history list, since "the agent wrote this automatically" and "I edited
// this myself" are different things to scan when picking a rollback
// target.
type CreatedBy string

const (
	CreatedByAgent    CreatedBy = "agent"
	CreatedByCustomer CreatedBy = "customer"
)

// Snapshot is one version's full content — what the file browser and ZIP
// download read.
type Snapshot struct {
	Version   int             `json:"version"`
	Files     []WorkspaceFile `json:"files"`
	Skipped   []SkippedFile   `json:"skipped"`
	FileCount int             `json:"file_count"`
	ByteSize  int64           `json:"byte_size"`
	CreatedBy CreatedBy       `json:"created_by"`
	CreatedAt time.Time       `json:"created_at"`
	// IsCheckpoint means this version is worth showing in the customer-
	// facing "Version history" list — true once a deploy has checkpointed
	// it (CheckpointCurrentVersion), OR immediately for a customer's own
	// edit-and-save (so it shows up right away, without waiting for a
	// redeploy). It does NOT mean this content is currently running — see
	// IsDeployed below, which used to be conflated with this field and
	// broke live 2026-08-31: a customer save is checkpointed on arrival
	// but had obviously never been deployed.
	IsCheckpoint bool `json:"is_checkpoint"`
	// IsDeployed means THIS version is byte-for-byte what CheckpointCurrentVersion
	// last stamped as inference_sessions.last_deployed_version — i.e. a
	// real successful deploy actually built and ran this exact content.
	// This, not IsCheckpoint, is what the console's Deploy button reads to
	// disable itself when a click would rebuild something that isn't
	// actually different.
	IsDeployed bool `json:"is_deployed"`
}

// VersionInfo is one entry in the history list — metadata only, no file
// content, so listing a session's whole history stays cheap regardless of
// how large individual versions are.
type VersionInfo struct {
	Version   int       `json:"version"`
	FileCount int       `json:"file_count"`
	ByteSize  int64     `json:"byte_size"`
	CreatedBy CreatedBy `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	// Current marks the version inference_sessions.current_workspace_version
	// points to right now — the one a rollback would be a no-op on, and
	// the one the console highlights as "live".
	Current bool `json:"current"`
	// IsDeployed marks the version inference_sessions.last_deployed_version
	// points to — i.e. what a real successful deploy actually built and
	// ran, as opposed to merely CreatedBy == agent (found live 2026-08-31:
	// the console's history list showed every agent-authored row as
	// "Deployed" and every customer row as "You", neither of which
	// actually answers "was this one deployed" — an agent row IS always
	// deploy-checkpointed, so that label happened to be right by
	// coincidence, but a customer's saved-and-never-deployed edit had no
	// way to be told apart from one that was later deployed).
	IsDeployed bool `json:"is_deployed"`
}

var (
	// ErrNoWorkspace means the session has no version saved yet — a build
	// that has not reached its first upload, not an error condition.
	ErrNoWorkspace = errors.New("no workspace saved for this session")
	// ErrVersionNotFound means a specific version number was requested
	// (read or rollback) that does not exist for this session.
	ErrVersionNotFound = errors.New("workspace version not found")
	// ErrInvalidSnapshot means the uploaded payload was refused: a path
	// that escapes the workspace root, or a size past the limits above.
	// Distinct from a storage failure so the handler can answer 400 rather
	// than 500 — this is the caller's payload being wrong, not us failing.
	ErrInvalidSnapshot = errors.New("invalid workspace snapshot")
	// ErrTooManyVersions means the session already holds
	// MaxWorkspaceVersions and a new save was refused rather than
	// silently pruning an old one out of history.
	ErrTooManyVersions = errors.New("this session has reached its workspace version limit")
)

// validateWorkspaceFiles checks size/count limits and sanitizes every
// path, mutating files in place (Path is normalized) and returning the
// total byte size. Shared by SaveVersion so the same rules apply
// regardless of whether the agent or the customer is saving.
func validateWorkspaceFiles(files []WorkspaceFile) (int64, error) {
	if len(files) > MaxWorkspaceFiles {
		return 0, fmt.Errorf("%w: %d files exceeds the %d-file limit", ErrInvalidSnapshot, len(files), MaxWorkspaceFiles)
	}
	var total int64
	for i := range files {
		clean, err := sanitizeWorkspacePath(files[i].Path)
		if err != nil {
			return 0, err
		}
		files[i].Path = clean

		if len(files[i].Content) > MaxWorkspaceFileBytes {
			return 0, fmt.Errorf("%w: %q is %d bytes, over the %d-byte per-file limit",
				ErrInvalidSnapshot, clean, len(files[i].Content), MaxWorkspaceFileBytes)
		}
		total += int64(len(files[i].Content))
	}
	if total > MaxWorkspaceTotalBytes {
		return 0, fmt.Errorf("%w: version is %d bytes, over the %d-byte total limit",
			ErrInvalidSnapshot, total, MaxWorkspaceTotalBytes)
	}
	return total, nil
}

// SaveVersion records a workspace save for a session — agent auto-save
// and a customer's explicit edit-and-save in the console IDE are treated
// identically: both reuse the CURRENT version's row in place (an UPDATE,
// not a new row) as long as that row is still a draft (is_checkpoint =
// false), and NEITHER checkpoints on save. A draft only becomes a new,
// permanent "Version history" entry when CheckpointCurrentVersion marks
// it so, at the session's next successful deploy.
//
// This used to special-case a customer save as always-insert,
// always-checkpointed-immediately — a deliberate edit earning its own
// history entry regardless of deploy state. Found live 2026-08-31: that
// meant History showed a new entry on every save whether or not it was
// ever deployed, and "current" frequently pointed at content that had
// never actually shipped — a customer asked for History to answer "was
// this deployed", not "was this saved", which requires treating both
// write paths the same way here.
//
// Found live 2026-08-26 (agent side, still true either way): the agent
// calls this once per file_editor step, and always inserting a new row
// turned an active build into dozens of near-duplicate entries in the
// customer-facing "Version history" — the in-place reuse is what fixed
// that, now shared by both callers.
//
// Either way the current-version pointer moves to whatever row this call
// touched, so a bad save is always something to roll back FROM, not lost
// data — an in-place draft update never overwrites a checkpointed version,
// only ever a still-uncommitted draft of the most recent work (agent or
// customer — created_by on a reused row updates to whoever wrote last).
//
// The version number (for a new row) is assigned as MAX(existing)+1
// inside the same transaction that inserts it and updates the pointer, so
// two concurrent saves for one session cannot race onto the same number.
func (s *Store) SaveVersion(ctx context.Context, sessionID uuid.UUID, files []WorkspaceFile, skipped []SkippedFile, createdBy CreatedBy) (int, error) {
	files = stripInternalScratchFiles(files)
	total, err := validateWorkspaceFiles(files)
	if err != nil {
		return 0, err
	}

	if files == nil {
		files = []WorkspaceFile{}
	}
	if skipped == nil {
		skipped = []SkippedFile{}
	}
	filesJSON, err := json.Marshal(files)
	if err != nil {
		return 0, fmt.Errorf("failed to encode workspace files: %w", err)
	}
	skippedJSON, err := json.Marshal(skipped)
	if err != nil {
		return 0, fmt.Errorf("failed to encode skipped files: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	// Agent and customer saves are now handled identically here: BOTH reuse
	// the current draft row in place while it is uncheckpointed, and
	// NEITHER checkpoints on save — only CheckpointCurrentVersion does
	// that, and only at a REAL successful deploy. This used to special-
	// case createdBy == CreatedByAgent, with a customer save checkpointed
	// immediately instead — meaning History showed a new entry on every
	// keystroke-adjacent save, whether or not it was ever deployed, and
	// the "current" version frequently pointed at something that had
	// never actually shipped. Found live 2026-08-31: a customer asked for
	// History to mean "was this deployed", not "was this saved" — this
	// unification is what makes that true for both write paths, not just
	// the agent's.
	var currentVersion sql.NullInt64
	var isCheckpoint sql.NullBool
	var currentFileCount sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT v.version, v.is_checkpoint, v.file_count
		FROM billing.inference_sessions s
		LEFT JOIN billing.kumbha_workspace_versions v
		    ON v.session_id = s.id AND v.version = s.current_workspace_version
		WHERE s.id = $1
	`, sessionID).Scan(&currentVersion, &isCheckpoint, &currentFileCount); err != nil {
		return 0, fmt.Errorf("failed to check current draft version: %w", err)
	}
	if currentVersion.Valid && !isCheckpoint.Bool {
		// Never let an empty save overwrite a draft that had real
		// content — run.py calls upload_workspace() unconditionally at
		// the end of EVERY run, including one that never touched a
		// file (e.g. a relaunch that just inspected an already-empty
		// PVC and did nothing else). Before this guard, that automatic
		// final save silently clobbered the only saved copy of a real,
		// already-deployed build with zero files — a live 2026-08-31
		// incident: the PVC-wipe bug emptied the filesystem, then this
		// path faithfully (and destructively) recorded that emptiness
		// over the actual site source, which by then existed nowhere
		// else. A customer explicitly clearing their own workspace is
		// not something this automatic background path does — that
		// would be a deliberate action of its own — so refusing an
		// empty overwrite here has no legitimate case to break.
		if len(files) == 0 && currentFileCount.Valid && currentFileCount.Int64 > 0 {
			if err := tx.Commit(); err != nil {
				return 0, fmt.Errorf("failed to commit workspace version: %w", err)
			}
			return int(currentVersion.Int64), nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE billing.kumbha_workspace_versions
			SET files = $1, skipped = $2, file_count = $3, byte_size = $4, created_by = $7, created_at = NOW()
			WHERE session_id = $5 AND version = $6
		`, filesJSON, skippedJSON, len(files), total, sessionID, currentVersion.Int64, string(createdBy)); err != nil {
			return 0, fmt.Errorf("failed to update draft workspace version: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("failed to commit workspace version: %w", err)
		}
		return int(currentVersion.Int64), nil
	}

	var existingVersions, nextVersion int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) FROM billing.kumbha_workspace_versions WHERE session_id = $1
	`, sessionID).Scan(&existingVersions); err != nil {
		return 0, fmt.Errorf("failed to determine next version: %w", err)
	}
	if existingVersions >= MaxWorkspaceVersions {
		return 0, ErrTooManyVersions
	}
	nextVersion = existingVersions + 1

	// A fresh draft (no current row yet, or the current one was already
	// checkpointed by a real deploy) is never itself a checkpoint — agent
	// or customer, it becomes visible in History only via
	// CheckpointCurrentVersion, i.e. only once something is actually
	// deployed. See this function's own top comment for why customer and
	// agent saves are no longer treated differently here.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing.kumbha_workspace_versions
		            (session_id, version, files, skipped, file_count, byte_size, created_by, is_checkpoint, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, false, NOW())
	`, sessionID, nextVersion, filesJSON, skippedJSON, len(files), total, string(createdBy)); err != nil {
		return 0, fmt.Errorf("failed to save workspace version: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE billing.inference_sessions SET current_workspace_version = $1 WHERE id = $2
	`, nextVersion, sessionID); err != nil {
		return 0, fmt.Errorf("failed to update current version pointer: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit workspace version: %w", err)
	}
	return nextVersion, nil
}

// CheckpointCurrentVersion marks the session's current draft version as a
// real, permanent checkpoint — called once, right after a deploy actually
// succeeds (see DeployKumbhaSession). Before this call the current version
// exists (a build has something to fetch, the console's Files tab has
// something to show) but does not appear in the customer-facing "Version
// history" list, and every further agent auto-save reuses it in place. On
// success, this call flips a used, visible history entry; a further agent
// auto-save starts a NEW draft on top of it instead of mutating it. A
// no-op (not an error) if there is nothing to checkpoint, or the current
// version is already checkpointed — a redeploy of unchanged content must
// not spuriously touch history.
//
// Also stamps last_deployed_version — deliberately unconditional, unlike
// the is_checkpoint flip above: a CUSTOMER save is checkpointed the
// instant it's saved (so it shows in History right away), which means
// v.is_checkpoint is often already true here even though nothing has
// actually been deployed yet. is_checkpoint answers "worth showing in
// History"; last_deployed_version answers "is this exact content what's
// currently running" — the Deploy button's own no-op guard needs the
// second question, not the first (found live 2026-08-31: a customer
// edited and saved, and Deploy immediately disabled itself with "already
// deployed" for a version that had never been deployed).
func (s *Store) CheckpointCurrentVersion(ctx context.Context, sessionID uuid.UUID) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE billing.kumbha_workspace_versions v
		SET is_checkpoint = true
		FROM billing.inference_sessions s
		WHERE v.session_id = s.id AND s.id = $1 AND v.version = s.current_workspace_version
		  AND v.is_checkpoint = false
	`, sessionID); err != nil {
		return fmt.Errorf("failed to checkpoint current workspace version: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE billing.inference_sessions
		SET last_deployed_version = current_workspace_version
		WHERE id = $1
	`, sessionID); err != nil {
		return fmt.Errorf("failed to record last deployed version: %w", err)
	}
	return nil
}

// GetCurrentVersion returns whatever version inference_sessions.
// current_workspace_version points to, scoped to the owning account.
func (s *Store) GetCurrentVersion(ctx context.Context, sessionID, accountID uuid.UUID) (*Snapshot, error) {
	var version sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT current_workspace_version FROM billing.inference_sessions
		WHERE id = $1 AND account_id = $2
	`, sessionID, accountID).Scan(&version); err != nil {
		return nil, ErrSessionNotFound
	}
	if !version.Valid {
		return nil, ErrNoWorkspace
	}
	return s.getVersion(ctx, sessionID, accountID, int(version.Int64))
}

// GetVersion returns one specific version, scoped to the owning account —
// used by the history list's "view this version" and by a rollback's own
// existence check.
func (s *Store) GetVersion(ctx context.Context, sessionID, accountID uuid.UUID, version int) (*Snapshot, error) {
	return s.getVersion(ctx, sessionID, accountID, version)
}

func (s *Store) getVersion(ctx context.Context, sessionID, accountID uuid.UUID, version int) (*Snapshot, error) {
	var filesJSON, skippedJSON []byte
	var snap Snapshot
	var createdBy string

	// COALESCE(..., false): last_deployed_version is NULL for every
	// session that predates migration 033 (and for one that's never had a
	// real deploy succeed yet) — v.version = NULL evaluates to SQL NULL,
	// not false, which a non-nullable Go bool cannot Scan. Treating
	// "we don't know if this was ever deployed" as false is also the
	// correct, conservative default: it leaves the Deploy button enabled
	// rather than incorrectly claiming something is already live.
	err := s.db.QueryRowContext(ctx, `
		SELECT v.version, v.files, v.skipped, v.file_count, v.byte_size, v.created_by, v.created_at, v.is_checkpoint,
		       COALESCE(v.version = s.last_deployed_version, false) AS is_deployed
		FROM billing.kumbha_workspace_versions v
		JOIN billing.inference_sessions s ON s.id = v.session_id
		WHERE v.session_id = $1 AND s.account_id = $2 AND v.version = $3
	`, sessionID, accountID, version).Scan(
		&snap.Version, &filesJSON, &skippedJSON, &snap.FileCount, &snap.ByteSize, &createdBy, &snap.CreatedAt, &snap.IsCheckpoint, &snap.IsDeployed)
	if err != nil {
		// "No such version" and "not your session" land here identically
		// (sql.ErrNoRows either way) — existence must not leak, same
		// convention as ErrSessionNotFound elsewhere in this package.
		return nil, ErrVersionNotFound
	}
	snap.CreatedBy = CreatedBy(createdBy)

	if err := json.Unmarshal(filesJSON, &snap.Files); err != nil {
		return nil, fmt.Errorf("failed to decode stored workspace: %w", err)
	}
	if err := json.Unmarshal(skippedJSON, &snap.Skipped); err != nil {
		return nil, fmt.Errorf("failed to decode skipped files: %w", err)
	}
	return &snap, nil
}

// ListVersions returns every CHECKPOINTED version's metadata (no file
// content), newest first, scoped to the owning account — the console's
// history list. An agent's in-progress draft (not yet checkpointed, see
// SaveVersion/CheckpointCurrentVersion) is deliberately excluded: it is
// not yet a meaningful rollback target, and showing it would reintroduce
// the "one entry per file write" clutter this split exists to remove.
func (s *Store) ListVersions(ctx context.Context, sessionID, accountID uuid.UUID) ([]VersionInfo, error) {
	// COALESCE(..., false) on is_deployed: see getVersion's own comment —
	// last_deployed_version is NULL for a session that predates migration
	// 033 or has never had a real deploy succeed, and a bare comparison
	// against NULL would fail this Scan rather than defaulting to false.
	rows, err := s.db.QueryContext(ctx, `
		SELECT v.version, v.file_count, v.byte_size, v.created_by, v.created_at,
		       v.version = s.current_workspace_version AS is_current,
		       COALESCE(v.version = s.last_deployed_version, false) AS is_deployed
		FROM billing.kumbha_workspace_versions v
		JOIN billing.inference_sessions s ON s.id = v.session_id
		WHERE v.session_id = $1 AND s.account_id = $2 AND v.is_checkpoint
		ORDER BY v.version DESC
	`, sessionID, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to list workspace versions: %w", err)
	}
	defer rows.Close()

	// Initialized empty, not nil: a session with no deploy yet now
	// legitimately has zero checkpointed versions (routine under the
	// is_checkpoint split, not an edge case) — a nil slice encodes to
	// JSON `null`, and the console's `versions.data?.versions.length`
	// only guards `data` being missing, not `data.versions` itself being
	// null, so this crashed the dialog with a real TypeError on exactly
	// that routine case. Found live 2026-08-26.
	versions := []VersionInfo{}
	for rows.Next() {
		var v VersionInfo
		var createdBy string
		if err := rows.Scan(&v.Version, &v.FileCount, &v.ByteSize, &createdBy, &v.CreatedAt, &v.Current, &v.IsDeployed); err != nil {
			return nil, fmt.Errorf("failed to scan workspace version: %w", err)
		}
		v.CreatedBy = CreatedBy(createdBy)
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

// SetCurrentVersion moves the current-version pointer — a rollback (to an
// older version) or a redo (back to a newer one after rolling back).
// Verifies the target version actually exists AND is checkpointed for
// this session before pointing to it (an agent's in-progress draft is
// never a legitimate rollback target — it isn't shown as one anywhere in
// the console, and this is the defense-in-depth check for that), and
// scopes by account the same way every other workspace read/write does.
func (s *Store) SetCurrentVersion(ctx context.Context, sessionID, accountID uuid.UUID, version int) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE billing.inference_sessions
		SET current_workspace_version = $1
		WHERE id = $2 AND account_id = $3
		  AND EXISTS (
		      SELECT 1 FROM billing.kumbha_workspace_versions
		      WHERE session_id = $2 AND version = $1 AND is_checkpoint
		  )
	`, version, sessionID, accountID)
	if err != nil {
		return fmt.Errorf("failed to update current version: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to confirm version update: %w", err)
	}
	if n == 0 {
		return ErrVersionNotFound
	}
	return nil
}

// WriteZip streams a snapshot as a ZIP archive. Generated on demand rather
// than stored: the files are already held individually for the console's
// file browser, and zipping a few hundred KB of text costs less than
// keeping a second copy in sync with it.
func (snap *Snapshot) WriteZip(w io.Writer) error {
	zw := zip.NewWriter(w)
	for _, f := range snap.Files {
		entry, err := zw.Create(f.Path)
		if err != nil {
			return fmt.Errorf("failed to add %q to archive: %w", f.Path, err)
		}
		if _, err := io.WriteString(entry, f.Content); err != nil {
			return fmt.Errorf("failed to write %q to archive: %w", f.Path, err)
		}
	}
	return zw.Close()
}

// sanitizeWorkspacePath rejects anything that would escape the archive
// root when extracted. This is the Zip Slip guard: a path like
// "../../.ssh/authorized_keys" inside an archive a customer unzips on
// their own machine is a real attack, and the agent's file list is
// model-influenced content, not trusted input — and now that saves also
// come from the console's editable IDE, this applies equally to a
// customer-supplied path, not only an agent-supplied one.
func sanitizeWorkspacePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: empty file path", ErrInvalidSnapshot)
	}
	// Normalise separators first: a Windows-style path would otherwise slip
	// past the checks below and land as a single flat filename.
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: %q is absolute", ErrInvalidSnapshot, p)
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q escapes the workspace root", ErrInvalidSnapshot, p)
	}
	return clean, nil
}
