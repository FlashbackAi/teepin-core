// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/inferencegateway"
	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
)

// Permissions an API key carries for Teepin Inference.
//
//	inference:invoke         required for a key to call models at all
//	inference:model:<route>  optional, repeatable: restricts the key to the
//	                         listed models. With none present the key may call
//	                         every model the account can see.
//
// A signed-in user (JWT) is not subject to scopes; they act with their role.
const (
	ScopeInferenceInvoke = "inference:invoke"
	scopeInferenceModel  = "inference:model:"
)

const (
	// maxInferenceBodyBytes bounds one request body. Generous for long
	// prompts, but a hard stop against a client streaming gigabytes at us.
	maxInferenceBodyBytes = 8 << 20
	// settleTimeout bounds recording usage after a request ends.
	settleTimeout = 15 * time.Second
)

// inferenceGateway is the slice of inferencegateway.Gateway this handler uses.
type inferenceGateway interface {
	Complete(ctx context.Context, accountID string, req inference.Request) (*inference.Response, error)
	Stream(ctx context.Context, accountID string, req inference.Request, onChunk func(inference.Chunk) error) error
}

// catalogReader is the slice of modelcatalog.Service this handler uses.
type catalogReader interface {
	GetModel(ctx context.Context, modelRoute string) (*modelcatalog.Model, error)
	ListModels(ctx context.Context) ([]modelcatalog.Model, error)
}

// usageRecorder is the slice of billing.Service used to meter tokens.
type usageRecorder interface {
	RecordUsage(ctx context.Context, record *billing.UsageRecord) error
	ConsumeCredit(ctx context.Context, accountID, usageRecordID uuid.UUID, cost float64) (float64, error)
}

// InferenceHandler serves Teepin Inference's public, OpenAI-compatible API:
// one base URL for every model, the model chosen per request, authenticated
// with a project API key (or a signed-in session).
type InferenceHandler struct {
	gateway inferenceGateway
	catalog catalogReader
	usage   usageRecorder
}

// NewInferenceHandler wires the handler. usage may be nil (nothing is
// metered), which only makes sense in tests.
func NewInferenceHandler(g inferenceGateway, c catalogReader, u usageRecorder) *InferenceHandler {
	return &InferenceHandler{gateway: g, catalog: c, usage: u}
}

// apiError is the OpenAI error envelope, so stock client libraries surface
// our errors as they do OpenAI's.
type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

func writeInferenceError(c *gin.Context, status int, typ, code, message string) {
	c.JSON(status, gin.H{"error": apiError{Message: message, Type: typ, Code: code}})
}

// inferenceAccess is what the caller may do, resolved from their credential.
type inferenceAccess struct {
	allowed map[string]bool // nil = every model
}

func (a inferenceAccess) permits(route string) bool { return a.allowed == nil || a.allowed[route] }

// resolveAccess applies the credential's permissions. ok is false when the
// response has already been written.
func resolveAccess(c *gin.Context) (inferenceAccess, bool) {
	// A Kumbha agent credential is the platform's most narrowly scoped
	// token; it must never reach a paid model API.
	if _, isSession := auth.GetSessionID(c); isSession {
		writeInferenceError(c, http.StatusForbidden, "permission_error", "forbidden", "this credential cannot call inference models")
		return inferenceAccess{}, false
	}

	scopes, viaKey := auth.GetAPIKeyScopes(c)
	if !viaKey {
		return inferenceAccess{}, true // signed-in user: role-based, unrestricted here
	}

	invoke := false
	var restrict map[string]bool
	for _, s := range scopes {
		switch {
		case s == ScopeInferenceInvoke:
			invoke = true
		case strings.HasPrefix(s, scopeInferenceModel):
			if restrict == nil {
				restrict = map[string]bool{}
			}
			restrict[strings.TrimPrefix(s, scopeInferenceModel)] = true
		}
	}
	if !invoke {
		writeInferenceError(c, http.StatusForbidden, "permission_error", "missing_scope",
			"this API key does not have the "+ScopeInferenceInvoke+" permission")
		return inferenceAccess{}, false
	}
	return inferenceAccess{allowed: restrict}, true
}

// chatRequest is the subset of an OpenAI chat request the gateway needs. Every
// other field is passed through to the backend untouched.
type chatRequest struct {
	model     string
	messages  []json.RawMessage
	maxTokens int
	stream    bool
	extra     map[string]json.RawMessage
}

