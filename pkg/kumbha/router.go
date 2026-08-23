// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"fmt"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// Route names a model route the way the customer's own request selects
// it — "teepin/fast", "teepin/deep" — deliberately not "the vllm route"
// or "the anthropic route": the customer never learns which backend
// served a route, only that this route exists (see KUMBHA-DESIGN.md,
// "Kumbha has no console page of its own").
type Route struct {
	Provider inference.Provider
	// ProviderName is written to billing.usage_records.provider /
	// inference_session_usage.provider for margin attribution — the ONE
	// place the backend identity is allowed to appear. It is Provider.Name(),
	// captured here rather than called again at accrual time, so the
	// recorded provider can never drift from what actually served the
	// request even if routes are reconfigured mid-session.
	ProviderName string
}

// Router resolves a model name to the Route that serves it.
//
// Route-by-model-name, not a header or any other protocol extension — the
// OpenRouter precedent KUMBHA-DESIGN.md cites — so any OpenAI-compatible
// client selects a route with no protocol extension, including a
// customer's own script or a harness that has never heard of Teepin.
type Router struct {
	routes map[string]Route
}

// NewRouter builds a router from an explicit name->Route map. The caller
// (main.go) decides which routes exist, rather than the router
// discovering them: a route to a backend that was never configured (e.g.
// teepin/deep with no Anthropic key set) is simply absent from the map,
// and Resolve reports it as unknown rather than dispatching to a nil
// Provider.
func NewRouter(routes map[string]Route) *Router {
	return &Router{routes: routes}
}

// Resolve returns the Route for a model name, or
// inference.ErrUnknownModel — the same sentinel error pkg/inference
// already defines for "this model does not exist", reused here rather
// than inventing a second one, since from the caller's perspective an
// unrouted model name and an unknown model are the same failure.
func (r *Router) Resolve(model string) (Route, error) {
	route, ok := r.routes[model]
	if !ok || route.Provider == nil {
		return Route{}, fmt.Errorf("%w: %q", inference.ErrUnknownModel, model)
	}
	return route, nil
}
