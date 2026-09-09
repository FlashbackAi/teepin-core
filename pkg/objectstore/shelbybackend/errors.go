// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package shelbybackend

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aws/smithy-go"

	"github.com/FlashbackAi/teepin-core/pkg/objectstore"
)

// classifyErr normalizes a Shelby error to objectstore's sentinels, from
// the exact codes/messages confirmed in shelby-eval/results/.
//
// Shelby returns the SAME "ServiceUnavailable" message for both "not yet
// indexed, retry" and "actually deleted" — this function genuinely cannot
// tell those two apart, so both classify as ErrTransient. That is the
// correct read at every call site in this package: Service (the only
// caller) only ever calls Get/Head on a physical key its own catalog
// already says should exist, so "transient, might still show up" is
// always the right assumption to hand back up — see
// Service.getWithConsistencyRetry.
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return err
	}

	switch apiErr.ErrorCode() {
	case "NoSuchKey", "NoSuchBucket", "NotFound":
		return fmt.Errorf("%w: %v", objectstore.ErrNotFound, err)
	case "ServiceUnavailable":
		return fmt.Errorf("%w: %v", objectstore.ErrTransient, err)
	case "InternalError":
		// Confirmed live: "An error occurred (InternalError) when calling
		// the DeleteObject operation ...: Failed to delete blob" — this
		// specific failure is NOT transient (retrying it did not help
		// during the eval), so it gets its own sentinel rather than
		// falling into ErrPermanent, letting a caller (the Phase 5
		// reconciler) treat it as "give up after N attempts" rather than
		// "retry like any other permanent error."
		if strings.Contains(apiErr.ErrorMessage(), "Failed to delete blob") {
			return fmt.Errorf("%w: %v", objectstore.ErrUndeletable, err)
		}
		return fmt.Errorf("%w: %v", objectstore.ErrPermanent, err)
	default:
		return fmt.Errorf("%w: %v", objectstore.ErrPermanent, err)
	}
}