func parseChatRequest(body []byte) (*chatRequest, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("request body is not valid JSON: %v", err)
	}

	req := &chatRequest{extra: map[string]json.RawMessage{}}
	for k, v := range raw {
		switch k {
		case "model":
			if err := json.Unmarshal(v, &req.model); err != nil {
				return nil, errors.New(`"model" must be a string`)
			}
		case "messages":
			if err := json.Unmarshal(v, &req.messages); err != nil {
				return nil, errors.New(`"messages" must be an array`)
			}
		case "max_tokens", "max_completion_tokens":
			var n int
			if err := json.Unmarshal(v, &n); err != nil || n < 0 {
				return nil, fmt.Errorf("%q must be a non-negative integer", k)
			}
			// max_completion_tokens is the newer name; if both are sent it wins.
			if k == "max_completion_tokens" || req.maxTokens == 0 {
				req.maxTokens = n
			}
		case "stream":
			if err := json.Unmarshal(v, &req.stream); err != nil {
				return nil, errors.New(`"stream" must be a boolean`)
			}
		default:
			req.extra[k] = v
		}
	}
	if strings.TrimSpace(req.model) == "" {
		return nil, errors.New(`"model" is required`)
	}
	if len(req.messages) == 0 {
		return nil, errors.New(`"messages" must be a non-empty array`)
	}
	return req, nil
}

// ChatCompletions is POST /v1/chat/completions.
func (h *InferenceHandler) ChatCompletions(c *gin.Context) {
	access, ok := resolveAccess(c)
	if !ok {
		return
	}
	accountID, ok := auth.GetAccountID(c)
	if !ok {
		writeInferenceError(c, http.StatusUnauthorized, "authentication_error", "unauthorized", "authentication required")
		return
	}
	projectID, _ := auth.GetProjectID(c)

	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxInferenceBodyBytes))
	if err != nil {
		writeInferenceError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", "request body is too large")
		return
	}
	req, err := parseChatRequest(body)
	if err != nil {
		writeInferenceError(c, http.StatusBadRequest, "invalid_request_error", "invalid_request", err.Error())
		return
	}

	// Same answer whether the model does not exist or this key may not use it:
	// a key restricted to some models must not learn what else exists.
	model, err := h.catalog.GetModel(c.Request.Context(), req.model)
	if err != nil && !errors.Is(err, modelcatalog.ErrNotFound) {
		log.Printf("inference: catalog lookup for %q: %v", req.model, err)
		writeInferenceError(c, http.StatusInternalServerError, "api_error", "internal_error", "could not look up the model")
		return
	}
	if err != nil || !model.Enabled || !access.permits(req.model) {
		writeInferenceError(c, http.StatusNotFound, "invalid_request_error", "model_not_found",
			fmt.Sprintf("the model %q does not exist or you do not have access to it", req.model))
		return
	}

	ireq := inference.Request{
		Model:     req.model,
		Messages:  req.messages,
		MaxTokens: req.maxTokens,
		Stream:    req.stream,
		Extra:     req.extra,
	}
	acct := accountID.String()

	if req.stream {
		h.serveStream(c, acct, projectID, model, ireq)
		return
	}

	start := time.Now()
	resp, err := h.gateway.Complete(c.Request.Context(), acct, ireq)
	if err != nil {
		h.writeGatewayError(c, err)
		return
	}
	h.settle(c.Request.Context(), accountID, projectID, model, &resp.Usage, start)
	c.Data(http.StatusOK, "application/json", resp.Body)
}

func (h *InferenceHandler) serveStream(c *gin.Context, accountID string, projectID uuid.UUID, model *modelcatalog.Model, req inference.Request) {
	flusher, canFlush := c.Writer.(http.Flusher)
	started := false
	start := time.Now()
	var usage *inference.Usage

	begin := func() {
		if started {
			return
		}
		started = true
		h := c.Writer.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no") // stop proxies buffering the stream
		c.Writer.WriteHeader(http.StatusOK)
	}
	write := func(s string) error {
		if _, err := io.WriteString(c.Writer, s); err != nil {
			return err
		}
		if canFlush {
			flusher.Flush()
		}
		return nil
	}

	err := h.gateway.Stream(c.Request.Context(), accountID, req, func(ch inference.Chunk) error {
		begin()
		if ch.Usage != nil {
			usage = ch.Usage
		}
		if ch.Done {
			return write("data: [DONE]\n\n")
		}
		return write("data: " + string(ch.Data) + "\n\n")
	})

	// Tokens the backend generated cost real compute whether or not the client
	// stayed to read them, so usage is settled even when the stream errored or
	// the client hung up.
	if uid, perr := uuid.Parse(accountID); perr == nil {
		h.settle(c.Request.Context(), uid, projectID, model, usage, start)
	}

	if err != nil {
		if !started {
			h.writeGatewayError(c, err)
			return
		}
		// Headers are already sent; the only honest signal left is an error
		// event, then closing the stream without [DONE].
		payload, _ := json.Marshal(gin.H{"error": apiError{Message: "the model stopped responding mid-stream", Type: "api_error", Code: "stream_error"}})
		_ = write("data: " + string(payload) + "\n\n")
	}
}

