// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/inferencegateway"
)

// playgroundAccount is the fixed identity the operator playground dispatches
// under, so its traffic is throttled as one caller and never billed to (or
// starved by) a customer.
const playgroundAccount = "admin-playground"

// playgroundTimeout bounds one playground call end to end.
const playgroundTimeout = 12 * time.Minute

// completer is the slice of inferencegateway.Gateway the playground needs.
type completer interface {
	Complete(ctx context.Context, accountID string, req inference.Request) (*inference.Response, error)
}

// InferencePlaygroundHandler lets an operator send one prompt through the
// real gateway path (catalog lookup, live mounted-backend routing, tunnel
// dispatch) from Control Center — the way to verify a freshly mounted model
// works end to end before the public, API-key-authenticated endpoint exists.
// Mounted under /v1/admin behind the operator token.
type InferencePlaygroundHandler struct {
	gateway completer
}

// NewInferencePlaygroundHandler wires the gateway.
func NewInferencePlaygroundHandler(g completer) *InferencePlaygroundHandler {
	return &InferencePlaygroundHandler{gateway: g}
}

type playgroundRequest struct {
	ModelRoute string `json:"model_route" binding:"required"`
	Prompt     string `json:"prompt" binding:"required"`
	MaxTokens  int    `json:"max_tokens"`
}

type playgroundResponse struct {
	Content      string `json:"content"`
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	LatencyMs    int64  `json:"latency_ms"`
}

// Chat is POST /v1/admin/inference/chat.
func (h *InferencePlaygroundHandler) Chat(c *gin.Context) {
	var req playgroundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt is required"})
		return
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 || maxTokens > 4096 {
		maxTokens = 256
	}

	msg, _ := json.Marshal(map[string]string{"role": "user", "content": req.Prompt})

	ctx, cancel := context.WithTimeout(c.Request.Context(), playgroundTimeout)
	defer cancel()

	start := time.Now()
	resp, err := h.gateway.Complete(ctx, playgroundAccount, inference.Request{
		Model:     req.ModelRoute,
		Messages:  []json.RawMessage{msg},
		MaxTokens: maxTokens,
	})
	if err != nil {
		switch {
		case errors.Is(err, inferencegateway.ErrThrottled):
			c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
		case errors.Is(err, inference.ErrProviderUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, playgroundResponse{
		Content:      assistantText(resp.Body),
		Model:        resp.Model,
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		LatencyMs:    time.Since(start).Milliseconds(),
	})
}

// assistantText pulls choices[0].message.content out of an OpenAI-shaped
// body, falling back to the raw body so an unexpected shape is visible
// rather than silently empty.
func assistantText(body json.RawMessage) string {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Choices) == 0 {
		return string(body)
	}
	return parsed.Choices[0].Message.Content
}
