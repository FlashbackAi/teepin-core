// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package objectstore

import "github.com/google/uuid"

// systemPrefix is reserved for the platform's own use (health probes) and
// can never be produced by physicalKey, since it is not shaped like one —
// see keys_test.go.
const systemPrefix = "t/_system/"

// physicalKey derives the ONLY key shape this package ever writes to any
// backend: every component is a server-generated UUID, so no byte of a
// customer-supplied bucket/object name ever reaches a backend. This is the
// entire tenant-isolation mechanism for a backend (Shelby) that provides
// none of its own — see the package doc comment.
//
// A fresh objectID on every new object version (rather than reusing one
// across a delete-then-recreate at the same logical key) also sidesteps
// racing a backend's own eventually-consistent tombstone read.
func physicalKey(accountID, projectID, bucketID, objectID uuid.UUID) string {
	return "t/" + accountID.String() + "/" + projectID.String() + "/" + bucketID.String() + "/" + objectID.String()
}
