// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package objectstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Object status machine. Phase 2 (a direct-write backend like MinIO) only
// ever produces "available" or nothing at all — a failed Backend.Put never
// reaches the catalog. The intermediate states exist for the staged async
// path a RequiresStagedIngest backend (Shelby) needs — see ingest.go.
const (
	StatusStaging   = "staging"
	StatusUploading = "uploading"
	StatusAvailable = "available"
	StatusFailed    = "failed"
	StatusDeleting  = "deleting"
	StatusDeleted   = "deleted"
	StatusMissing   = "missing"
	StatusOrphaned  = "orphaned"
)

var (
	ErrBucketNotFound = errors.New("objectstore: bucket not found")
	ErrBucketExists   = errors.New("objectstore: bucket name already in use")
	ErrBucketNotEmpty = errors.New("objectstore: bucket is not empty")
	ErrObjectNotFound = errors.New("objectstore: object not found")

	bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

// ValidBucketName reports whether name meets the naming rule enforced by
// storage.buckets' own CHECK constraint (kept here too so a bad name is
// rejected with a clear message before ever reaching the database).
func ValidBucketName(name string) bool {
	return bucketNameRe.MatchString(name)
}

// BucketRecord is a row of storage.buckets — a Teepin catalog construct
// only. It never maps 1:1 onto a backend-side bucket, even where the
// backend could support that (MinIO, S3) — this is what keeps backend
// swapping config-only and avoids S3's 1000-bucket-per-account cap.
type BucketRecord struct {
	ID          uuid.UUID
	AccountID   uuid.UUID
	ProjectID   uuid.UUID
	Name        string
	Backend     string // stamped at creation, never changes
	ObjectCount int64
	TotalBytes  int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ObjectRecord is a row of storage.objects. ContentType/Metadata are
// authoritative for every backend, not a Shelby-specific workaround — a
// read path must never trust a backend to hand these back.
type ObjectRecord struct {
	ID             uuid.UUID
	BucketID       uuid.UUID
	AccountID      uuid.UUID
	ProjectID      uuid.UUID
	Key            string
	PhysicalKey    string
	SizeBytes      int64
	ContentType    string
	Metadata       map[string]string
	ChecksumSHA256 string
	Status         string
	Backend        string
	UploadError    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	UploadedAt     *time.Time
}

// Store provides CRUD access to the storage.buckets / storage.objects
// catalog — the source of truth for what exists and who owns it, since no
// backend here (Shelby least of all) can be trusted to answer that itself.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// CreateBucket inserts a new bucket. Returns ErrBucketExists if the
// (project, lower(name)) pair is already in use by a live bucket.
func (s *Store) CreateBucket(ctx context.Context, rec *BucketRecord) error {
	if rec.ID == uuid.Nil {
		rec.ID = uuid.New()
	}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO storage.buckets (id, account_id, project_id, name, backend)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at, updated_at
	`, rec.ID, rec.AccountID, rec.ProjectID, rec.Name, rec.Backend).Scan(&rec.CreatedAt, &rec.UpdatedAt)
	if isUniqueViolation(err) {
		return ErrBucketExists
	}
	if err != nil {
		return fmt.Errorf("objectstore: create bucket %s: %w", rec.Name, err)
	}
	return nil
}

const bucketSelectColumns = `
	SELECT id, account_id, project_id, name, backend, object_count, total_bytes, created_at, updated_at
	FROM storage.buckets
`

// GetBucketByName returns a tenant's live bucket by name, or nil if none
// exists — never another tenant's, since account_id/project_id are always
// part of the WHERE clause, not a post-fetch check.
func (s *Store) GetBucketByName(ctx context.Context, accountID, projectID uuid.UUID, name string) (*BucketRecord, error) {
	row := s.db.QueryRowContext(ctx, bucketSelectColumns+`
		WHERE account_id = $1 AND project_id = $2 AND lower(name) = lower($3) AND deleted_at IS NULL
	`, accountID, projectID, name)
	return scanBucket(row)
}

// ListBuckets returns every live bucket a tenant owns.
func (s *Store) ListBuckets(ctx context.Context, accountID, projectID uuid.UUID) ([]BucketRecord, error) {
	rows, err := s.db.QueryContext(ctx, bucketSelectColumns+`
		WHERE account_id = $1 AND project_id = $2 AND deleted_at IS NULL
		ORDER BY created_at DESC
	`, accountID, projectID)
	if err != nil {
		return nil, fmt.Errorf("objectstore: list buckets: %w", err)
	}
	defer rows.Close()

	var out []BucketRecord
	for rows.Next() {
		var b BucketRecord
		if err := rows.Scan(&b.ID, &b.AccountID, &b.ProjectID, &b.Name, &b.Backend,
			&b.ObjectCount, &b.TotalBytes, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("objectstore: scan bucket: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteBucket soft-deletes an empty bucket. Returns ErrBucketNotFound or
// ErrBucketNotEmpty as appropriate.
func (s *Store) DeleteBucket(ctx context.Context, accountID, projectID, bucketID uuid.UUID) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE storage.buckets SET deleted_at = NOW()
		WHERE id = $1 AND account_id = $2 AND project_id = $3
		  AND deleted_at IS NULL AND object_count = 0
	`, bucketID, accountID, projectID)
	if err != nil {
		return fmt.Errorf("objectstore: delete bucket %s: %w", bucketID, err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return nil
	}

	b, err := s.getBucketByID(ctx, accountID, projectID, bucketID)
	if err != nil {
		return err
	}
	if b == nil {
		return ErrBucketNotFound
	}
	return ErrBucketNotEmpty
}

func (s *Store) getBucketByID(ctx context.Context, accountID, projectID, bucketID uuid.UUID) (*BucketRecord, error) {
	row := s.db.QueryRowContext(ctx, bucketSelectColumns+`
		WHERE id = $1 AND account_id = $2 AND project_id = $3 AND deleted_at IS NULL
	`, bucketID, accountID, projectID)
	return scanBucket(row)
}

func scanBucket(row *sql.Row) (*BucketRecord, error) {
	var b BucketRecord
	err := row.Scan(&b.ID, &b.AccountID, &b.ProjectID, &b.Name, &b.Backend,
		&b.ObjectCount, &b.TotalBytes, &b.CreatedAt, &b.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("objectstore: scan bucket: %w", err)
	}
	return &b, nil
}

const objectSelectColumns = `
	SELECT id, bucket_id, account_id, project_id, key, physical_key, size_bytes,
	       COALESCE(content_type, ''), metadata, COALESCE(checksum_sha256, ''),
	       status, backend, COALESCE(upload_error, ''), created_at, updated_at, uploaded_at
	FROM storage.objects
`

// PutObjectRecord commits a successful backend write to the catalog. If an
// object already lives at this key, it is soft-deleted first (the
// overwrite is a new physical object at a fresh key — see keys.go — never
// a mutation in place) and its physical key/backend are returned so the
// caller can best-effort delete the now-orphaned old blob from the
// backend it actually lived on. The bucket's object_count only changes on
// a genuine create, not an overwrite.
func (s *Store) PutObjectRecord(ctx context.Context, rec *ObjectRecord) (oldPhysicalKey, oldBackend string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("objectstore: begin tx: %w", err)
	}
	defer tx.Rollback()

	var oldID uuid.UUID
	var oldSize int64
	row := tx.QueryRowContext(ctx, `
		SELECT id, physical_key, backend, size_bytes FROM storage.objects
		WHERE bucket_id = $1 AND key = $2 AND deleted_at IS NULL
		FOR UPDATE
	`, rec.BucketID, rec.Key)
	scanErr := row.Scan(&oldID, &oldPhysicalKey, &oldBackend, &oldSize)
	isOverwrite := scanErr == nil
	if scanErr != nil && scanErr != sql.ErrNoRows {
		return "", "", fmt.Errorf("objectstore: check existing object: %w", scanErr)
	}

	if isOverwrite {
		if _, err := tx.ExecContext(ctx, `
			UPDATE storage.objects SET status = $1, deleted_at = NOW() WHERE id = $2
		`, StatusDeleted, oldID); err != nil {
			return "", "", fmt.Errorf("objectstore: soft-delete replaced object: %w", err)
		}
	}

	if rec.ID == uuid.Nil {
		rec.ID = uuid.New()
	}
	// QueryRowContext + RETURNING, not ExecContext: created_at/updated_at
	// (DEFAULT NOW()) and uploaded_at (the literal NOW() below) are all
	// database-generated, and ExecContext discards them — the caller
	// (Service.PutObject, and the API response it feeds) would otherwise
	// hand back a record with these still at Go's zero value ("0001-01-01
	// T00:00:00Z"), confirmed live: the immediate PUT response showed
	// zeroed timestamps while a follow-up list of the same object showed
	// the real ones, since ListObjects re-reads the row from the DB.
	var uploadedAt time.Time
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO storage.objects
		(id, bucket_id, account_id, project_id, key, physical_key, size_bytes,
		 content_type, metadata, checksum_sha256, status, backend, uploaded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW())
		RETURNING created_at, updated_at, uploaded_at
	`, rec.ID, rec.BucketID, rec.AccountID, rec.ProjectID, rec.Key, rec.PhysicalKey, rec.SizeBytes,
		nullIfEmpty(rec.ContentType), jsonMetadata(rec.Metadata), nullIfEmpty(rec.ChecksumSHA256),
		rec.Status, rec.Backend,
	).Scan(&rec.CreatedAt, &rec.UpdatedAt, &uploadedAt); err != nil {
		return "", "", fmt.Errorf("objectstore: insert object: %w", err)
	}
	rec.UploadedAt = &uploadedAt

	countDelta := 1
	if isOverwrite {
		countDelta = 0
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE storage.buckets
		SET object_count = object_count + $1, total_bytes = total_bytes + $2, updated_at = NOW()
		WHERE id = $3
	`, countDelta, rec.SizeBytes-oldSize, rec.BucketID); err != nil {
		return "", "", fmt.Errorf("objectstore: update bucket counters: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("objectstore: commit put: %w", err)
	}
	if !isOverwrite {
		return "", "", nil
	}
	return oldPhysicalKey, oldBackend, nil
}

// GetObject returns a tenant's live object by key, or nil if none exists.
func (s *Store) GetObject(ctx context.Context, accountID, projectID, bucketID uuid.UUID, key string) (*ObjectRecord, error) {
	row := s.db.QueryRowContext(ctx, objectSelectColumns+`
		WHERE account_id = $1 AND project_id = $2 AND bucket_id = $3 AND key = $4 AND deleted_at IS NULL
	`, accountID, projectID, bucketID, key)
	return scanObject(row)
}

// ListObjects returns one page of a tenant's live objects under prefix,
// ordered by key so cursor is just "the last key seen" — cheap and stable
// without needing an offset.
func (s *Store) ListObjects(ctx context.Context, accountID, projectID, bucketID uuid.UUID, prefix, cursor string, limit int) ([]ObjectRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, objectSelectColumns+`
		WHERE account_id = $1 AND project_id = $2 AND bucket_id = $3 AND deleted_at IS NULL
		  AND key LIKE $4 AND key > $5
		ORDER BY key ASC
		LIMIT $6
	`, accountID, projectID, bucketID, likePrefix(prefix), cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("objectstore: list objects: %w", err)
	}
	defer rows.Close()

	var out []ObjectRecord
	for rows.Next() {
		o, err := scanObjectRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// DeleteObject soft-deletes a tenant's object and returns its physical key
// + backend so the caller can issue the actual backend delete. Returns
// ErrObjectNotFound if no live object exists at that key.
func (s *Store) DeleteObject(ctx context.Context, accountID, projectID, bucketID uuid.UUID, key string) (physicalKey, backend string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("objectstore: begin tx: %w", err)
	}
	defer tx.Rollback()

	var id uuid.UUID
	var size int64
	row := tx.QueryRowContext(ctx, `
		SELECT id, physical_key, backend, size_bytes FROM storage.objects
		WHERE account_id = $1 AND project_id = $2 AND bucket_id = $3 AND key = $4 AND deleted_at IS NULL
		FOR UPDATE
	`, accountID, projectID, bucketID, key)
	if err := row.Scan(&id, &physicalKey, &backend, &size); err != nil {
		if err == sql.ErrNoRows {
			return "", "", ErrObjectNotFound
		}
		return "", "", fmt.Errorf("objectstore: find object to delete: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE storage.objects SET status = $1, deleted_at = NOW() WHERE id = $2
	`, StatusDeleted, id); err != nil {
		return "", "", fmt.Errorf("objectstore: soft-delete object: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE storage.buckets SET object_count = object_count - 1, total_bytes = total_bytes - $1, updated_at = NOW()
		WHERE id = $2
	`, size, bucketID); err != nil {
		return "", "", fmt.Errorf("objectstore: update bucket counters: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("objectstore: commit delete: %w", err)
	}
	return physicalKey, backend, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanObject(row rowScanner) (*ObjectRecord, error) {
	o, err := scanObjectRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return o, err
}

func scanObjectRow(row rowScanner) (*ObjectRecord, error) {
	var o ObjectRecord
	var metadataJSON []byte
	err := row.Scan(&o.ID, &o.BucketID, &o.AccountID, &o.ProjectID, &o.Key, &o.PhysicalKey, &o.SizeBytes,
		&o.ContentType, &metadataJSON, &o.ChecksumSHA256, &o.Status, &o.Backend, &o.UploadError,
		&o.CreatedAt, &o.UpdatedAt, &o.UploadedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("objectstore: scan object: %w", err)
	}
	o.Metadata = parseMetadata(metadataJSON)
	return &o, nil
}

func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func likePrefix(prefix string) string {
	escaped := regexp.MustCompile(`([%_\\])`).ReplaceAllString(prefix, `\$1`)
	return escaped + "%"
}

func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// jsonMetadata/parseMetadata round-trip storage.objects.metadata (JSONB).
// A nil map marshals to "{}", never SQL NULL — the column is NOT NULL
// DEFAULT '{}', and every read path treats "no metadata" as an empty map,
// not a case to nil-check separately.
func jsonMetadata(m map[string]string) []byte {
	if m == nil {
		m = map[string]string{}
	}
	b, _ := json.Marshal(m)
	return b
}

func parseMetadata(raw []byte) map[string]string {
	m := map[string]string{}
	if len(raw) == 0 {
		return m
	}
	_ = json.Unmarshal(raw, &m)
	return m
}
