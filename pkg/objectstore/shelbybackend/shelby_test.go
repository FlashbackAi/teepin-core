// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package shelbybackend

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// TestNewBackend_UploaderMatchesClientChecksumSetting pins a confirmed
// live bug: manager.Uploader keeps its OWN RequestChecksumCalculation,
// entirely independent of the *s3.Client's setting — it defaults to
// WhenSupported regardless of what the client is configured with. Left
// at that default, every multipart part gets a CRC32 checksum attached,
// which forces aws-chunked trailer encoding — confirmed live against
// Shelby on real 42MB and 155MB uploads to be rejected outright ("api
// error NotImplemented: aws-chunked transfer encoding is not supported",
// HTTP 501). Setting the SAME WhenRequired value on both is what
// actually avoids it; this test exists so that value never silently
// drifts back to the uploader's own default.
func TestNewBackend_UploaderMatchesClientChecksumSetting(t *testing.T) {
	b, err := NewBackend(context.Background(), Config{
		Endpoint: "https://example.invalid",
		APIKey:   "test-key",
		Bucket:   "test-bucket",
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	if b.uploader.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired {
		t.Fatalf(
			"uploader.RequestChecksumCalculation = %v, want WhenRequired — "+
				"multipart uploads to Shelby will be rejected with a 501 (aws-chunked not supported) otherwise",
			b.uploader.RequestChecksumCalculation,
		)
	}
}
