// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
)

// KumbhaRouteHandler is Control Center's operator-only view over Kumbha's
// configured routes: which ones exist, whether each is turned on, and
// whether its backend is currently reachable. Deliberately separate from
// anything customer-facing — see KUMBHA-DESIGN.md's "no console page of its
// own": a customer never learns a route's backend identity, but an operator
// deciding whether to flip teepin/deep on for a demo needs exactly that.
//
// candidates/secrets/environment are OPTIONAL — nil candidates disables
// every candidate endpoint (404) and List falls back to showing only the
// static, env-var-configured routes, exactly as before candidates existed.
type KumbhaRouteHandler struct {
	router      *kumbha.Router
	store       *kumbha.RouteStore
	monitor     *kumbha.RouteMonitor
	candidates  *kumbha.CandidateStore
	secrets     kumbha.SecretsClient
	environment string
}

// NewKumbhaRouteHandler wires the handler. router and monitor come from the
// same routes map main.go builds when it constructs the Kumbha Gateway.
func NewKumbhaRouteHandler(router *kumbha.Router, store *kumbha.RouteStore, monitor *kumbha.RouteMonitor, candidates *kumbha.CandidateStore, secrets kumbha.SecretsClient, environment string) *KumbhaRouteHandler {
	return &KumbhaRouteHandler{router: router, store: store, monitor: monitor, candidates: candidates, secrets: secrets, environment: environment}
}

type candidateView struct {
	ID              string `json:"id"`
	Priority        int    `json:"priority"`
	ProviderType    string `json:"provider_type"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	ContextWindow   int    `json:"context_window"`
	SupportsTools   bool   `json:"supports_tools"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Enabled         bool   `json:"enabled"`
	HasSecret       bool   `json:"has_secret"`
	Health          string `json:"health"`
	HealthErr       string `json:"health_error,omitempty"`
	CheckedAt       string `json:"checked_at,omitempty"`
}

func candidateViewFrom(c kumbha.RouteCandidate) candidateView {
	return candidateView{
		ID: c.ID.String(), Priority: c.Priority, ProviderType: c.ProviderType,
		BaseURL: c.BaseURL, Model: c.Model, ContextWindow: c.ContextWindow,
		SupportsTools: c.SupportsTools, MaxOutputTokens: c.MaxOutputTokens,
		Enabled: c.Enabled, HasSecret: c.HasSecret, Health: string(kumbha.HealthUnknown),
	}
}

