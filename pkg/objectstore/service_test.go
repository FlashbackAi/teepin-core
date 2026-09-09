// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package objectstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// fakeBackend is an in-memory objectstore.Backend used to test Service's
// orchestration without any real network — the shape Phase 1's tests are
// meant to use per the plan.
type fakeBackend struct {
	name string
	caps Capabilities
	data map[string][]byte
	meta map[string]ObjectInfo

	putErr    error
	putCalled bool

	// getFailCount simulates a backend that is briefly not-yet-consistent
	// after a write (Shelby's confirmed eventual-consistency window):
	// Get fails with ErrNotFound this many times before succeeding.
	getFailCount int
	getAttempts  int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		name: "fake",
		caps: Capabilities{MaxConcurrentOps: 4, MaxSinglePutBytes: 1 << 30},
		data: map[string][]byte{},
		meta: map[string]ObjectInfo{},
	}
}

func (f *fakeBackend) Name() string               { return f.name }
func (f *fakeBackend) Capabilities() Capabilities { return f.caps }

func (f *fakeBackend) Put(ctx context.Context, key string, r io.Reader, size int64, opts PutOptions) (PutResult, error) {
	f.putCalled = true
	if f.putErr != nil {
		return PutResult{}, f.putErr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return PutResult{}, err
	}
	f.data[key] = b
	// Deliberately return a DIFFERENT content type than the caller
	// supplied, to prove Service never trusts a backend's own view of
	// metadata — see storage.objects' comment on why the catalog alone
	// is authoritative for every backend, not just Shelby.
	f.meta[key] = ObjectInfo{Size: size, ContentType: "backend/should-be-ignored"}
	return PutResult{ETag: "etag-" + key}, nil
}

func (f *fakeBackend) Get(ctx context.Context, key string, rng *ByteRange) (io.ReadCloser, ObjectInfo, error) {
	if f.getAttempts < f.getFailCount {
		f.getAttempts++
		return nil, ObjectInfo{}, ErrNotFound
	}
	b, ok := f.data[key]
	if !ok {
		return nil, ObjectInfo{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), f.meta[key], nil
}

func (f *fakeBackend) Head(ctx context.Context, key string) (ObjectInfo, error) {
	info, ok := f.meta[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return info, nil
}

func (f *fakeBackend) Delete(ctx context.Context, key string) error {
	if _, ok := f.data[key]; !ok {
		return ErrNotFound
	}
	delete(f.data, key)
	delete(f.meta, key)
	return nil
}

func (f *fakeBackend) List(ctx context.Context, prefix, cursor string, limit int) (ListPage, error) {
	var keys []string
	for k := range f.data {
		keys = append(keys, k)
	}
	return ListPage{Keys: keys}, nil
}

func newTestService(t *testing.T) (*Service, sqlmock.Sqlmock, *fakeBackend) {
	t.Helper()
	store, mock := newMockStore(t)
	backend := newFakeBackend()
	return NewService(store, backend, 0), mock, backend
}

func TestService_PutObject_WritesBackendThenCatalog(t *testing.T) {
	svc, mock, backend := newTestService(t)
	accountID, projectID, bucketID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT .* FROM storage\.buckets`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "name", "backend", "object_count", "total_bytes", "created_at", "updated_at",
		}).AddRow(bucketID, accountID, projectID, "photos", "fake", 0, 0, time.Now(), time.Now()))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, physical_key, backend, size_bytes FROM storage\.objects`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "physical_key", "backend", "size_bytes"}))
	mock.ExpectQuery(`INSERT INTO storage\.objects`).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at", "uploaded_at"}).
			AddRow(time.Now(), time.Now(), time.Now()))
	mock.ExpectExec(`UPDATE storage\.buckets\s+SET object_count`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	content := []byte("hello world")
	obj, err := svc.PutObject(context.Background(), accountID, projectID, "photos", "a.txt",
		bytes.NewReader(content), int64(len(content)), "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if !backend.putCalled {
		t.Fatal("expected the backend's Put to be called")
	}
	if obj.ContentType != "text/plain" {
		t.Fatalf("catalog record should keep the CALLER's content type, got %q", obj.ContentType)
	}
	// Regression pin: a PUT response used to hand back Go's zero-value
	// timestamps ("0001-01-01T00:00:00Z") since the insert discarded its
	// own RETURNING-worthy columns — confirmed live against a real object.
	if obj.CreatedAt.IsZero() || obj.UpdatedAt.IsZero() || obj.UploadedAt == nil {
		t.Fatalf("PutObject response has unpopulated timestamps: %+v", obj)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestService_PutObject_BackendFailure_NeverTouchesCatalog proves that a
// failed backend write never reaches the catalog at all — there is no
// "staging" or "failed" row created, matching the direct synchronous
// write path's contract (see service.go's own doc comment).
func TestService_PutObject_BackendFailure_NeverTouchesCatalog(t *testing.T) {
	svc, mock, backend := newTestService(t)
	accountID, projectID, bucketID := uuid.New(), uuid.New(), uuid.New()
	backend.putErr = errors.New("simulated backend outage")

	mock.ExpectQuery(`SELECT .* FROM storage\.buckets`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "name", "backend", "object_count", "total_bytes", "created_at", "updated_at",
		}).AddRow(bucketID, accountID, projectID, "photos", "fake", 0, 0, time.Now(), time.Now()))
	// No further SQL expectations set — PutObjectRecord must never be
	// called; ExpectationsWereMet below would fail if it were.

	_, err := svc.PutObject(context.Background(), accountID, projectID, "photos", "a.txt",
		bytes.NewReader([]byte("x")), 1, "text/plain", nil)
	if err == nil {
		t.Fatal("expected an error when the backend write fails")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestService_PutObject_TooLarge_NeverCallsBackendOrCatalog(t *testing.T) {
	store, mock := newMockStore(t)
	backend := newFakeBackend()
	svc := NewService(store, backend, 10) // 10-byte cap

	_, err := svc.PutObject(context.Background(), uuid.New(), uuid.New(), "photos", "a.txt",
		bytes.NewReader(make([]byte, 100)), 100, "text/plain", nil)
	if !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("expected ErrObjectTooLarge, got %v", err)
	}
	if backend.putCalled {
		t.Fatal("an oversized object must never reach the backend")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err) // no bucket lookup should even happen before the size check
	}
}

