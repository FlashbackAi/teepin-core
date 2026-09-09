// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package models

import "time"

// Bucket is a Teepin S3 bucket — a catalog construct only, never mapped
// 1:1 onto a backend-side bucket (see pkg/objectstore's own doc comment
// on why). AccountID/ProjectID are deliberately not exposed: the caller
// already knows its own scope, and echoing it back teaches nothing.
type Bucket struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Backend     string    `json:"backend"`
	ObjectCount int64     `json:"object_count"`
	TotalBytes  int64     `json:"total_bytes"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// StorageObject is one object in a Teepin S3 bucket.
//
// PhysicalKey is deliberately NEVER exposed here — pkg/objectstore's
// entire tenant-isolation design rests on a customer only ever
// addressing an object by its own Key, never the backend's physical key
// (see keys.go). BucketID/AccountID/ProjectID are also omitted: internal
// scoping the caller already knows from the URL/its own credential.
type StorageObject struct {
	ID             string            `json:"id"`
	Key            string            `json:"key"`
	SizeBytes      int64             `json:"size_bytes"`
	ContentType    string            `json:"content_type,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	ChecksumSHA256 string            `json:"checksum_sha256,omitempty"`
	Status         string            `json:"status"`
	Backend        string            `json:"backend"`
	UploadError    string            `json:"upload_error,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	UploadedAt     *time.Time        `json:"uploaded_at,omitempty"`
}
