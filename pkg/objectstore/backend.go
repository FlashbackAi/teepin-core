// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package objectstore is Teepin's own S3-like object storage service: a
// customer-facing catalog (buckets/objects, tenant-scoped) backed by a
// pluggable Backend — MinIO today, Shelby (a third-party, blockchain-
// coordinated S3-compatible network evaluated in shelby-eval/) and AWS S3
// later. Swapping the backend is a config change (TEEPIN_OBJECTSTORE_BACKEND),
// never a change to the catalog, the API, or the console.
//
// The backend never provides tenant isolation on its own — Shelby in
// particular ties one API key to one flat account-scoped namespace, with no
// concept of "buckets" a customer could be isolated by. Isolation is
// enforced entirely in this package: every physical key is a server-derived
// UUID path (see keys.go) that never carries a byte of customer-supplied
// input, and every catalog query is scoped by (account_id, project_id).
package objectstore

import (
	"context"
	"errors"
	"io"
	"time"
)

// Sentinel errors a Backend implementation normalizes its own errors to.
// Shelby in particular returns the SAME ambiguous message for "not yet
// indexed" and "deleted" (confirmed in shelby-eval), so ErrTransient is
// only correct when the caller already knows (from the catalog) that the
// object should exist — a Backend cannot tell these apart unassisted.
var (
	ErrNotFound    = errors.New("objectstore: not found")
	ErrTransient   = errors.New("objectstore: transient backend error, retry")
	ErrPermanent   = errors.New("objectstore: permanent backend error")
	ErrUndeletable = errors.New("objectstore: backend refused to delete this object")
)

// Capabilities describes what a Backend can and cannot do, so its
// quirks are configuration the Service adapts to rather than special-cased
// code. Values below for Shelby come from the hands-on evaluation in
// shelby-eval/ (see results/report.html) and are a starting point, not a
// permanent truth — the health probe (Phase 5) re-measures these live once
// it's running, since Shelby is an unstable prototype with no SLA.
type Capabilities struct {
	// MaxSinglePutBytes is the largest object this backend will accept via
	// a single, non-multipart write. Put must switch to multipart itself
	// above this size — the caller always makes one Put call regardless of
	// object size, and never sees whether it single-shot or multiparted.
	MaxSinglePutBytes int64
	// MultipartPartSize is the part size Put uses once it switches to
	// multipart.
	MultipartPartSize int64
	// MaxConcurrentOps bounds how many simultaneous requests Service will
	// let in flight against this backend.
	MaxConcurrentOps int
	// PreservesContentType/PreservesUserMetadata: false means the catalog
	// (storage.objects.content_type / .metadata), not the backend, is the
	// only source of truth for these — read paths must never rely on the
	// backend returning them.
	PreservesContentType     bool
	PreservesUserMetadata    bool
	SupportsPresignedURLs    bool
	ReadAfterWriteConsistent bool
	// IndexingDelayHint is how long a Get/Head may need to retry after a
	// successful write before the object is actually readable.
	IndexingDelayHint time.Duration
	// DeleteMissingIsError: true means deleting an already-absent key
	// returns an error rather than succeeding idempotently, so callers
	// must not treat a delete error as proof the object still exists.
	DeleteMissingIsError bool
	// MaxListKeys is the largest page List will accept. Some backends
	// (Shelby) reject a larger request outright rather than clamping it.
	MaxListKeys int
}

// PutOptions carries the metadata a Backend.Put call should attempt to set,
// for backends that do PreservesContentType/PreservesUserMetadata. It is
// never the only place this data lives — the catalog always stores it too.
type PutOptions struct {
	ContentType string
	Metadata    map[string]string
}

// PutResult is what a successful Backend.Put reports back.
type PutResult struct {
	ETag string
}

// ObjectInfo is what a Backend can tell us about one object.
type ObjectInfo struct {
	Size         int64
	ContentType  string // only meaningful when Capabilities().PreservesContentType
	Metadata     map[string]string
	ETag         string
	LastModified time.Time
}

// ByteRange is an inclusive byte range for a ranged Get, matching HTTP
// Range semantics. Nil means "the whole object".
type ByteRange struct {
	Start, End int64
}

// ListPage is one page of a prefix listing.
type ListPage struct {
	Keys       []string
	NextCursor string // empty when there are no more pages
}

// Backend is a single physical storage container, bound to exactly one at
// construction (mirroring pkg/storage/s3.Client's own rule: "one bucket per
// Client keeps the surface honest"). A bucket/account is never a method
// parameter — for Shelby specifically, the "bucket" IS the account
// address, so letting one reach a call from anywhere but config is a bug
// class this interface makes unrepresentable.
//
// Implementations satisfy this interface structurally (no import of this
// package from the backend subpackages), the same relationship
// pkg/harbor/pkg/ecrregistry have to pkg/build.RegistryProvider.
type Backend interface {
	// Name identifies this backend for catalog rows and metrics, e.g.
	// "minio", "shelby", "s3".
	Name() string
	// Capabilities is static and cheap — read once at wiring time.
	Capabilities() Capabilities

	// Put writes the full object in one call — the caller (Service) never
	// stages or chunks anything itself. An implementation whose
	// Capabilities().MaxSinglePutBytes is smaller than size MUST switch to
	// its own multipart upload internally rather than attempt a single
	// request the backend is known to fail or time out on.
	Put(ctx context.Context, key string, r io.Reader, size int64, opts PutOptions) (PutResult, error)
	Get(ctx context.Context, key string, rng *ByteRange) (io.ReadCloser, ObjectInfo, error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix, cursor string, limit int) (ListPage, error)
}