// TestService_GetObject_NeverTrustsBackendMetadata proves the catalog's
// ContentType survives even when the backend's own Get returns something
// different — the direct fix for the confirmed Shelby metadata-loss bug,
// verified here to hold for every backend, not just Shelby.
func TestService_GetObject_NeverTrustsBackendMetadata(t *testing.T) {
	svc, mock, backend := newTestService(t)
	accountID, projectID, bucketID, objectID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	physKey := physicalKey(accountID, projectID, bucketID, objectID)
	backend.data[physKey] = []byte("bytes")
	backend.meta[physKey] = ObjectInfo{ContentType: "backend/should-be-ignored"}

	mock.ExpectQuery(`SELECT .* FROM storage\.buckets`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "name", "backend", "object_count", "total_bytes", "created_at", "updated_at",
		}).AddRow(bucketID, accountID, projectID, "photos", "fake", 1, 5, time.Now(), time.Now()))
	mock.ExpectQuery(`SELECT .* FROM storage\.objects`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "bucket_id", "account_id", "project_id", "key", "physical_key", "size_bytes",
			"content_type", "metadata", "checksum_sha256", "status", "backend", "upload_error",
			"created_at", "updated_at", "uploaded_at",
		}).AddRow(objectID, bucketID, accountID, projectID, "a.jpg", physKey, int64(5),
			"image/jpeg", []byte("{}"), "", StatusAvailable, "fake", "",
			time.Now(), time.Now(), nil))

	body, obj, err := svc.GetObject(context.Background(), accountID, projectID, "photos", "a.jpg", nil)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer body.Close()

	if obj.ContentType != "image/jpeg" {
		t.Fatalf("expected the CATALOG's content type to win, got %q", obj.ContentType)
	}
}

