// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
)

// ModelCatalogHandler serves Teepin Inference's admin-only model catalog
// and pricing endpoints. Mounted under /v1/admin, same operator-token guard
// as the rest of that group (see AdminHandler.RequireAdminToken).
type ModelCatalogHandler struct {
	catalog *modelcatalog.Service
}

// NewModelCatalogHandler wires the catalog service.
func NewModelCatalogHandler(svc *modelcatalog.Service) *ModelCatalogHandler {
	return &ModelCatalogHandler{catalog: svc}
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
}

// RegisterModel is POST /v1/admin/inference/models — creates or updates a
// catalog entry's capabilities/engine. Pricing is a separate call
// (SetPricing) so this can never reset a price an admin already set.
func (h *ModelCatalogHandler) RegisterModel(c *gin.Context) {
	var req registerModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updatedBy := "admin-api"
	err := h.catalog.RegisterModel(c.Request.Context(), modelcatalog.Model{
		ModelRoute:     req.ModelRoute,
		DisplayName:    req.DisplayName,
		CostClass:      modelcatalog.CostClass(req.CostClass),
		Engine:         req.Engine,
		ContextWindow:  req.ContextWindow,
		SupportsTools:  req.SupportsTools,
		SupportsVision: req.SupportsVision,
		SupportsAudio:  req.SupportsAudio,
		Enabled:        req.Enabled,
		UpdatedBy:      &updatedBy,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	m, err := h.catalog.GetModel(c.Request.Context(), req.ModelRoute)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, m)
}

// ListModels is GET /v1/admin/inference/models — every catalog entry,
// enabled and disabled alike; the admin catalog page wants to show
// disabled ones too.
func (h *ModelCatalogHandler) ListModels(c *gin.Context) {
	models, err := h.catalog.ListModels(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"models": models})
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
	if err := h.catalog.DeleteModel(c.Request.Context(), route); err != nil {
		if errors.Is(err, modelcatalog.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "model deleted", "model_route": route})
}
