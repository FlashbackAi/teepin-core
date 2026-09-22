// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"testing"

	"github.com/FlashbackAi/teepin-core/pkg/api"
)

// Gin panics at registration time when two routes conflict, which neither the
// compiler nor go vet catches — it would only surface as a crash at startup.
// Registering every optional handler at once proves the route table is
// coherent, including the public inference routes beside Kumbha's own
// /v1/kumbha/chat/completions.
func TestSetupRouter_RegistersEveryRouteWithoutConflict(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("route registration panicked: %v", r)
		}
	}()

	router := setupRouter(
		&api.Server{}, &api.AuthHandler{}, &api.AccountHandler{}, nil,
		&api.BillingHandler{}, &api.RegistryHandler{}, &api.AdminHandler{}, &api.WebhookHandler{},
		&api.NodeHandler{}, &api.ModelCatalogHandler{}, &api.NodeServicesHandler{},
		&api.InferencePlaygroundHandler{}, &api.NodeModelCacheHandler{}, &api.InferenceHandler{},
		&api.KumbhaRouteHandler{}, nil, nil, nil, nil, "teepin.test",
	)

	want := map[string]bool{
		"POST /v1/chat/completions":                false,
		"GET /v1/models":                           false,
		"POST /v1/kumbha/chat/completions":         false,
		"GET /v1/admin/nodes/:id/cached-models":    false,
		"DELETE /v1/admin/nodes/:id/cached-models": false,
		"GET /v1/admin/kumbha/routes":              false,
		"PUT /v1/admin/kumbha/routes":              false,
	}
	for _, r := range router.Routes() {
		key := r.Method + " " + r.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Errorf("route not registered: %s", route)
		}
	}
}
