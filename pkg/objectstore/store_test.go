// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package objectstore

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

func newMockStore(t *testing.T) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db), mock
}

func TestCreateBucket_Success(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID := uuid.New(), uuid.New()

	mock.ExpectQuery(`INSERT INTO storage\.buckets`).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at"}).
			AddRow(time.Now(), time.Now()))

	rec := &BucketRecord{AccountID: accountID, ProjectID: projectID, Name: "photos", Backend: "minio"}
	if err := store.CreateBucket(context.Background(), rec); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if rec.ID == uuid.Nil {
		t.Error("CreateBucket did not assign an ID")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestCreateBucket_DuplicateName proves a postgres unique_violation (23505)
// on (project_id, lower(name)) is surfaced as ErrBucketExists, not a raw
// SQL error — the API handler matches on this sentinel to return 409.
func TestCreateBucket_DuplicateName(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectQuery(`INSERT INTO storage\.buckets`).
		WillReturnError(&pq.Error{Code: "23505", Message: "duplicate key value violates unique constraint"})

	err := store.CreateBucket(context.Background(), &BucketRecord{
		AccountID: uuid.New(), ProjectID: uuid.New(), Name: "taken", Backend: "minio",
	})
	if err != ErrBucketExists {
		t.Fatalf("expected ErrBucketExists, got %v", err)
	}
}

// TestGetBucketByName_ScopesToTenant proves every lookup filters by
// account_id AND project_id, not name alone — the mechanism that keeps
// one tenant from ever resolving another tenant's bucket by guessing its
// name.
func TestGetBucketByName_ScopesToTenant(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .* FROM storage\.buckets\s+WHERE account_id = \$1 AND project_id = \$2`).
		WithArgs(accountID, projectID, "photos").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "name", "backend", "object_count", "total_bytes", "created_at", "updated_at",
		}).AddRow(uuid.New(), accountID, projectID, "photos", "minio", 0, 0, time.Now(), time.Now()))

	b, err := store.GetBucketByName(context.Background(), accountID, projectID, "photos")
	if err != nil {
		t.Fatalf("GetBucketByName: %v", err)
	}
	if b == nil || b.Name != "photos" {
		t.Fatalf("unexpected result: %+v", b)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestGetBucketByName_NotFoundReturnsNilNotError(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID := uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .* FROM storage\.buckets`).
		WithArgs(accountID, projectID, "ghost").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "name", "backend", "object_count", "total_bytes", "created_at", "updated_at",
		}))

	b, err := store.GetBucketByName(context.Background(), accountID, projectID, "ghost")
	if err != nil {
		t.Fatalf("GetBucketByName: %v", err)
	}
	if b != nil {
		t.Fatalf("expected nil for a missing bucket, got %+v", b)
	}
}

