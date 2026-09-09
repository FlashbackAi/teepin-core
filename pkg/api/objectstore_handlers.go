// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/objectstore"
)

const metaHeaderPrefix = "X-Teepin-Meta-"

// CreateBucket handles POST /v1/storage/buckets.
func (s *Server) CreateBucket(c *gin.Context) {
	if s.objectStore == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "object storage is not available on this deployment"})
		return
	}
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	var req struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	bucket, err := s.objectStore.CreateBucket(c.Request.Context(), accountID, projectID, req.Name)
	if err != nil {
		if errors.Is(err, objectstore.ErrBucketExists) {
			c.JSON(http.StatusConflict, gin.H{"error": "a bucket with this name already exists"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, bucket)
}

// ListBuckets handles GET /v1/storage/buckets.
func (s *Server) ListBuckets(c *gin.Context) {
	if s.objectStore == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "object storage is not available on this deployment"})
		return
	}
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	buckets, err := s.objectStore.ListBuckets(c.Request.Context(), accountID, projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"buckets": buckets})
}

// GetBucket handles GET /v1/storage/buckets/:bucket.
func (s *Server) GetBucket(c *gin.Context) {
	if s.objectStore == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "object storage is not available on this deployment"})
		return
	}
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	bucket, err := s.objectStore.GetBucket(c.Request.Context(), accountID, projectID, c.Param("bucket"))
	if err != nil {
		if errors.Is(err, objectstore.ErrBucketNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "bucket not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, bucket)
}

// DeleteBucket handles DELETE /v1/storage/buckets/:bucket.
func (s *Server) DeleteBucket(c *gin.Context) {
	if s.objectStore == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "object storage is not available on this deployment"})
		return
	}
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	err := s.objectStore.DeleteBucket(c.Request.Context(), accountID, projectID, c.Param("bucket"))
	switch {
	case err == nil:
		c.Status(http.StatusNoContent)
	case errors.Is(err, objectstore.ErrBucketNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "bucket not found"})
	case errors.Is(err, objectstore.ErrBucketNotEmpty):
		c.JSON(http.StatusConflict, gin.H{"error": "bucket is not empty"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// ListObjects handles GET /v1/storage/buckets/:bucket/objects?prefix=&cursor=&limit=.
func (s *Server) ListObjects(c *gin.Context) {
	if s.objectStore == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "object storage is not available on this deployment"})
		return
	}
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	limit, _ := strconv.Atoi(c.Query("limit"))
	objects, err := s.objectStore.ListObjects(c.Request.Context(), accountID, projectID,
		c.Param("bucket"), c.Query("prefix"), c.Query("cursor"), limit)
	if err != nil {
		if errors.Is(err, objectstore.ErrBucketNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "bucket not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"objects": objects})
}

// PutObject handles PUT /v1/storage/buckets/:bucket/object?key=... — the
// request body streams straight into the backend (see Service.PutObject's
// own doc comment: no staging, no async commit, the response only comes
// back once the write has actually succeeded or failed for real).
func (s *Server) PutObject(c *gin.Context) {
	if s.objectStore == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "object storage is not available on this deployment"})
		return
	}
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	key := c.Query("key")
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key query parameter is required"})
		return
	}
	if c.Request.ContentLength < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Content-Length is required"})
		return
	}

	contentType := c.GetHeader("Content-Type")
	metadata := map[string]string{}
	for name, values := range c.Request.Header {
		if strings.HasPrefix(name, metaHeaderPrefix) && len(values) > 0 {
			metadata[strings.ToLower(strings.TrimPrefix(name, metaHeaderPrefix))] = values[0]
		}
	}

	obj, err := s.objectStore.PutObject(c.Request.Context(), accountID, projectID,
		c.Param("bucket"), key, c.Request.Body, c.Request.ContentLength, contentType, metadata)
	if err != nil {
		switch {
		case errors.Is(err, objectstore.ErrBucketNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "bucket not found"})
		case errors.Is(err, objectstore.ErrObjectTooLarge):
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "object exceeds the maximum allowed size"})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to write object to storage backend: " + err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, obj)
}

// GetObjectMeta handles GET /v1/storage/buckets/:bucket/object?key=... —
// catalog metadata only, no bytes.
func (s *Server) GetObjectMeta(c *gin.Context) {
	if s.objectStore == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "object storage is not available on this deployment"})
		return
	}
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	key := c.Query("key")
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key query parameter is required"})
		return
	}

	obj, err := s.objectStore.StatObject(c.Request.Context(), accountID, projectID, c.Param("bucket"), key)
	if err != nil {
		if errors.Is(err, objectstore.ErrBucketNotFound) || errors.Is(err, objectstore.ErrObjectNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "object not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, obj)
}

// GetObjectContent handles GET /v1/storage/buckets/:bucket/object/content?key=...
// — streams the object's bytes, honoring a single-range Range header.
// Content-Type is always set from the catalog, never from the backend —
// see Service.GetObject's own comment on why that must hold for every
// backend, not just Shelby.
func (s *Server) GetObjectContent(c *gin.Context) {
	if s.objectStore == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "object storage is not available on this deployment"})
		return
	}
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	key := c.Query("key")
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key query parameter is required"})
		return
	}

	bucketName := c.Param("bucket")
	meta, err := s.objectStore.StatObject(c.Request.Context(), accountID, projectID, bucketName, key)
	if err != nil {
		if errors.Is(err, objectstore.ErrBucketNotFound) || errors.Is(err, objectstore.ErrObjectNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "object not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	rng, status := parseRange(c.GetHeader("Range"), meta.SizeBytes)

	body, _, err := s.objectStore.GetObject(c.Request.Context(), accountID, projectID, bucketName, key, rng)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read object from storage backend: " + err.Error()})
		return
	}
	defer body.Close()

	length := meta.SizeBytes
	extra := map[string]string{
		"Content-Disposition": `inline; filename="` + safeFilename(key) + `"`,
	}
	if rng != nil {
		length = rng.End - rng.Start + 1
		extra["Content-Range"] = "bytes " + strconv.FormatInt(rng.Start, 10) + "-" + strconv.FormatInt(rng.End, 10) + "/" + strconv.FormatInt(meta.SizeBytes, 10)
	}
	c.DataFromReader(status, length, contentTypeOrDefault(meta.ContentType), body, extra)
}

