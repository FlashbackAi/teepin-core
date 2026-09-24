// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/inferencegateway"
	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
)

// ModelCatalogHandler serves the admin-only model registry: every model the
// platform can serve, how each is reached, its pricing, and where it may be
// used (Teepin Inference's customer API, the Kumbha build agent, or both).
// Mounted under /v1/admin, same operator-token guard as the rest of that
// group (see AdminHandler.RequireAdminToken).
type ModelCatalogHandler struct {
	catalog *modelcatalog.Service

	// status reports each model's live servability; nil omits it.
	status ModelStatusSource
	// secrets/environment store external models' API keys; nil secrets
	// makes an api_key in a registration a reported, non-fatal failure.
	secrets     inferencegateway.SecretsClient
	environment string
}

// ModelStatusSource reports whether a model can be served right now —
// implemented by *inferencegateway.Gateway.
type ModelStatusSource interface {
	Status(ctx context.Context, m modelcatalog.Model) inferencegateway.ModelStatus
}

// NewModelCatalogHandler wires the catalog service.
func NewModelCatalogHandler(svc *modelcatalog.Service) *ModelCatalogHandler {
	return &ModelCatalogHandler{catalog: svc}
}

// WithModelStatus adds each model's live status to the catalog listing.
func (h *ModelCatalogHandler) WithModelStatus(src ModelStatusSource) *ModelCatalogHandler {
	h.status = src
	return h
}

// WithAPIKeys enables storing external models' API keys in Secrets Manager.
func (h *ModelCatalogHandler) WithAPIKeys(secrets inferencegateway.SecretsClient, environment string) *ModelCatalogHandler {
	h.secrets = secrets
	h.environment = environment
	return h
}

// Every model-scoped endpoint below addresses a model by a `model_route`
// QUERY parameter, never a path segment — a route contains a literal "/"
// (e.g. "teepin/qwen3-omni-7b"), which would collide with gin's
// single-segment path params. Same reasoning object storage's own `?key=`
// parameter already uses for its slash-bearing object keys.