type kumbhaRouteView struct {
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Health    string `json:"health"`
	HealthErr string `json:"health_error,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
	// Candidates is only populated on a deployment with candidates wired in
	// (WithCandidates on the Gateway/RouteMonitor) — nil/omitted on one
	// that still only has the static, env-var-configured route, same as
	// before this existed.
	Candidates []candidateView `json:"candidates,omitempty"`
}

// List is GET /v1/admin/kumbha/routes.
func (h *KumbhaRouteHandler) List(c *gin.Context) {
	ctx := c.Request.Context()

	// A route name can come from either source now: the static,
	// env-var-configured router, or a candidate row created entirely from
	// Control Center (a route with no router entry at all). Union them so
	// a brand-new route shows up here even though main.go never heard of
	// it.
	names := map[string]bool{}
	for _, n := range h.router.Names() {
		names[n] = true
	}
	var byRoute map[string][]kumbha.RouteCandidate
	if h.candidates != nil {
		var err error
		byRoute, err = h.candidates.ListAll(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load route candidates"})
			return
		}
		for name := range byRoute {
			names[name] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted) // stable order across requests, for a UI that doesn't want rows jumping around

	enabled, err := h.store.Enabled(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load route settings"})
		return
	}
	statuses := h.monitor.Statuses()
	candidateHealth := h.monitor.CandidateStatuses()

	views := make([]kumbhaRouteView, 0, len(sorted))
	for _, name := range sorted {
		isEnabled := true
		if v, ok := enabled[name]; ok {
			isEnabled = v
		}
		st := statuses[name]
		v := kumbhaRouteView{Name: name, Enabled: isEnabled, Health: string(st.Status), HealthErr: st.Error}
		if !st.CheckedAt.IsZero() {
			v.CheckedAt = st.CheckedAt.Format(rfc3339)
		}
		for _, row := range byRoute[name] {
			cv := candidateViewFrom(row)
			if ch, ok := candidateHealth[row.ID]; ok {
				cv.Health = string(ch.Status)
				cv.HealthErr = ch.Error
				if !ch.CheckedAt.IsZero() {
					cv.CheckedAt = ch.CheckedAt.Format(rfc3339)
				}
			}
			v.Candidates = append(v.Candidates, cv)
		}
		views = append(views, v)
	}
	c.JSON(http.StatusOK, gin.H{"routes": views})
}

type setKumbhaRouteEnabledRequest struct {
	Enabled *bool `json:"enabled" binding:"required"`
}

// SetEnabled is PUT /v1/admin/kumbha/routes?route=teepin/fast — turns one
// route on or off. The route name travels as a QUERY parameter, never a
// path segment: a route contains a literal "/" ("teepin/fast"), which would
// collide with gin's single-segment path params — same reasoning the model
// catalog's own `?model_route=` and object storage's `?key=` already use for
// slash-bearing identifiers. Takes effect on the NEXT request that resolves
// this route; sessions already using it are unaffected (nothing tears down
// an in-flight completion over a config change mid-request).
func (h *KumbhaRouteHandler) SetEnabled(c *gin.Context) {
	name := c.Query("route")
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "route name is required"})
		return
	}

	var req setKumbhaRouteEnabledRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enabled (bool) is required"})
		return
	}

	// Refuses a route name neither the router nor any candidate recognises
	// — a typo here would otherwise sit in the table forever doing
	// nothing, silently, until someone happened to notice via List.
	known := false
	for _, n := range h.router.Names() {
		if n == name {
			known = true
			break
		}
	}
	if !known && h.candidates != nil {
		if rows, err := h.candidates.ListByRoute(c.Request.Context(), name); err == nil && len(rows) > 0 {
			known = true
		}
	}
	if !known {
		c.JSON(http.StatusNotFound, gin.H{"error": "no such route is configured on this deployment"})
		return
	}

	// No per-operator identity is tracked on this admin surface today —
	// matches modelcatalog's own RegisterModel, which records the same
	// literal string rather than a real caller identity.
	if err := h.store.SetEnabled(c.Request.Context(), name, *req.Enabled, "admin-api"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not update route setting"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "updated"})
}

type candidateRequest struct {
	RouteName       string `json:"route_name"` // required on create; ignored on update (a candidate's route is immutable — see CandidateStore.Update)
	Priority        int    `json:"priority"`
	ProviderType    string `json:"provider_type" binding:"required"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model" binding:"required"`
	ContextWindow   int    `json:"context_window"`
	SupportsTools   bool   `json:"supports_tools"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Enabled         *bool  `json:"enabled"`
	// APIKey is write-only — never returned by any candidate endpoint.
	// Create: sets the initial key, if given. Update: rotates the key
	// ONLY when non-empty; an empty value leaves whatever key (if any) is
	// already stored untouched, so an operator editing base_url doesn't
	// have to also re-paste the key every time.
	APIKey string `json:"api_key,omitempty"`
}

func (r candidateRequest) toInput(routeName string, existingEnabled bool) kumbha.CandidateInput {
	enabled := existingEnabled
	if r.Enabled != nil {
		enabled = *r.Enabled
	}
	maxOut := r.MaxOutputTokens
	if maxOut == 0 {
		maxOut = 4096
	}
	return kumbha.CandidateInput{
		RouteName: routeName, Priority: r.Priority, ProviderType: r.ProviderType,
		BaseURL: r.BaseURL, Model: r.Model, ContextWindow: r.ContextWindow,
		SupportsTools: r.SupportsTools, MaxOutputTokens: maxOut, Enabled: enabled,
	}
}

// CreateCandidate is POST /v1/admin/kumbha/candidates — registers a new
// backend candidate for a route, creating the route itself (in the sense
// List will now show it) if it didn't exist. This is what makes adding a
// whole new backend, or a whole new route, a Control Center action instead
// of a code change plus a redeploy.
func (h *KumbhaRouteHandler) CreateCandidate(c *gin.Context) {
	if h.candidates == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "route candidates are not available on this deployment"})
		return
	}
	var req candidateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RouteName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "route_name is required"})
		return
	}

	candidate, err := h.candidates.Create(c.Request.Context(), req.toInput(req.RouteName, true), "admin-api")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.APIKey != "" {
		if err := h.setSecret(c.Request.Context(), candidate.ID, req.APIKey); err != nil {
			c.JSON(http.StatusCreated, gin.H{
				"candidate": candidateViewFrom(candidate),
				"warning":   fmt.Sprintf("candidate created, but its API key could not be saved: %v — set it again from Control Center", err),
			})
			return
		}
		candidate.HasSecret = true
	}
	c.JSON(http.StatusCreated, gin.H{"candidate": candidateViewFrom(candidate)})
}

// UpdateCandidate is PUT /v1/admin/kumbha/candidates/:id — edits a
// candidate's configuration, priority, enabled state, and optionally
// rotates its API key. Takes effect on the NEXT request that dispatches
// through this candidate (ProviderFactory rebuilds only when config or
// secret actually changed) — no redeploy, no restart.
func (h *KumbhaRouteHandler) UpdateCandidate(c *gin.Context) {
	if h.candidates == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "route candidates are not available on this deployment"})
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid candidate id"})
		return
	}
	existing, err := h.candidates.Get(c.Request.Context(), id)
	if errors.Is(err, kumbha.ErrCandidateNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "candidate not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load candidate"})
		return
	}

	var req candidateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updated, err := h.candidates.Update(c.Request.Context(), id, req.toInput(existing.RouteName, existing.Enabled), "admin-api")
	if errors.Is(err, kumbha.ErrCandidateNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "candidate not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.APIKey != "" {
		if err := h.setSecret(c.Request.Context(), id, req.APIKey); err != nil {
			c.JSON(http.StatusOK, gin.H{
				"candidate": candidateViewFrom(updated),
				"warning":   fmt.Sprintf("candidate updated, but its API key could not be saved: %v", err),
			})
			return
		}
		updated.HasSecret = true
	}
	c.JSON(http.StatusOK, gin.H{"candidate": candidateViewFrom(updated)})
}

// DeleteCandidate is DELETE /v1/admin/kumbha/candidates/:id.
func (h *KumbhaRouteHandler) DeleteCandidate(c *gin.Context) {
	if h.candidates == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "route candidates are not available on this deployment"})
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid candidate id"})
		return
	}
	if err := h.candidates.Delete(c.Request.Context(), id); err != nil {
		if errors.Is(err, kumbha.ErrCandidateNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "candidate not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not delete candidate"})
		return
	}
	// Best-effort secret cleanup — not failing the delete over a Secrets
	// Manager hiccup, since the row (the thing that actually matters for
	// routing) is already gone. An orphaned secret costs nothing and sits
	// under its own kumbha-candidate-* prefix, easy to find and remove by
	// hand later if this happens.
	if h.secrets != nil {
		_ = h.secrets.Delete(c.Request.Context(), kumbha.CandidateSecretName(h.environment, id))
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

func (h *KumbhaRouteHandler) setSecret(ctx context.Context, id uuid.UUID, value string) error {
	if h.secrets == nil {
		return fmt.Errorf("no secrets backend configured on this deployment")
	}
	if err := h.secrets.Put(ctx, kumbha.CandidateSecretName(h.environment, id), value); err != nil {
		return err
	}
	return h.candidates.SetHasSecret(ctx, id, true, "admin-api")
}

// rfc3339 avoids importing "time" into this file just for one format string.
const rfc3339 = "2006-01-02T15:04:05Z07:00"