// TestService_GetObject_RetriesWithinIndexingDelayHint proves GetObject
// rides out a backend's brief post-write inconsistency window (confirmed
// for Shelby: a Get can 404 for a few seconds right after a successful
// Put) rather than surfacing the first failure to the caller.
func TestService_GetObject_RetriesWithinIndexingDelayHint(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID, bucketID, objectID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	physKey := physicalKey(accountID, projectID, bucketID, objectID)

	backend := newFakeBackend()
	backend.caps.IndexingDelayHint = 2 * time.Second
	backend.getFailCount = 2 // fails twice, succeeds on the third attempt
	backend.data[physKey] = []byte("bytes")
	backend.meta[physKey] = ObjectInfo{}

	svc := NewService(store, backend, 0)

	mock.ExpectQuery(`SELECT .* FROM storage\.buckets`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "name", "backend", "object_count", "total_bytes", "created_at", "updated_at",
		}).AddRow(bucketID, accountID, projectID, "photos", "fake", 1, 5, time.Now(), time.Now()))
	mock.ExpectQuery(`SELECT .* FROM storage\.objects`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "bucket_id", "account_id", "project_id", "key", "physical_key", "size_bytes",
			"content_type", "metadata", "checksum_sha256", "status", "backend", "upload_error",
			"created_at", "updated_at", "uploaded_at",
		}).AddRow(objectID, bucketID, accountID, projectID, "a.jpg", physKey, int64(5),
			"image/jpeg", []byte("{}"), "", StatusAvailable, "fake", "",
			time.Now(), time.Now(), nil))

	body, _, err := svc.GetObject(context.Background(), accountID, projectID, "photos", "a.jpg", nil)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer body.Close()
	if backend.getAttempts != backend.getFailCount {
		t.Fatalf("expected exactly %d failed attempts before success, got %d", backend.getFailCount, backend.getAttempts)
	}
}

// TestService_GetObject_GivesUpAfterIndexingDelayHint proves the retry
// loop is bounded — a backend that never becomes consistent must not hang
// the request forever.
func TestService_GetObject_GivesUpAfterIndexingDelayHint(t *testing.T) {
	store, mock := newMockStore(t)
	accountID, projectID, bucketID, objectID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	physKey := physicalKey(accountID, projectID, bucketID, objectID)

	backend := newFakeBackend()
	backend.caps.IndexingDelayHint = 300 * time.Millisecond
	backend.getFailCount = 1000 // never succeeds within the window
	svc := NewService(store, backend, 0)

	mock.ExpectQuery(`SELECT .* FROM storage\.buckets`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "name", "backend", "object_count", "total_bytes", "created_at", "updated_at",
		}).AddRow(bucketID, accountID, projectID, "photos", "fake", 1, 5, time.Now(), time.Now()))
	mock.ExpectQuery(`SELECT .* FROM storage\.objects`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "bucket_id", "account_id", "project_id", "key", "physical_key", "size_bytes",
			"content_type", "metadata", "checksum_sha256", "status", "backend", "upload_error",
			"created_at", "updated_at", "uploaded_at",
		}).AddRow(objectID, bucketID, accountID, projectID, "a.jpg", physKey, int64(5),
			"image/jpeg", []byte("{}"), "", StatusAvailable, "fake", "",
			time.Now(), time.Now(), nil))

	start := time.Now()
	_, _, err := svc.GetObject(context.Background(), accountID, projectID, "photos", "a.jpg", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error once the indexing-delay window is exhausted")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("retry loop ran far longer than its IndexingDelayHint window: %v", elapsed)
	}
}

func TestService_ResolveBucket_CrossTenantIsNotFound(t *testing.T) {
	svc, mock, _ := newTestService(t)

	mock.ExpectQuery(`SELECT .* FROM storage\.buckets`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "name", "backend", "object_count", "total_bytes", "created_at", "updated_at",
		})) // no row: another tenant's bucket, or none at all — indistinguishable on purpose

	_, err := svc.GetBucket(context.Background(), uuid.New(), uuid.New(), "someone-elses-bucket")
	if !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("expected ErrBucketNotFound, got %v", err)
	}
}
