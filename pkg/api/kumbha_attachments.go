// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/objectstore"
)

// Kumbha attachments: images and files a customer attaches to a build
// prompt or a follow-up chat message. Built on the existing Teepin S3
// service (pkg/objectstore) rather than a separate storage path — a
// project's attachments are just objects in one well-known bucket, so this
// file adds no new storage code, only the upload/URL-minting shape Kumbha
// specifically needs.
//
// Two different attachment mechanisms exist because they have to (see
// KUMBHA-DESIGN.md's own note on why): an IMAGE is sent as real ImageContent
// inside the agent's message — the model sees it directly, which needs a
// URL an EXTERNAL LLM provider (Anthropic, Aptos) can fetch over the public
// internet. A PDF or other file has no equivalent "content block" in the
// agent SDK at all — it is materialized into the agent's own workspace
// directory instead, and the agent reads it itself with tools it already
// has (see run.py's own handling). Both start from the SAME upload
// endpoint below; only what happens to the resulting URL differs
// downstream.

// kumbhaAttachmentsBucket is one well-known bucket per project, auto-created
// on first use — a customer never sees or manages a bucket concept here,
// only "attach a file."
const kumbhaAttachmentsBucket = "kumbha-attachments"

// maxKumbhaAttachmentBytes bounds one attachment. Generous for a real
// screenshot or a short PDF, far below objectstore's own platform-wide cap
// (which exists for arbitrary customer storage, not chat attachments) —
// keeps a customer from turning the build composer into a bulk-upload tool.
const maxKumbhaAttachmentBytes = 25 << 20 // 25MiB

// kumbhaAttachmentURLTTL uses objectstore's own platform maximum: a
// signed link handed to an external LLM provider has to survive however
// long a queued or idle Kumbha session takes to actually process the
// message it's attached to (up to run.py's own 30-minute idle timeout,
// itself potentially preceded by a wait for the agent's current turn to
// finish) — a short TTL risks the link expiring before anything ever
// fetches it.
const kumbhaAttachmentURLTTL = objectstore.MaxSignedURLTTL

// CreateKumbhaAttachment handles POST
// /v1/kumbha/attachments?filename=<name> — the request body is the raw
// file bytes (mirrors PutObject's own shape), Content-Type from the
// header. Project-scoped, not session-scoped: the composer uploads before
// a session exists at all (the initial prompt), so there is no session id
// to key off yet — the resulting URL is just handed to whichever endpoint
// (CreateKumbhaSession or SendKumbhaMessage) uses it next.
func (s *Server) CreateKumbhaAttachment(c *gin.Context) {
	if s.objectStore == nil || s.objectStoreSigner == nil || s.publicBaseURL == "" {
		// All three genuinely are required: no object store means nowhere
		// to put the file, no signer means no way to hand out a URL, and
		// no public base URL means any URL minted would be unreachable
		// from outside Teepin's own network — see publicBaseURL's own
		// doc comment for why that's a hard requirement here specifically.
		c.JSON(http.StatusNotFound, gin.H{"error": "attachments are not available on this deployment"})
		return
	}
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	filename := c.Query("filename")
	if filename == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "filename query parameter is required"})
		return
	}
	if c.Request.ContentLength < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Content-Length is required"})
		return
	}
	if c.Request.ContentLength > maxKumbhaAttachmentBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "attachment exceeds the maximum allowed size"})
		return
	}
	contentType := c.GetHeader("Content-Type")

	if _, err := s.objectStore.CreateBucket(c.Request.Context(), accountID, projectID, kumbhaAttachmentsBucket); err != nil &&
		!errors.Is(err, objectstore.ErrBucketExists) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not prepare attachment storage: " + err.Error()})
		return
	}

	// A fresh UUID prefix, not the customer's filename alone: multiple
	// attachments across multiple sessions share this ONE bucket, and a
	// customer-chosen name is exactly the kind of caller-supplied string
	// object storage's own key-derivation design already refuses to let
	// anywhere near a physical key (see pkg/objectstore's own tenant-
	// isolation notes) — the sanitized original name still rides along
	// for a readable key, it just never collides or path-traverses on
	// its own.
	key := uuid.New().String() + "-" + sanitizeAttachmentFilename(filename)

	obj, err := s.objectStore.PutObject(c.Request.Context(), accountID, projectID,
		kumbhaAttachmentsBucket, key, c.Request.Body, c.Request.ContentLength, contentType, nil)
	if err != nil {
		switch {
		case errors.Is(err, objectstore.ErrObjectTooLarge):
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "attachment exceeds the maximum allowed size"})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to store attachment: " + err.Error()})
		}
		return
	}

	expiresAt := time.Now().Add(kumbhaAttachmentURLTTL)
	token := s.objectStoreSigner.Mint(objectstore.SignedDownload{
		AccountID: accountID, ProjectID: projectID, Bucket: kumbhaAttachmentsBucket, Key: key,
		ExpiresAt: expiresAt, Disposition: objectstore.DispositionInline,
	})

	c.JSON(http.StatusOK, gin.H{
		// Absolute, via publicBaseURL — this URL is fetched by whatever
		// LLM backend serves the customer's chosen tier, not by their own
		// browser, so a relative "/v1/storage/d/..." path (what the
		// generic MintObjectDownloadURL returns) would be meaningless to it.
		"url":          s.publicBaseURL + "/v1/storage/d/" + token,
		"content_type": obj.ContentType,
		"size_bytes":   obj.SizeBytes,
		"filename":     filename,
		"expires_at":   expiresAt,
	})
}

// sanitizeAttachmentFilename strips path separators and control characters
// from a customer-supplied filename before it becomes part of a storage
// key — defense in depth on top of the UUID prefix already making the key
// unique and unguessable, not the only thing standing between this and a
// traversal attempt.
func sanitizeAttachmentFilename(name string) string {
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	const maxLen = 200
	if len(name) > maxLen {
		name = name[len(name)-maxLen:]
	}
	return name
}
