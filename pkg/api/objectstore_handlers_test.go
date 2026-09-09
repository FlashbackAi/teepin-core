// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/objectstore"
)

// TestObjectModel_NeverLeaksPhysicalKey pins the fix for a real bug:
// ObjectRecord (and BucketRecord) have no JSON tags, so serializing them
// directly leaked PhysicalKey — the internal backend key the whole
// tenant-isolation design in pkg/objectstore/keys.go depends on never
// reaching a customer — straight into every object response. objectModel
// is what stands between the catalog record and the wire; this proves it
// actually excludes it, not just that it happens to today.
func TestObjectModel_NeverLeaksPhysicalKey(t *testing.T) {
	rec := &objectstore.ObjectRecord{
		ID:          uuid.New(),
		BucketID:    uuid.New(),
		AccountID:   uuid.New(),
		ProjectID:   uuid.New(),
		Key:         "a.jpg",
		PhysicalKey: "t/super-secret-internal-path/should-never-be-seen",
		SizeBytes:   1024,
		ContentType: "image/jpeg",
		Status:      objectstore.StatusAvailable,
		Backend:     "shelby",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	dto := objectModel(rec)
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if strings.Contains(string(raw), "super-secret-internal-path") {
		t.Fatalf("objectModel leaked the physical key into the response: %s", raw)
	}
	if strings.Contains(string(raw), "PhysicalKey") || strings.Contains(string(raw), "physical_key") {
		t.Fatalf("objectModel's JSON output mentions the physical key field at all: %s", raw)
	}
	// The customer's own fields must still be present.
	for _, want := range []string{`"key":"a.jpg"`, `"content_type":"image/jpeg"`, `"backend":"shelby"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("expected %s in output, got: %s", want, raw)
		}
	}
}

func TestObjectModel_UsesSnakeCaseJSONFields(t *testing.T) {
	rec := &objectstore.ObjectRecord{
		ID: uuid.New(), Key: "a.jpg", SizeBytes: 1024, Status: objectstore.StatusAvailable, Backend: "minio",
	}
	raw, err := json.Marshal(objectModel(rec))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Confirms this DTO round-trips in the same snake_case convention as
	// every other Teepin API response (see pkg/models.Instance).
	for _, want := range []string{`"id":`, `"key":`, `"size_bytes":`, `"status":`, `"backend":`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("expected snake_case field %s, got: %s", want, raw)
		}
	}
}

func TestBucketModel_UsesSnakeCaseJSONFields(t *testing.T) {
	rec := &objectstore.BucketRecord{
		ID: uuid.New(), Name: "photos", Backend: "shelby", ObjectCount: 3, TotalBytes: 4096,
	}
	raw, err := json.Marshal(bucketModel(rec))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"id":`, `"name":"photos"`, `"object_count":3`, `"total_bytes":4096`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("expected %s, got: %s", want, raw)
		}
	}
}