func TestPutObjectRecord_Create(t *testing.T) {
	store, mock := newMockStore(t)
	bucketID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, physical_key, backend, size_bytes FROM storage\.objects`).
		WithArgs(bucketID, "a.jpg").
		WillReturnRows(sqlmock.NewRows([]string{"id", "physical_key", "backend", "size_bytes"}))
	insertedAt := time.Now()
	mock.ExpectQuery(`INSERT INTO storage\.objects`).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at", "uploaded_at"}).
			AddRow(insertedAt, insertedAt, insertedAt))
	mock.ExpectExec(`UPDATE storage\.buckets\s+SET object_count = object_count \+ \$1`).
		WithArgs(1, int64(1024), bucketID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	rec := &ObjectRecord{
		BucketID: bucketID, AccountID: accountID, ProjectID: projectID,
		Key: "a.jpg", PhysicalKey: "t/x/y/z/1", SizeBytes: 1024,
		ContentType: "image/jpeg", Status: StatusAvailable, Backend: "minio",
	}
	oldKey, oldBackend, err := store.PutObjectRecord(context.Background(), rec)
	if err != nil {
		t.Fatalf("PutObjectRecord: %v", err)
	}
	if oldKey != "" || oldBackend != "" {
		t.Fatalf("a fresh key must not report a superseded object, got (%q, %q)", oldKey, oldBackend)
	}
	// Regression pin: PutObjectRecord used to ExecContext the insert
	// (discarding its own RETURNING-worthy columns), leaving these at Go's
	// zero value — confirmed live: the PUT response showed
	// "0001-01-01T00:00:00Z" while a follow-up list of the same object
	// showed the real timestamp.
	if rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() {
		t.Fatalf("CreatedAt/UpdatedAt were not populated from the insert: %+v", rec)
	}
	if rec.UploadedAt == nil || rec.UploadedAt.IsZero() {
		t.Fatalf("UploadedAt was not populated from the insert: %+v", rec)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestPutObjectRecord_Overwrite proves that writing to an existing key
// soft-deletes the old row (never mutates it in place — every physical
// object is a fresh UUID, see keys.go) and leaves the bucket's
// object_count unchanged, only adjusting total_bytes by the size delta.
func TestPutObjectRecord_Overwrite(t *testing.T) {
	store, mock := newMockStore(t)
	bucketID := uuid.New()
	oldID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, physical_key, backend, size_bytes FROM storage\.objects`).
		WithArgs(bucketID, "a.jpg").
		WillReturnRows(sqlmock.NewRows([]string{"id", "physical_key", "backend", "size_bytes"}).
			AddRow(oldID, "t/old/physical/key", "minio", int64(500)))
	mock.ExpectExec(`UPDATE storage\.objects SET status = \$1, deleted_at = NOW\(\) WHERE id = \$2`).
		WithArgs(StatusDeleted, oldID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO storage\.objects`).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at", "uploaded_at"}).
			AddRow(time.Now(), time.Now(), time.Now()))
	mock.ExpectExec(`UPDATE storage\.buckets\s+SET object_count = object_count \+ \$1, total_bytes = total_bytes \+ \$2`).
		WithArgs(0, int64(1524), bucketID). // 2024 - 500 = 1524
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	rec := &ObjectRecord{
		BucketID: bucketID, Key: "a.jpg", PhysicalKey: "t/new/physical/key",
		SizeBytes: 2024, Status: StatusAvailable, Backend: "minio",
	}
	oldKey, oldBackend, err := store.PutObjectRecord(context.Background(), rec)
	if err != nil {
		t.Fatalf("PutObjectRecord: %v", err)
	}
	if oldKey != "t/old/physical/key" || oldBackend != "minio" {
		t.Fatalf("expected the superseded object's physical key/backend back, got (%q, %q)", oldKey, oldBackend)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestDeleteBucket_NotEmpty(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID, bucketID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectExec(`UPDATE storage\.buckets SET deleted_at = NOW\(\)`).
		WithArgs(bucketID, accountID, projectID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT .* FROM storage\.buckets`).
		WithArgs(bucketID, accountID, projectID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "name", "backend", "object_count", "total_bytes", "created_at", "updated_at",
		}).AddRow(bucketID, accountID, projectID, "photos", "minio", 3, 3072, time.Now(), time.Now()))

	err := store.DeleteBucket(context.Background(), accountID, projectID, bucketID)
	if err != ErrBucketNotEmpty {
		t.Fatalf("expected ErrBucketNotEmpty, got %v", err)
	}
}

func TestDeleteObject_NotFound(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID, bucketID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, physical_key, backend, size_bytes FROM storage\.objects`).
		WithArgs(accountID, projectID, bucketID, "missing.jpg").
		WillReturnRows(sqlmock.NewRows([]string{"id", "physical_key", "backend", "size_bytes"}))

	_, _, err := store.DeleteObject(context.Background(), accountID, projectID, bucketID, "missing.jpg")
	if err != ErrObjectNotFound {
		t.Fatalf("expected ErrObjectNotFound, got %v", err)
	}
}

func TestValidBucketName(t *testing.T) {
	cases := map[string]bool{
		"photos":        true,
		"my-bucket.v2":  true,
		"ab":            false, // too short
		"UPPER":         false,
		"-leading-dash": false,
		"trailing-":     false,
	}
	for name, want := range cases {
		if got := ValidBucketName(name); got != want {
			t.Errorf("ValidBucketName(%q) = %v, want %v", name, got, want)
		}
	}
}