type registerModelRequest struct {
	ModelRoute     string `json:"model_route" binding:"required"`
	DisplayName    string `json:"display_name" binding:"required"`
	CostClass      string `json:"cost_class" binding:"required"`
	Engine         string `json:"engine" binding:"required"`
	ContextWindow  int    `json:"context_window"`
	SupportsTools  bool   `json:"supports_tools"`
	SupportsVision bool   `json:"supports_vision"`
	SupportsAudio  bool   `json:"supports_audio"`
	// Enabled has no explicit default in JSON binding terms — an omitted
	// field binds to false. Deliberate: a freshly registered model stays
	// unroutable until an admin explicitly flips it on, the safer default
	// for something that gates live traffic.
	Enabled bool `json:"enabled"`

	// How the model is served; empty provider means self-hosted ("node").
	Provider        string `json:"provider"`
	ProviderModel   string `json:"provider_model"`
	BaseURL         string `json:"base_url"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	// APIKey is write-only — never returned by any endpoint. Empty leaves
	// any key already stored untouched, so editing a model's settings never
	// requires re-pasting its key.
	APIKey string `json:"api_key,omitempty"`

	availabilityRequest
}

// availabilityRequest is where a model may be used; a nil field is left as
// it is (see modelcatalog.Availability).
type availabilityRequest struct {
	OfferedToCustomers *bool   `json:"offered_to_customers"`
	KumbhaEnabled      *bool   `json:"kumbha_enabled"`
	KumbhaPriority     *int    `json:"kumbha_priority"`
	KumbhaAlias        *string `json:"kumbha_alias"`
}

func (r availabilityRequest) toAvailability() modelcatalog.Availability {
	return modelcatalog.Availability{
		OfferedToCustomers: r.OfferedToCustomers,
		KumbhaEnabled:      r.KumbhaEnabled,
		KumbhaPriority:     r.KumbhaPriority,
		KumbhaAlias:        r.KumbhaAlias,
	}
}

func (r availabilityRequest) any() bool {
	return r.OfferedToCustomers != nil || r.KumbhaEnabled != nil || r.KumbhaPriority != nil || r.KumbhaAlias != nil
}

// RegisterModel is POST /v1/admin/inference/models — creates or updates a
// model's capabilities and how it is served, and optionally its
// availability and API key. Pricing is a separate call (SetPricing) so
// this can never reset a price an admin already set.
func (h *ModelCatalogHandler) RegisterModel(c *gin.Context) {
	var req registerModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()

	updatedBy := "admin-api"
	err := h.catalog.RegisterModel(ctx, modelcatalog.Model{
		ModelRoute:      req.ModelRoute,
		DisplayName:     req.DisplayName,
		CostClass:       modelcatalog.CostClass(req.CostClass),
		Engine:          req.Engine,
		ContextWindow:   req.ContextWindow,
		SupportsTools:   req.SupportsTools,
		SupportsVision:  req.SupportsVision,
		SupportsAudio:   req.SupportsAudio,
		Enabled:         req.Enabled,
		Provider:        modelcatalog.Provider(req.Provider),
		ProviderModel:   req.ProviderModel,
		BaseURL:         req.BaseURL,
		MaxOutputTokens: req.MaxOutputTokens,
		UpdatedBy:       &updatedBy,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.availabilityRequest.any() {
		if err := h.catalog.SetAvailability(ctx, req.ModelRoute, req.toAvailability(), updatedBy); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	var warning string
	if req.APIKey != "" {
		if err := h.storeAPIKey(ctx, req.ModelRoute, req.APIKey); err != nil {
			// The model itself saved; only the key did not. Reported
			// rather than failing the whole registration.
			warning = fmt.Sprintf("model saved, but its API key could not be stored: %v — set it again", err)
		}
	}

	m, err := h.catalog.GetModel(ctx, req.ModelRoute)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if warning != "" {
		c.JSON(http.StatusOK, gin.H{"model": m, "warning": warning})
		return
	}
	c.JSON(http.StatusOK, gin.H{"model": m})
}

// storeAPIKey writes a model's API key to Secrets Manager — under the
// model's existing secret when it has one, so a rotation keeps the same
// name — and records where it is.
func (h *ModelCatalogHandler) storeAPIKey(ctx context.Context, modelRoute, key string) error {
	if h.secrets == nil {
		return errors.New("no secrets backend is configured on this deployment")
	}
	m, err := h.catalog.GetModel(ctx, modelRoute)
	if err != nil {
		return err
	}
	ref := m.APIKeyRef
	if ref == "" {
		ref = inferencegateway.NewAPIKeyRef()
	}
	if err := h.secrets.Put(ctx, inferencegateway.SecretName(h.environment, ref), key); err != nil {
		return err
	}
	return h.catalog.SetAPIKeyRef(ctx, modelRoute, ref, "admin-api")
}

// catalogModelView is a catalog entry plus its live status.
type catalogModelView struct {
	modelcatalog.Model
	Status *inferencegateway.ModelStatus `json:"status,omitempty"`
}

// ListModels is GET /v1/admin/inference/models — every catalog entry,
// enabled and disabled alike (the admin page shows disabled ones too),
// with each model's live status when a status source is wired.
func (h *ModelCatalogHandler) ListModels(c *gin.Context) {
	ctx := c.Request.Context()
	models, err := h.catalog.ListModels(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	views := make([]catalogModelView, 0, len(models))
	for _, m := range models {
		v := catalogModelView{Model: m}
		if h.status != nil && m.Enabled {
			st := h.status.Status(ctx, m)
			v.Status = &st
		}
		views = append(views, v)
	}
	c.JSON(http.StatusOK, gin.H{"models": views})
}

// SetAvailability is PUT /v1/admin/inference/models/availability?model_route=...
// — whether a model is offered to customers, and whether (and in what
// order) the Kumbha build agent may use it. Omitted fields are unchanged.
func (h *ModelCatalogHandler) SetAvailability(c *gin.Context) {
	route := c.Query("model_route")
	if route == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_route is required"})
		return
	}
	var req availabilityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !req.any() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "set at least one of offered_to_customers, kumbha_enabled, kumbha_priority"})
		return
	}
	if err := h.catalog.SetAvailability(c.Request.Context(), route, req.toAvailability(), "admin-api"); err != nil {
		if errors.Is(err, modelcatalog.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "availability updated", "model_route": route})
}

// GetModel is GET /v1/admin/inference/models/one?model_route=...
func (h *ModelCatalogHandler) GetModel(c *gin.Context) {
	route := c.Query("model_route")
	if route == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_route is required"})
		return
	}
	m, err := h.catalog.GetModel(c.Request.Context(), route)
	if err != nil {
		if errors.Is(err, modelcatalog.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, m)
}

type setModelPricingRequest struct {
	InputPricePerMillion  float64 `json:"input_price_per_million"`
	OutputPricePerMillion float64 `json:"output_price_per_million"`
}

// SetPricing is PUT /v1/admin/inference/models/pricing?model_route=...
// Zero is a valid rate ("do not charge"), same contract as every other
// price this platform exposes.
func (h *ModelCatalogHandler) SetPricing(c *gin.Context) {
	route := c.Query("model_route")
	if route == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_route is required"})
		return
	}
	var req setModelPricingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.catalog.SetPricing(c.Request.Context(), route, req.InputPricePerMillion, req.OutputPricePerMillion, "admin-api"); err != nil {
		if errors.Is(err, modelcatalog.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "pricing updated", "model_route": route,
		"input_price_per_million": req.InputPricePerMillion, "output_price_per_million": req.OutputPricePerMillion,
	})
}

type setVendorCostRequest struct {
	VendorInputCostPerMillion  *float64 `json:"vendor_input_cost_per_million"`
	VendorOutputCostPerMillion *float64 `json:"vendor_output_cost_per_million"`
}

// SetVendorCost is PUT /v1/admin/inference/models/vendor-cost?model_route=...
// — margin-tracking data for a frontier model, distinct from the
// customer-facing rate SetPricing sets. Either field may be null, which
// clears it back to "unknown" rather than a fabricated zero.
func (h *ModelCatalogHandler) SetVendorCost(c *gin.Context) {
	route := c.Query("model_route")
	if route == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_route is required"})
		return
	}
	var req setVendorCostRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.catalog.SetVendorCost(c.Request.Context(), route, req.VendorInputCostPerMillion, req.VendorOutputCostPerMillion, "admin-api"); err != nil {
		if errors.Is(err, modelcatalog.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "vendor cost updated", "model_route": route})
}

type setModelEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

// SetEnabled is PUT /v1/admin/inference/models/enabled?model_route=... —
// gates routing without touching pricing/audit history, for retiring or
// re-enabling a model.
func (h *ModelCatalogHandler) SetEnabled(c *gin.Context) {
	route := c.Query("model_route")
	if route == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_route is required"})
		return
	}
	var req setModelEnabledRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.catalog.SetEnabled(c.Request.Context(), route, req.Enabled, "admin-api"); err != nil {
		if errors.Is(err, modelcatalog.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "enabled updated", "model_route": route, "enabled": req.Enabled})
}

// DeleteModel is DELETE /v1/admin/inference/models?model_route=... —
// removes a catalog entry entirely. Prefer SetEnabled(false) for a model
// that has ever been priced or billed against; this is for cleaning up a
// mistaken registration.
func (h *ModelCatalogHandler) DeleteModel(c *gin.Context) {
	route := c.Query("model_route")
	if route == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_route is required"})
		return
	}
	ctx := c.Request.Context()
	existing, err := h.catalog.GetModel(ctx, route)
	if errors.Is(err, modelcatalog.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := h.catalog.DeleteModel(ctx, route); err != nil {
		if errors.Is(err, modelcatalog.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Best-effort key cleanup: the catalog row, which is what routing reads,
	// is already gone, and an orphaned secret costs nothing and is easy to
	// find under its own prefix.
	if existing.APIKeyRef != "" && h.secrets != nil {
		if err := h.secrets.Delete(ctx, inferencegateway.SecretName(h.environment, existing.APIKeyRef)); err != nil {
			log.Printf("WARN: model %q deleted but its API key secret was not: %v", route, err)
		}
	}
	c.JSON(http.StatusOK, gin.H{"message": "model deleted", "model_route": route})
}
