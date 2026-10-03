// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"encoding/base64"
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/teepinbuild"
)

// maxDescribeImageBody bounds the request: an image up to teepinbuild.MaxImageBytes,
// base64-encoded (about 4/3 larger), plus a little for the JSON around it.
const maxDescribeImageBody = teepinbuild.MaxImageBytes*4/3 + 64<<10

type describeBuildImageRequest struct {
	// ImageBase64 is the image itself. It is sent as bytes, never as a URL for
	// the platform to fetch: a prompt-injected builder must not be able to aim the
	// control plane at an address of its choosing.
	ImageBase64 string `json:"image_base64"`
	// Request is what the customer asked for, so the reader knows what the
	// picture is for. Optional.
	Request string `json:"request"`
}

// DescribeBuildImage is POST /v1/build/sessions/:id/describe-image — the
// builder pod asks the platform's image reader to describe an image the customer
// attached, because the builder model cannot see images. Callable only with the
// session's own credential. The reader's tokens are billed to the build, so the
// build's budget and the account's credit apply as they do to any model call.
func (s *Server) DescribeBuildImage(c *gin.Context) {
	if s.teepinBuild == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Teepin Build is not available on this deployment"})
		return
	}
	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}
	callerSession, ok := auth.GetSessionID(c)
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "this endpoint requires a Teepin Build session credential"})
		return
	}
	if callerSession != sessionID {
		c.JSON(http.StatusForbidden, gin.H{"error": "this credential does not belong to that session"})
		return
	}
	_, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxDescribeImageBody)
	var req describeBuildImageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the request must be JSON with image_base64, and the image is limited to 8 MB", "code": "bad_request"})
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.ImageBase64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image_base64 is not valid base64", "code": "bad_image"})
		return
	}

	sess, err := s.teepinBuild.GetSession(c.Request.Context(), sessionID, accountID)
	if err != nil {
		if errors.Is(err, teepinbuild.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	description, err := s.teepinBuild.DescribeImage(c.Request.Context(), sess, data, req.Request)
	if err != nil {
		log.Printf("build: describe-image session=%s failed: %v", sessionID, err)
		switch {
		case errors.Is(err, teepinbuild.ErrBadImage):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "bad_image"})
		case errors.Is(err, teepinbuild.ErrNoImageReader):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no image reader is available right now", "code": "no_image_reader"})
		case errors.Is(err, teepinbuild.ErrSessionClosed):
			c.JSON(http.StatusConflict, gin.H{"error": "session is closed", "code": "session_closed"})
		case errors.Is(err, teepinbuild.ErrBudgetExhausted):
			c.JSON(http.StatusPaymentRequired, gin.H{"error": "session budget exhausted", "code": "budget_exhausted"})
		case errors.Is(err, billing.ErrInsufficientCredit):
			c.JSON(http.StatusPaymentRequired, gin.H{"error": "your account does not have enough credit", "code": "insufficient_credit"})
		case errors.Is(err, teepinbuild.ErrGateUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not verify your credit right now", "code": "billing_unavailable"})
		case errors.Is(err, inference.ErrProviderUnavailable), errors.Is(err, inference.ErrProviderRejected):
			c.JSON(http.StatusBadGateway, gin.H{"error": "the image reader could not serve this request", "code": "reader_failed"})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": "the image could not be described", "code": "reader_failed"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"description": description})
}
