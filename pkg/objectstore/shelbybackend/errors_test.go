// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package shelbybackend

import (
	"errors"
	"testing"

	"github.com/aws/smithy-go"

	"github.com/FlashbackAi/teepin-core/pkg/objectstore"
)

func TestClassifyErr_ServiceUnavailableIsTransient(t *testing.T) {
	// The exact ambiguity confirmed in shelby-eval: Shelby returns this
	// SAME code for both "not yet indexed" and "actually deleted."
	err := classifyErr(&smithy.GenericAPIError{Code: "ServiceUnavailable", Message: "Shelby upstream returned 404. This can be transient..."})
	if !errors.Is(err, objectstore.ErrTransient) {
		t.Fatalf("expected ErrTransient, got %v", err)
	}
}

func TestClassifyErr_FailedToDeleteBlobIsUndeletable(t *testing.T) {
	// The exact confirmed live message.
	err := classifyErr(&smithy.GenericAPIError{
		Code:    "InternalError",
		Message: "An error occurred (InternalError) when calling the DeleteObject operation (reached max retries: 0): Failed to delete blob",
	})
	if !errors.Is(err, objectstore.ErrUndeletable) {
		t.Fatalf("expected ErrUndeletable, got %v", err)
	}
}

func TestClassifyErr_OtherInternalErrorIsPermanent(t *testing.T) {
	err := classifyErr(&smithy.GenericAPIError{Code: "InternalError", Message: "something else entirely"})
	if !errors.Is(err, objectstore.ErrPermanent) {
		t.Fatalf("expected ErrPermanent, got %v", err)
	}
}

func TestClassifyErr_NoSuchKeyIsNotFound(t *testing.T) {
	err := classifyErr(&smithy.GenericAPIError{Code: "NoSuchKey", Message: "the specified key does not exist"})
	if !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestClassifyErr_UnknownCodeIsPermanent(t *testing.T) {
	err := classifyErr(&smithy.GenericAPIError{Code: "SomethingNew", Message: "unrecognized"})
	if !errors.Is(err, objectstore.ErrPermanent) {
		t.Fatalf("expected an unrecognized code to default to ErrPermanent, got %v", err)
	}
}

func TestClassifyErr_NilIsNil(t *testing.T) {
	if err := classifyErr(nil); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestClassifyErr_NonAPIErrorPassesThrough(t *testing.T) {
	plain := errors.New("plain network error")
	if got := classifyErr(plain); got != plain {
		t.Fatalf("a non-API error should pass through unchanged, got %v", got)
	}
}
