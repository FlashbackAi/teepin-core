// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
)

// KumbhaRouteHandler is Control Center's operator-only view over Kumbha's
// configured routes: which ones exist, whether each is turned on, and
// whether its backend is currently reachable. Deliberately separate from
// anything customer-facing — see KUMBHA-DESIGN.md's "no console page of its
// own": a customer never learns a route's backend identity, but an operator
// deciding whether to flip teepin/deep on for a demo needs exactly that.
type KumbhaRouteHandler struct {
	router  *kumbha.Router
	store   *kumbha.RouteStore
	monitor *kumbha.RouteMonitor
}

// NewKumbhaRouteHandler wires the handler. router and monitor come from the
// same routes map main.go builds when it constructs the Kumbha Gateway.
func NewKumbhaRouteHandler(router *kumbha.Router, store *kumbha.RouteStore, monitor *kumbha.RouteMonitor) *KumbhaRouteHandler {
	return &KumbhaRouteHandler{router: router, store: store, monitor: monitor}
}

type kumbhaRouteView struct {
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Health    string `json:"health"`
	HealthErr string `json:"health_error,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

// List is GET /v1/admin/kumbha/routes.
func (h *KumbhaRouteHandler) List(c *gin.Context) {
	names := h.router.Names()
	sort.Strings(names) // stable order across requests, for a UI that doesn't want rows jumping around

	enabled, err := h.store.Enabled(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load route settings"})
		return
	}
	statuses := h.monitor.Statuses()

	views := make([]kumbhaRouteView, 0, len(names))
	for _, name := range names {
		isEnabled := true
		if v, ok := enabled[name]; ok {
			isEnabled = v
		}
		st := statuses[name]
		v := kumbhaRouteView{Name: name, Enabled: isEnabled, Health: string(st.Status), HealthErr: st.Error}
		if !st.CheckedAt.IsZero() {
			v.CheckedAt = st.CheckedAt.Format(rfc3339)
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

	// Refuses a route name the router does not actually recognise — a
	// typo here would otherwise sit in the table forever doing nothing,
	// silently, until someone happened to notice via List.
	known := false
	for _, n := range h.router.Names() {
		if n == name {
			known = true
			break
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

// rfc3339 avoids importing "time" into this file just for one format string.
const rfc3339 = "2006-01-02T15:04:05Z07:00"
