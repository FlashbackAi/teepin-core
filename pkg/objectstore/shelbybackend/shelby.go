// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package shelbybackend implements objectstore.Backend against Shelby, a
// third-party, Aptos-blockchain-coordinated, S3-compatible storage
// network — evaluated hands-on in shelby-eval/ (see results/report.html).
// Every capability value and error-classification rule below traces back
// to a specific confirmed finding there, not a guess:
//
//   - Single-request PUTs fail at a FIXED ~304s regardless of object size
//     (200MB/500MB/1GB all failed at the same elapsed time) — Put here
//     switches to Shelby's own multipart upload internally for anything
//     above MaxSinglePutBytes, since multipart was the one thing that
//     worked reliably in the eval.
//   - A failed upload can leave an undeletable orphaned blob
//     (InternalError: Failed to delete blob) — see errors.go.
//   - Content-Type and custom metadata are never preserved on read
//     (confirmed on 5/5 real JPEGs) — PreservesContentType/
//     PreservesUserMetadata are both false, so objectstore's catalog
//     (never this backend) is what a caller must trust for either.
//   - Presigned URLs are rejected outright ("Anonymous requests are not
//     allowed ... AWS Signature Version 4") — SupportsPresignedURLs is
//     false; objectstore's own signer.go is what stands in for this.
//   - Bucket addressing is by account address (66 chars, not a valid DNS
//     label) and ListBuckets always returns empty — path-style addressing
//     is mandatory, and SupportsBucketNamespaces is false.
//   - A 411 MissingContentLength was hit until request checksum
//     calculation was set to "when required" (boto3's
//     request_checksum_calculation="when_required") — the Go equivalent,
//     aws.RequestChecksumCalculationWhenRequired, is set below.
//   - Multipart UploadPart calls were rejected outright ("api error
//     NotImplemented: aws-chunked transfer encoding is not supported.
//     Use standard Content-Length for uploads.", HTTP 501) — confirmed
//     live on real uploads (42MB and 155MB, same failure both times).
//     Root cause: aws-sdk-go-v2's manager.Uploader keeps its OWN
//     RequestChecksumCalculation setting, entirely independent of the
//     *s3.Client's — it defaults to WhenSupported regardless of the
//     client, which attaches a CRC32 checksum to every part and forces
//     aws-chunked trailer encoding to send it. Fixed by setting the
//     SAME WhenRequired value on the Uploader itself (see NewBackend).
//     Worth reporting to Shelby regardless: this is standard, documented
//     AWS S3 behavior their compatibility layer doesn't implement, and
//     any other current-generation S3 client's multipart upload would
//     likely hit the same 501.
//
// These are seeded defaults, not a permanent truth: Shelby is an unstable
// prototype with no SLA and no ongoing monitoring exists to re-measure
// them automatically (a health probe was built and then removed after
// live testing showed it flagging Shelby's own eventual-consistency
// window as a false failure) — treat these as accurate as of 2026-09-09,
// not guaranteed to still hold later.
package shelbybackend

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/FlashbackAi/teepin-core/pkg/objectstore"
)

var capabilities = objectstore.Capabilities{
	MaxSinglePutBytes:        8 << 20, // 8 MiB — comfortably inside the ~304s fixed timeout at the eval's measured ~0.23 MB/s effective throughput
	MultipartPartSize:        8 << 20,
	MaxConcurrentOps:         6, // eval measured a 12.3% error rate already at concurrency 10
	PreservesContentType:     false,
	PreservesUserMetadata:    false,
	SupportsPresignedURLs:    false,
	ReadAfterWriteConsistent: false,
	IndexingDelayHint:        5 * time.Second,
	DeleteMissingIsError:     true,
	MaxListKeys:              1000, // Shelby REJECTS a larger request outright rather than clamping it
}

// Config configures access to Shelby's single shared account/namespace.
type Config struct {
	Endpoint string
	Region   string // defaults to "shelbyland", Shelby's own region name
	// APIKey is used as BOTH the access key and the secret key — Shelby's
	// own dual-purpose auth model, confirmed in shelby-eval.
	APIKey string
	// Bucket is Shelby's account address, not a real bucket name — see
	// this package's own doc comment.
	Bucket string
}

// Backend implements objectstore.Backend against Shelby's one
// account-scoped namespace.
type Backend struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
}

func NewBackend(ctx context.Context, cfg Config) (*Backend, error) {
	if cfg.Endpoint == "" || cfg.APIKey == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("shelbybackend: endpoint, api key and bucket (account address) are all required")
	}
	region := cfg.Region
	if region == "" {
		region = "shelbyland"
	}

	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.APIKey, cfg.APIKey, "")),
		// Shelby's own write pipeline already times out at a fixed ~304s;
		// letting the SDK ALSO retry on top of that would just make an
		// eventual failure take even longer to surface. Retry policy at
		// this layer is deliberately a no-op — Service's own semantics
		// (fail fast, tell the caller) are what should govern a slow or
		// failing write, not a hidden transport-level retry loop.
		config.WithRetryer(func() aws.Retryer { return aws.NopRetryer{} }),
	)
	if err != nil {
		return nil, fmt.Errorf("shelbybackend: load AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		// Shelby's "bucket" is a 66-char account address — not a valid DNS
		// label, so virtual-hosted-style addressing is not an option.
		o.UsePathStyle = true
		// Avoids the confirmed 411 MissingContentLength: only compute a
		// request checksum when the operation actually requires one,
		// matching boto3's request_checksum_calculation="when_required".
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})

	return &Backend{
		client: client,
		uploader: manager.NewUploader(client, func(u *manager.Uploader) {
			u.PartSize = capabilities.MultipartPartSize
			u.Concurrency = capabilities.MaxConcurrentOps
			// manager.Uploader keeps its OWN RequestChecksumCalculation,
			// entirely independent of the *s3.Client's setting above —
			// it defaults to WhenSupported regardless of what the client
			// is configured with. Left at that default, every part gets
			// a CRC32 checksum attached, which forces the SDK to send it
			// via aws-chunked trailer encoding — confirmed live against
			// Shelby to be rejected outright: "api error NotImplemented:
			// aws-chunked transfer encoding is not supported. Use
			// standard Content-Length for uploads." Matching it to the
			// client's own WhenRequired here is what actually avoids
			// that, since each part is already fully buffered
			// (known length) before UploadPart is called.
			u.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		}),
		bucket: cfg.Bucket,
	}, nil
}

