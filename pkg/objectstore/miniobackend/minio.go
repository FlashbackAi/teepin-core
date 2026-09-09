// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package miniobackend implements objectstore.Backend against a
// self-hosted MinIO server (or anything else speaking the S3 API) using
// aws-sdk-go-v2 with path-style addressing and a custom endpoint — no
// separate MinIO SDK needed, since MinIO is itself S3-compatible.
//
// This backend satisfies objectstore.Backend structurally: it imports
// pkg/objectstore only for its value types (Capabilities, PutOptions,
// ...), never the Backend interface itself, matching the relationship
// pkg/harbor/pkg/ecrregistry have to pkg/build.RegistryProvider.
//
// MinIO is NOT wired underneath any other backend as hidden staging — it
// is a fully independent, separately-selectable primary backend
// (TEEPIN_OBJECTSTORE_BACKEND=minio), useful for local development and
// testing without touching Shelby's small shared sandbox at all, and as a
// real non-Shelby option.
package miniobackend

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/FlashbackAi/teepin-core/pkg/objectstore"
)

// capabilities reflects a locally-run, reliable S3-compatible server — no
// quirks confirmed against Shelby apply here. See backend.go's Capabilities
// doc comment.
var capabilities = objectstore.Capabilities{
	MaxSinglePutBytes:        64 << 20, // 64 MiB
	MultipartPartSize:        16 << 20, // 16 MiB
	MaxConcurrentOps:         64,
	PreservesContentType:     true,
	PreservesUserMetadata:    true,
	SupportsPresignedURLs:    true,
	ReadAfterWriteConsistent: true,
	DeleteMissingIsError:     false,
	MaxListKeys:              1000,
}

// Config configures a single MinIO/S3-compatible bucket.
type Config struct {
	Endpoint  string // e.g. "http://minio.storage.svc.cluster.local:9000"
	Region    string // MinIO ignores this in practice; kept for SDK compatibility
	AccessKey string
	SecretKey string
	Bucket    string
}

// Backend implements objectstore.Backend against one bucket on one
// MinIO/S3-compatible server — see that interface's own doc comment on
// why the bucket is bound at construction, not passed per call.
type Backend struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
}

func NewBackend(ctx context.Context, cfg Config) (*Backend, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("miniobackend: endpoint, bucket, access key and secret key are all required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("miniobackend: load AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		// A 66-char account-address-style bucket name (Shelby's shape;
		// kept consistent here too) cannot be a DNS label, and MinIO is
		// almost always reached by IP/hostname rather than a
		// bucket-subdomain anyway.
		o.UsePathStyle = true
	})

	return &Backend{
		client: client,
		uploader: manager.NewUploader(client, func(u *manager.Uploader) {
			u.PartSize = capabilities.MultipartPartSize
		}),
		bucket: cfg.Bucket,
	}, nil
}

func (b *Backend) Name() string                           { return "minio" }
func (b *Backend) Capabilities() objectstore.Capabilities { return capabilities }

func (b *Backend) Put(ctx context.Context, key string, r io.Reader, size int64, opts objectstore.PutOptions) (objectstore.PutResult, error) {
	// Below the single-put threshold, a plain PutObject avoids the
	// overhead of the multipart manager's own buffering; above it, the
	// manager handles the multipart upload transparently to the caller,
	// which never has to know which path was taken (see Backend.Put's
	// own doc comment on this contract).
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
	return out.Body, objectInfoFromGet(out), nil
}

func (b *Backend) Head(ctx context.Context, key string) (objectstore.ObjectInfo, error) {
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
	if err != nil {
		return objectstore.ObjectInfo{}, classifyErr(err)
	}
	return objectstore.ObjectInfo{
		Size:         aws.ToInt64(out.ContentLength),
		ContentType:  aws.ToString(out.ContentType),
		Metadata:     out.Metadata,
		ETag:         aws.ToString(out.ETag),
		LastModified: aws.ToTime(out.LastModified),
	}, nil
}

func (b *Backend) Delete(ctx context.Context, key string) error {
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
	if err != nil {
		return classifyErr(err)
	}
	return nil
}

func (b *Backend) List(ctx context.Context, prefix, cursor string, limit int) (objectstore.ListPage, error) {
	if limit <= 0 || limit > capabilities.MaxListKeys {
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

func objectInfoFromGet(out *s3.GetObjectOutput) objectstore.ObjectInfo {
	return objectstore.ObjectInfo{
		Size:         aws.ToInt64(out.ContentLength),
		ContentType:  aws.ToString(out.ContentType),
		Metadata:     out.Metadata,
		ETag:         aws.ToString(out.ETag),
		LastModified: aws.ToTime(out.LastModified),
	}
}

// classifyErr normalizes an AWS SDK error to this package's sentinels. A
// reliable S3-compatible server has no ambiguous-error problem the way
// Shelby does, so a straightforward not-found check is all that's needed
// here (contrast shelbybackend's own errors.go).
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	var nf *types.NoSuchKey
	var nb *types.NoSuchBucket
	if errors.As(err, &nf) || errors.As(err, &nb) {
		return fmt.Errorf("%w: %v", objectstore.ErrNotFound, err)
	}
	return err
}
