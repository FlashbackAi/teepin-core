// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
	"github.com/FlashbackAi/teepin-core/pkg/modelprobe"
)

// ModelProber is the capability checker the catalog admin endpoints drive
// (*modelprobe.Service).
type ModelProber interface {
	CheckAsync(route string) bool
	IsRunning(route string) bool
	Latest(ctx context.Context, route string) (*modelprobe.Report, error)
	Reports(ctx context.Context) (map[string]*modelprobe.Report, error)
	Discover(ctx context.Context, m modelcatalog.Model, apiKey string) (*modelprobe.MetadataReport, error)
	Invalidate(ctx context.Context, route string) error
}

// WithProbes turns on capability checking: results appear on the catalog
// listing, a model is checked when it is first registered or its backend
// changes, and an operator can re-check on demand.
func (h *ModelCatalogHandler) WithProbes(p ModelProber) *ModelCatalogHandler {
	h.probes = p
	return h
}

// effectiveCapability is what the platform treats a model as being able to do,
// and why: evidence where there is some, the operator's declaration otherwise.
type effectiveCapability struct {
	Allowed  bool   `json:"allowed"`
	Basis    string `json:"basis"`
	Declared bool   `json:"declared"`
}

// capabilityView is a model's capability evidence for the admin page.
type capabilityView struct {
	Report    *modelprobe.Report             `json:"report,omitempty"`
	Running   bool                           `json:"running"`
	Effective map[string]effectiveCapability `json:"effective"`
}

func buildCapabilityView(m modelcatalog.Model, r *modelprobe.Report, running bool) *capabilityView {
	declared := map[modelprobe.Capability]bool{
		modelprobe.CapTools:  m.SupportsTools,
		modelprobe.CapVision: m.SupportsVision,
		modelprobe.CapAudio:  m.SupportsAudio,
	}
	v := &capabilityView{Report: r, Running: running, Effective: map[string]effectiveCapability{}}
	for c, d := range declared {
		ok, basis := modelprobe.Effective(r, c, d)
		v.Effective[string(c)] = effectiveCapability{Allowed: ok, Basis: basis, Declared: d}
	}
	return v
}

// CheckModel is POST /v1/admin/inference/models/check?model_route=... — starts
// a capability check (metadata plus a real test of tools, vision and audio)
// and returns at once; the result appears on the listing and on
// GET .../capabilities when it finishes.
func (h *ModelCatalogHandler) CheckModel(c *gin.Context) {
	if h.probes == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "capability checks are not enabled on this deployment"})
		return
	}
	route := c.Query("model_route")
	if route == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_route is required"})
		return
	}
	if _, err := h.catalog.GetModel(c.Request.Context(), route); err != nil {
		if errors.Is(err, modelcatalog.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	started := h.probes.CheckAsync(route)
	c.JSON(http.StatusAccepted, gin.H{"started": started, "running": true})
}

// ModelCapabilities is GET /v1/admin/inference/models/capabilities?model_route=...
func (h *ModelCatalogHandler) ModelCapabilities(c *gin.Context) {
	if h.probes == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "capability checks are not enabled on this deployment"})
		return
	}
	route := c.Query("model_route")
	m, err := h.catalog.GetModel(c.Request.Context(), route)
	if err != nil {
		if errors.Is(err, modelcatalog.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	rep, err := h.probes.Latest(c.Request.Context(), route)
	if err != nil && !errors.Is(err, modelprobe.ErrNoReport) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, buildCapabilityView(*m, rep, h.probes.IsRunning(route)))
}

type discoverModelRequest struct {
	Provider      string `json:"provider"`
	BaseURL       string `json:"base_url"`
	ProviderModel string `json:"provider_model"`
	// APIKey is used for this one lookup and never stored.
	APIKey string `json:"api_key,omitempty"`
}

// DiscoverModel is POST /v1/admin/inference/models/discover — asks a backend
// about a model that is not registered yet (context window, output limit, image
// input where it says), so the registration form can be filled in from the
// source instead of typed from memory. Nothing is stored.
func (h *ModelCatalogHandler) DiscoverModel(c *gin.Context) {
	if h.probes == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "capability checks are not enabled on this deployment"})
		return
	}
	var req discoverModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Provider == "" || req.ProviderModel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider and provider_model are required"})
		return
	}
	md, err := h.probes.Discover(c.Request.Context(), modelcatalog.Model{
		ModelRoute:    "discover/unsaved",
		Provider:      modelcatalog.Provider(req.Provider),
		ProviderModel: req.ProviderModel,
		BaseURL:       req.BaseURL,
	}, req.APIKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"metadata": md})
}

// afterRegister decides what a registration means for the model's evidence: a
// model checked for the first time, or whose backend changed (a different
// provider, endpoint or model id, or a new API key), is re-checked, because the
// old results describe something else. An ordinary edit (a price, a label)
// leaves them alone.
func (h *ModelCatalogHandler) afterRegister(ctx context.Context, prev *modelcatalog.Model, saved modelcatalog.Model, newKey bool) {
	if h.probes == nil {
		return
	}
	changed := prev == nil || prev.Provider != saved.Provider || prev.ProviderModel != saved.ProviderModel ||
		prev.BaseURL != saved.BaseURL || newKey
	if !changed {
		if _, err := h.probes.Latest(ctx, saved.ModelRoute); !errors.Is(err, modelprobe.ErrNoReport) {
			return
		}
	} else if prev != nil {
		_ = h.probes.Invalidate(ctx, saved.ModelRoute)
	}
	h.probes.CheckAsync(saved.ModelRoute)
}
