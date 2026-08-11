// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package s3 is a thin wrapper over the AWS S3 client for the platform's
// object storage needs — currently the immutable invoice-document bucket.
//
// It deliberately exposes only the two operations the platform performs:
// PutObject (write a rendered document once, at issue time) and
// PresignGetObject (mint a short-lived read URL for a customer download).
// There is no Delete: the bucket is configured no-delete by IAM, and
// leaving it out of the wrapper too means no code path can even attempt
// to remove a financial record.
//
// Credentials come from the AWS SDK's default chain. In production the
// ECS task role supplies temporary credentials automatically via the
// container credentials endpoint — no static access keys exist anywhere.
package s3

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Client wraps an S3 client bound to a single bucket. One bucket per
// Client keeps the surface honest: nothing here can address a bucket it
// was not constructed for.
type Client struct {
	s3      *s3.Client
	presign *s3.PresignClient
	bucket  string
}

// NewClient builds an S3 client for the given bucket using the SDK's
// default credential/region resolution. Region comes from the standard
// AWS_REGION / AWS_DEFAULT_REGION environment (set on the ECS task).
//
// Returns an error if AWS configuration cannot be loaded — the caller
// decides whether that is fatal. For the invoice feature it is not:
// issuing an invoice still succeeds without a stored PDF (local dev has
// no AWS), so main wires this up only when a bucket is configured and
// treats a construction failure as "PDF storage unavailable", not a
// crash.
func NewClient(ctx context.Context, bucket string) (*Client, error) {
	if bucket == "" {
		return nil, fmt.Errorf("s3: bucket name is required")
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("s3: load AWS config: %w", err)
	}
	api := s3.NewFromConfig(cfg)
	return &Client{
		s3:      api,
		presign: s3.NewPresignClient(api),
		bucket:  bucket,
	}, nil
}

// Bucket returns the bucket this client is bound to.
func (c *Client) Bucket() string { return c.bucket }

// PutObject writes body to key with the given content type and SSE-S3
// (AES256) server-side encryption. Called once per invoice at issue
// time; the bucket's versioning means an accidental re-write preserves
// the prior object as a version rather than destroying it.
func (c *Client) PutObject(ctx context.Context, key string, body []byte, contentType string) error {
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:               &c.bucket,
		Key:                  &key,
		Body:                 bytes.NewReader(body),
		ContentType:          &contentType,
		ServerSideEncryption: types.ServerSideEncryptionAes256,
	})
	if err != nil {
		return fmt.Errorf("s3: put %s: %w", key, err)
	}
	return nil
}

// PresignGetObject returns a URL that grants time-limited read access to
// key without any further credentials — the URL carries its own signed
// authorization in the query string. Used so a customer's browser can
// download an invoice directly while the bucket itself stays private
// (Block Public Access on). Keep ttl short; a download is immediate.
//
// The browser NAVIGATES to this URL (a link click), it does not fetch()
// it — a cross-origin fetch into S3 would be blocked by CORS, since S3
// sends no Access-Control-Allow-Origin. Navigation is not subject to
// CORS, which is exactly what presigned URLs are designed for.
//
// filename, when non-empty, is baked into a Content-Disposition:
// attachment response header (via ResponseContentDisposition, which S3
// honours from the presigned request). That makes the browser SAVE the
// file under that name rather than render the PDF inline in a tab.
func (c *Client) PresignGetObject(ctx context.Context, key, filename string, ttl time.Duration) (string, error) {
	in := &s3.GetObjectInput{
		Bucket: &c.bucket,
		Key:    &key,
	}
	if filename != "" {
		disp := fmt.Sprintf("attachment; filename=%q", filename)
		in.ResponseContentDisposition = &disp
	}
	req, err := c.presign.PresignGetObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("s3: presign %s: %w", key, err)
	}
	return req.URL, nil
}
