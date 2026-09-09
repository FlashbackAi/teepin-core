// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package objectstore

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPhysicalKey_ContainsNoCustomerInput(t *testing.T) {
	accountID, projectID, bucketID, objectID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	key := physicalKey(accountID, projectID, bucketID, objectID)

	if !strings.HasPrefix(key, "t/") {
		t.Fatalf("physical key %q does not start with the reserved t/ prefix", key)
	}
	for _, part := range []uuid.UUID{accountID, projectID, bucketID, objectID} {
		if !strings.Contains(key, part.String()) {
			t.Fatalf("physical key %q missing expected UUID component %s", key, part)
		}
	}
	// Every component is a UUID, so the key can never collide with the
	// reserved system prefix used by health probes.
	if strings.HasPrefix(key, systemPrefix) {
		t.Fatalf("physical key %q collides with the reserved system prefix", key)
	}
}

func TestPhysicalKey_FreshObjectIDOnEveryCall(t *testing.T) {
	accountID, projectID, bucketID := uuid.New(), uuid.New(), uuid.New()
	k1 := physicalKey(accountID, projectID, bucketID, uuid.New())
	k2 := physicalKey(accountID, projectID, bucketID, uuid.New())
	if k1 == k2 {
		t.Fatalf("two distinct object IDs produced the same physical key: %q", k1)
	}
}
