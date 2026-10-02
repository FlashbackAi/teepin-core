// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import "context"

// routeModel returns the catalog's view of route (its effective capabilities,
// from what the model was seen to do), or false when it cannot be found: no such
// model, or the list could not be read. Best effort, like routeLimits: a failed
// lookup falls back to the agent's defaults rather than blocking a launch.
func (g *Gateway) routeModel(ctx context.Context, route string) (Model, bool) {
	models, err := g.kumbhaModels(ctx)
	if err != nil {
		return Model{}, false
	}
	for _, m := range models {
		if m.Route == route {
			return m, true
		}
	}
	return Model{}, false
}

// launchProfile is what the agent pod is told about its model beyond its
// limits: whether it may be shown images, and how it talks about tools.
//
// Vision follows the model: a model that can see is allowed screenshots, and
// one that cannot is told so, instead of one switch for every model. When the
// model is not found the operator's setting is used.
//
// ToolMode is "native" (structured tool calls) unless the model was found to
// need the text protocol, in which case the agent harness describes the tools in
// its prompt and reads calls out of the reply.
func (g *Gateway) launchProfile(ctx context.Context, route string) (vision bool, toolMode string) {
	m, ok := g.routeModel(ctx, route)
	if !ok {
		return g.agentConfig.VisionCapable, "native"
	}
	mode := m.ToolMode
	if mode == "" {
		mode = "native"
	}
	return m.SupportsVision, mode
}
