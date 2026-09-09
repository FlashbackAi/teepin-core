// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/google/uuid"
)

// consistencyRetryInterval is how often GetObject re-polls a backend
// during its IndexingDelayHint window. Fixed rather than exponential:
// the window itself is short (single-digit seconds for Shelby), so a
// steady poll is simpler and no slower in practice than backoff would be.
const consistencyRetryInterval = 200 * time.Millisecond

// ErrObjectTooLarge is returned when a Put exceeds the configured
// per-object cap, checked before anything reaches the backend.
var ErrObjectTooLarge = errors.New("objectstore: object exceeds the configured size limit")

// Service is the tenant-scoped orchestration layer between the API and one
// primary Backend. There is deliberately no staging backend and no async
// commit step here: a Put call streams straight to the primary and does
// not return until the write has actually succeeded or failed against it.
// When the backend is slow or degraded, the caller sees that directly —
// never a silent write to somewhere else.
type Service struct {
	store          *Store
	backend        Backend
	maxObjectBytes int64
	sem            chan struct{}
}

// NewService builds a Service bound to one Backend. maxObjectBytes <= 0
// means no platform-wide size cap beyond whatever the backend itself
// enforces.
func NewService(store *Store, backend Backend, maxObjectBytes int64) *Service {
	caps := backend.Capabilities()
	n := caps.MaxConcurrentOps
	if n <= 0 {
		n = 1
	}
	return &Service{
		store:          store,
		backend:        backend,
		maxObjectBytes: maxObjectBytes,
		sem:            make(chan struct{}, n),
	}
}

// BackendName reports which backend this Service is bound to, for display
// (e.g. the console's health chip) — never persisted from here, since
// storage.buckets.backend is stamped once at bucket creation.
func (s *Service) BackendName() string { return s.backend.Name() }

// Health returns the currently active backend's derived health status —
// what the storage service tab's monitoring panel renders. Reads probe
// history only; it never itself performs a live probe (that's Prober's
// job, running independently), so this call is always fast regardless of
// how the backend itself is behaving right now.
func (s *Service) Health(ctx context.Context) (HealthStatus, error) {
	recent, err := s.store.RecentProbeResults(ctx, s.backend.Name(), 60)
	if err != nil {
		return HealthStatus{}, err
	}
	return DeriveHealthStatus(s.backend.Name(), recent), nil
}

func (s *Service) acquire(ctx context.Context) error {
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) release() { <-s.sem }