// writeGatewayError maps gateway/provider failures onto OpenAI-style errors.
func (h *InferenceHandler) writeGatewayError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, inferencegateway.ErrThrottled):
		c.Header("Retry-After", "5")
		writeInferenceError(c, http.StatusTooManyRequests, "rate_limit_error", "rate_limited",
			"too many concurrent requests for this model right now; retry shortly")
	case errors.Is(err, inference.ErrProviderUnavailable):
		c.Header("Retry-After", "10")
		writeInferenceError(c, http.StatusServiceUnavailable, "api_error", "model_unavailable",
			"this model is not available right now")
	case errors.Is(err, context.Canceled):
		// Client went away; nothing useful to write.
		c.Status(499)
	default:
		log.Printf("inference: request failed: %v", err)
		writeInferenceError(c, http.StatusBadGateway, "api_error", "upstream_error", "the model returned an error")
	}
}

// settle records what a request consumed and draws it down against the
// account's credits. Runs on a detached context so a client disconnect cannot
// cancel the write that bills for tokens already generated. A failure is
// logged, never returned: the response has already been produced.
func (h *InferenceHandler) settle(ctx context.Context, accountID, projectID uuid.UUID, model *modelcatalog.Model, usage *inference.Usage, start time.Time) {
	if h.usage == nil || usage == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	end := time.Now()

	lines := []struct {
		resource        string
		tokens          int
		perMillion      float64
		vendorPerMillon *float64
	}{
		{"input_tokens", usage.InputTokens, model.InputPricePerMillion, model.VendorInputCostPerMillion},
		{"output_tokens", usage.OutputTokens, model.OutputPricePerMillion, model.VendorOutputCostPerMillion},
	}
	for _, l := range lines {
		if l.tokens <= 0 {
			continue
		}
		cost := float64(l.tokens) / 1e6 * l.perMillion
		record := &billing.UsageRecord{
			AccountID:    accountID,
			ProjectID:    projectID,
			SubjectType:  "inference_model",
			SubjectID:    model.ModelRoute,
			ResourceType: "inference/" + l.resource,
			Quantity:     float64(l.tokens),
			Unit:         "tokens",
			UnitPrice:    l.perMillion,
			TotalCost:    cost,
			Provider:     model.Engine,
			StartTime:    start,
			EndTime:      end,
		}
		if l.vendorPerMillon != nil {
			record.CostBasis = float64(l.tokens) / 1e6 * *l.vendorPerMillon
		}
		if err := h.usage.RecordUsage(ctx, record); err != nil {
			log.Printf("inference: failed to record %s for %s/%s: %v", l.resource, accountID, model.ModelRoute, err)
			continue
		}
		if cost > 0 {
			if _, err := h.usage.ConsumeCredit(ctx, accountID, record.ID, cost); err != nil {
				log.Printf("inference: failed to draw credit for %s/%s: %v", accountID, model.ModelRoute, err)
			}
		}
	}
}

type modelView struct {
	ID            string  `json:"id"`
	Object        string  `json:"object"`
	Created       int64   `json:"created"`
	OwnedBy       string  `json:"owned_by"`
	DisplayName   string  `json:"display_name"`
	ContextWindow int     `json:"context_window,omitempty"`
	SupportsTools bool    `json:"supports_tools"`
	Vision        bool    `json:"supports_vision"`
	Audio         bool    `json:"supports_audio"`
	Pricing       pricing `json:"pricing"`
}

type pricing struct {
	InputPerMillion  float64 `json:"input_per_million_tokens"`
	OutputPerMillion float64 `json:"output_per_million_tokens"`
}

// ListModels is GET /v1/models: the enabled models this credential may call,
// in the OpenAI list shape plus Teepin's capability and price fields.
func (h *InferenceHandler) ListModels(c *gin.Context) {
	access, ok := resolveAccess(c)
	if !ok {
		return
	}
	models, err := h.catalog.ListModels(c.Request.Context())
	if err != nil {
		log.Printf("inference: list models: %v", err)
		writeInferenceError(c, http.StatusInternalServerError, "api_error", "internal_error", "could not list models")
		return
	}
	out := make([]modelView, 0, len(models))
	for _, m := range models {
		if !m.Enabled || !access.permits(m.ModelRoute) {
			continue
		}
		out = append(out, modelView{
			ID: m.ModelRoute, Object: "model", Created: m.CreatedAt.Unix(), OwnedBy: "teepin", DisplayName: m.DisplayName,
			ContextWindow: m.ContextWindow, SupportsTools: m.SupportsTools, Vision: m.SupportsVision, Audio: m.SupportsAudio,
			Pricing: pricing{InputPerMillion: m.InputPricePerMillion, OutputPerMillion: m.OutputPricePerMillion},
		})
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": out})
}

// InferenceUsageRecorder is the billing surface the inference handler meters
// tokens through (implemented by *billing.Service). Exported so main.go can
// pass a nil interface, not a nil pointer, when billing is unavailable.
type InferenceUsageRecorder = usageRecorder