func (b *Backend) Name() string                           { return "shelby" }
func (b *Backend) Capabilities() objectstore.Capabilities { return capabilities }

// Put writes the full object in one call regardless of size — see this
// package's doc comment on why anything above MaxSinglePutBytes MUST go
// through Shelby's own multipart upload rather than a single request.
func (b *Backend) Put(ctx context.Context, key string, r io.Reader, size int64, opts objectstore.PutOptions) (objectstore.PutResult, error) {
	if size <= capabilities.MaxSinglePutBytes {
		out, err := b.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(b.bucket),
			Key:           aws.String(key),
			Body:          r,
			ContentLength: aws.Int64(size),
			ContentType:   aws.String(opts.ContentType),
			Metadata:      opts.Metadata,
		})
		if err != nil {
			return objectstore.PutResult{}, classifyErr(err)
		}
		return objectstore.PutResult{ETag: aws.ToString(out.ETag)}, nil
	}

	out, err := b.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(b.bucket),
		Key:         aws.String(key),
		Body:        r,
		ContentType: aws.String(opts.ContentType),
		Metadata:    opts.Metadata,
	})
	if err != nil {
		return objectstore.PutResult{}, classifyErr(err)
	}
	return objectstore.PutResult{ETag: aws.ToString(out.ETag)}, nil
}

func (b *Backend) Get(ctx context.Context, key string, rng *objectstore.ByteRange) (io.ReadCloser, objectstore.ObjectInfo, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)}
	if rng != nil {
		in.Range = aws.String(fmt.Sprintf("bytes=%d-%d", rng.Start, rng.End))
	}
	out, err := b.client.GetObject(ctx, in)
	if err != nil {
		return nil, objectstore.ObjectInfo{}, classifyErr(err)
	}
	// ContentType/Metadata are read here for completeness only — the
	// caller (objectstore.Service) never trusts either from this backend
	// (PreservesContentType/PreservesUserMetadata are both false above),
	// so what Shelby actually returns for them is irrelevant in practice.
	return out.Body, objectstore.ObjectInfo{
		Size:         aws.ToInt64(out.ContentLength),
		ContentType:  aws.ToString(out.ContentType),
		Metadata:     out.Metadata,
		ETag:         aws.ToString(out.ETag),
		LastModified: aws.ToTime(out.LastModified),
	}, nil
}

func (b *Backend) Head(ctx context.Context, key string) (objectstore.ObjectInfo, error) {
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
	if err != nil {
		return objectstore.ObjectInfo{}, classifyErr(err)
	}
	return objectstore.ObjectInfo{
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         aws.ToString(out.ETag),
		LastModified: aws.ToTime(out.LastModified),
	}, nil
}

// Delete may return objectstore.ErrUndeletable — confirmed live behavior,
// not a hypothetical: a failed upload's blob can end up permanently
// refusing deletion (InternalError: Failed to delete blob). Callers must
// not treat that as something worth retrying forever (see errors.go and
// the Phase 5 reconciler's orphan-retry cap).
func (b *Backend) Delete(ctx context.Context, key string) error {
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
	if err != nil {
		return classifyErr(err)
	}
	return nil
}

// List enforces Shelby's confirmed hard cap by REJECTING an over-limit
// request rather than silently clamping it (contrast miniobackend, which
// clamps) — Shelby itself rejects rather than clamps, and a caller relying
// on List (the future reconciliation sweep) should see that distinction
// rather than have it silently hidden.
func (b *Backend) List(ctx context.Context, prefix, cursor string, limit int) (objectstore.ListPage, error) {
	if limit > capabilities.MaxListKeys {
		return objectstore.ListPage{}, fmt.Errorf("shelbybackend: requested limit %d exceeds Shelby's max of %d", limit, capabilities.MaxListKeys)
	}
	if limit <= 0 {
		limit = capabilities.MaxListKeys
	}
	in := &s3.ListObjectsV2Input{
		Bucket:  aws.String(b.bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(int32(limit)),
	}
	if cursor != "" {
		in.ContinuationToken = aws.String(cursor)
	}
	out, err := b.client.ListObjectsV2(ctx, in)
	if err != nil {
		return objectstore.ListPage{}, classifyErr(err)
	}
	page := objectstore.ListPage{NextCursor: aws.ToString(out.NextContinuationToken)}
	for _, o := range out.Contents {
		page.Keys = append(page.Keys, aws.ToString(o.Key))
	}
	return page, nil
}