// DeleteObject handles DELETE /v1/storage/buckets/:bucket/object?key=...
func (s *Server) DeleteObject(c *gin.Context) {
	if s.objectStore == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "object storage is not available on this deployment"})
		return
	}
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	key := c.Query("key")
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key query parameter is required"})
		return
	}

	err := s.objectStore.DeleteObject(c.Request.Context(), accountID, projectID, c.Param("bucket"), key)
	switch {
	case err == nil:
		c.Status(http.StatusNoContent)
	case errors.Is(err, objectstore.ErrBucketNotFound), errors.Is(err, objectstore.ErrObjectNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "object not found"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// MintObjectDownloadURL handles POST
// /v1/storage/buckets/:bucket/object/download-url?key=&ttl_seconds= — our
// own stand-in for a presigned URL, since Shelby rejects presigned URLs
// outright (see pkg/objectstore/signer.go's doc comment).
func (s *Server) MintObjectDownloadURL(c *gin.Context) {
	if s.objectStore == nil || s.objectStoreSigner == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "signed download links are not available on this deployment"})
		return
	}
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	key := c.Query("key")
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key query parameter is required"})
		return
	}
	bucketName := c.Param("bucket")

	// Confirm the object actually exists and belongs to this tenant
	// before minting anything — a token for a nonexistent object is just
	// a needlessly working 404 that shows up later instead of now.
	if _, err := s.objectStore.StatObject(c.Request.Context(), accountID, projectID, bucketName, key); err != nil {
		if errors.Is(err, objectstore.ErrBucketNotFound) || errors.Is(err, objectstore.ErrObjectNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "object not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	ttl := objectstore.DefaultSignedURLTTL
	if raw := c.Query("ttl_seconds"); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ttl_seconds must be a positive integer"})
			return
		}
		ttl = time.Duration(secs) * time.Second
		if ttl > objectstore.MaxSignedURLTTL {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("ttl_seconds cannot exceed %d", int(objectstore.MaxSignedURLTTL.Seconds()))})
			return
		}
	}

	expiresAt := time.Now().Add(ttl)
	token := s.objectStoreSigner.Mint(objectstore.SignedDownload{
		AccountID: accountID, ProjectID: projectID, Bucket: bucketName, Key: key, ExpiresAt: expiresAt,
	})
	c.JSON(http.StatusOK, gin.H{
		"url":        "/v1/storage/d/" + token,
		"expires_at": expiresAt,
	})
}

// RedeemObjectDownloadURL handles GET /v1/storage/d/:token — mounted
// OUTSIDE authentication at the router (see main.go): the token itself is
// the credential, the same posture as the exec/Kumbha-event ticket
// handlers ("the WS handshake carries no Authorization header").
func (s *Server) RedeemObjectDownloadURL(c *gin.Context) {
	if s.objectStore == nil || s.objectStoreSigner == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	d, err := s.objectStoreSigner.Verify(c.Param("token"))
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid or expired link"})
		return
	}

	// Re-resolve against the LIVE catalog — a token can outlive a delete
	// or a bucket rename, and must never serve stale or wrong bytes.
	meta, err := s.objectStore.StatObject(c.Request.Context(), d.AccountID, d.ProjectID, d.Bucket, d.Key)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "object not found"})
		return
	}
	body, _, err := s.objectStore.GetObject(c.Request.Context(), d.AccountID, d.ProjectID, d.Bucket, d.Key, nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read object from storage backend: " + err.Error()})
		return
	}
	defer body.Close()

	c.DataFromReader(http.StatusOK, meta.SizeBytes, contentTypeOrDefault(meta.ContentType), body, map[string]string{
		"Content-Disposition": `inline; filename="` + safeFilename(d.Key) + `"`,
	})
}

// parseRange parses a single-range "bytes=start-end" Range header. Returns
// (nil, http.StatusOK) for anything absent or malformed — falling back to
// the whole object is always a valid response to an unparseable Range.
func parseRange(header string, size int64) (*objectstore.ByteRange, int) {
	if header == "" || size <= 0 {
		return nil, http.StatusOK
	}
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return nil, http.StatusOK
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return nil, http.StatusOK
	}
	start, err1 := strconv.ParseInt(parts[0], 10, 64)
	end, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start || end >= size {
		return nil, http.StatusOK
	}
	return &objectstore.ByteRange{Start: start, End: end}, http.StatusPartialContent
}

func contentTypeOrDefault(ct string) string {
	if ct == "" {
		return "application/octet-stream"
	}
	return ct
}

// safeFilename strips any path separators from a customer-supplied key so
// it cannot inject extra fields into the Content-Disposition header.
func safeFilename(key string) string {
	name := key
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	return strings.ReplaceAll(name, `"`, "")
}