// CreateBucket creates a new catalog bucket bound to this Service's
// backend. The backend is stamped at creation and never changes — see
// storage.buckets.backend's own comment on why.
func (s *Service) CreateBucket(ctx context.Context, accountID, projectID uuid.UUID, name string) (*BucketRecord, error) {
	if !ValidBucketName(name) {
		return nil, fmt.Errorf("objectstore: invalid bucket name %q", name)
	}
	rec := &BucketRecord{
		AccountID: accountID,
		ProjectID: projectID,
		Name:      name,
		Backend:   s.backend.Name(),
	}
	if err := s.store.CreateBucket(ctx, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func (s *Service) ListBuckets(ctx context.Context, accountID, projectID uuid.UUID) ([]BucketRecord, error) {
	return s.store.ListBuckets(ctx, accountID, projectID)
}

// resolveBucket looks up a tenant's bucket by name. Returns
// ErrBucketNotFound rather than nil so every caller gets the same 404
// behaviour without re-checking for nil themselves.
func (s *Service) resolveBucket(ctx context.Context, accountID, projectID uuid.UUID, name string) (*BucketRecord, error) {
	b, err := s.store.GetBucketByName(ctx, accountID, projectID, name)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, ErrBucketNotFound
	}
	return b, nil
}

func (s *Service) GetBucket(ctx context.Context, accountID, projectID uuid.UUID, name string) (*BucketRecord, error) {
	return s.resolveBucket(ctx, accountID, projectID, name)
}

func (s *Service) DeleteBucket(ctx context.Context, accountID, projectID uuid.UUID, name string) error {
	b, err := s.resolveBucket(ctx, accountID, projectID, name)
	if err != nil {
		return err
	}
	return s.store.DeleteBucket(ctx, accountID, projectID, b.ID)
}

// PutObject streams r straight into the backend and, only once that write
// has actually succeeded, commits the result to the catalog. size must be
// known up front — every Backend implementation needs it (to decide
// single-shot vs. its own internal multipart), and gin already requires
// Content-Length for a non-chunked request body.
func (s *Service) PutObject(ctx context.Context, accountID, projectID uuid.UUID, bucketName, key string, r io.Reader, size int64, contentType string, metadata map[string]string) (*ObjectRecord, error) {
	if s.maxObjectBytes > 0 && size > s.maxObjectBytes {
		return nil, ErrObjectTooLarge
	}
	bucket, err := s.resolveBucket(ctx, accountID, projectID, bucketName)
	if err != nil {
		return nil, err
	}

	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()

	objectID := uuid.New()
	physKey := physicalKey(accountID, projectID, bucket.ID, objectID)

	hash := sha256.New()
	if _, err := s.backend.Put(ctx, physKey, io.TeeReader(r, hash), size, PutOptions{
		ContentType: contentType,
		Metadata:    metadata,
	}); err != nil {
		return nil, fmt.Errorf("objectstore: write object: %w", err)
	}

	rec := &ObjectRecord{
		ID:             objectID,
		BucketID:       bucket.ID,
		AccountID:      accountID,
		ProjectID:      projectID,
		Key:            key,
		PhysicalKey:    physKey,
		SizeBytes:      size,
		ContentType:    contentType,
		Metadata:       metadata,
		ChecksumSHA256: hex.EncodeToString(hash.Sum(nil)),
		Status:         StatusAvailable,
		Backend:        s.backend.Name(),
	}
	oldPhysicalKey, oldBackend, err := s.store.PutObjectRecord(ctx, rec)
	if err != nil {
		// The backend write already succeeded and cannot be undone
		// synchronously (Shelby in particular is known to sometimes
		// refuse a delete) — the reverse reconciliation sweep (Phase 5)
		// is what discovers and cleans up a blob left behind by a
		// catalog-side failure like this one.
		return nil, fmt.Errorf("objectstore: object written to backend but catalog commit failed (physical key %s): %w", physKey, err)
	}

	if oldPhysicalKey != "" && oldBackend == s.backend.Name() {
		// Best-effort cleanup of the object this Put overwrote. A
		// failure here does not fail the request — see DeleteObject's
		// own comment on why a backend delete is never load-bearing for
		// what the customer sees.
		if err := s.backend.Delete(ctx, oldPhysicalKey); err != nil {
			log.Printf("WARN: objectstore: failed to delete superseded object %s on backend %s: %v", oldPhysicalKey, oldBackend, err)
		}
	}

	return rec, nil
}

// GetObject returns a live stream of the object's bytes plus its catalog
// record. Content-Type and metadata on the returned record are ALWAYS
// from the catalog, never from the backend — see storage.objects'
// comment on why that is required even for a backend that does preserve
// them, so every read path behaves identically regardless of backend.
func (s *Service) GetObject(ctx context.Context, accountID, projectID uuid.UUID, bucketName, key string, rng *ByteRange) (io.ReadCloser, *ObjectRecord, error) {
	bucket, err := s.resolveBucket(ctx, accountID, projectID, bucketName)
	if err != nil {
		return nil, nil, err
	}
	obj, err := s.store.GetObject(ctx, accountID, projectID, bucket.ID, key)
	if err != nil {
		return nil, nil, err
	}
	if obj == nil {
		return nil, nil, ErrObjectNotFound
	}

	if err := s.acquire(ctx); err != nil {
		return nil, nil, err
	}
	defer s.release()

	// Service is the one caller position that already knows this object
	// SHOULD exist (the catalog row was just found above) — exactly the
	// hint Capabilities.IndexingDelayHint's own doc comment calls for. A
	// backend that just accepted this Put may still 404 or return its
	// ambiguous "transient" error for a short window afterward (confirmed
	// for Shelby in shelby-eval); MinIO's zero hint makes this a single
	// attempt with no added latency.
	body, err := s.getWithConsistencyRetry(ctx, obj.PhysicalKey, rng)
	if err != nil {
		return nil, nil, fmt.Errorf("objectstore: read object: %w", err)
	}
	return body, obj, nil
}

func (s *Service) getWithConsistencyRetry(ctx context.Context, physicalKey string, rng *ByteRange) (io.ReadCloser, error) {
	deadline := time.Now().Add(s.backend.Capabilities().IndexingDelayHint)
	for {
		body, _, err := s.backend.Get(ctx, physicalKey, rng)
		if err == nil {
			return body, nil
		}
		retryable := errors.Is(err, ErrTransient) || errors.Is(err, ErrNotFound)
		if !retryable || !time.Now().Before(deadline) {
			return nil, err
		}
		select {
		case <-time.After(consistencyRetryInterval):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// StatObject returns an object's catalog metadata without reading its
// bytes.
func (s *Service) StatObject(ctx context.Context, accountID, projectID uuid.UUID, bucketName, key string) (*ObjectRecord, error) {
	bucket, err := s.resolveBucket(ctx, accountID, projectID, bucketName)
	if err != nil {
		return nil, err
	}
	obj, err := s.store.GetObject(ctx, accountID, projectID, bucket.ID, key)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, ErrObjectNotFound
	}
	return obj, nil
}

func (s *Service) ListObjects(ctx context.Context, accountID, projectID uuid.UUID, bucketName, prefix, cursor string, limit int) ([]ObjectRecord, error) {
	bucket, err := s.resolveBucket(ctx, accountID, projectID, bucketName)
	if err != nil {
		return nil, err
	}
	return s.store.ListObjects(ctx, accountID, projectID, bucket.ID, prefix, cursor, limit)
}

// DeleteObject removes an object from the catalog and best-effort deletes
// it from the backend. The catalog delete is authoritative for what the
// customer sees — succeeding it does not wait on (or fail because of) the
// backend delete, since Shelby has been confirmed to sometimes refuse to
// delete a blob at all (InternalError: Failed to delete blob). A backend
// delete failure here is logged and left for the reconciler's orphan
// register, not surfaced to the caller as a request failure.
func (s *Service) DeleteObject(ctx context.Context, accountID, projectID uuid.UUID, bucketName, key string) error {
	bucket, err := s.resolveBucket(ctx, accountID, projectID, bucketName)
	if err != nil {
		return err
	}
	physKey, backendName, err := s.store.DeleteObject(ctx, accountID, projectID, bucket.ID, key)
	if err != nil {
		return err
	}

	if err := s.acquire(ctx); err != nil {
		return nil // catalog delete already succeeded; backend cleanup can wait
	}
	defer s.release()

	if backendName == s.backend.Name() {
		if err := s.backend.Delete(ctx, physKey); err != nil {
			log.Printf("WARN: objectstore: failed to delete %s on backend %s: %v", physKey, backendName, err)
		}
	}
	return nil
}
