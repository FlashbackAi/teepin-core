// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"errors"
	"strings"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

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

// AgentStart is how the agent pod's start is going, in words safe to show a
// customer. Status is the cluster's vocabulary (pending, running, failed,
// terminated) or "missing" when the pod cannot be found.
type AgentStart struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// AgentStart reads the agent pod's current status. It exists so the console can
// say WHY a build has not started (waiting for capacity, fetching the builder
// environment, failed to start) instead of a page that just says "watching".
func (g *Gateway) AgentStart(ctx context.Context, sess *Session) (AgentStart, error) {
	if g.cluster == nil || sess.AgentInstanceID == "" {
		return AgentStart{}, nil
	}
	st, err := g.cluster.GetInstanceStatus(ctx, cluster.ProjectScope(sess.ProjectID.String()), sess.AgentInstanceID)
	if err != nil {
		if errors.Is(err, cluster.ErrNotFound) {
			return AgentStart{Status: "missing", Detail: friendlyStartDetail("missing", "")}, nil
		}
		return AgentStart{}, err
	}
	return AgentStart{Status: st.Status, Detail: friendlyStartDetail(st.Status, st.Message)}, nil
}

// friendlyStartDetail turns a pod's status message into a sentence for the
// customer. The raw message names images, nodes and registries, which are the
// platform's business, so it is mapped to a few causes and never passed through.
func friendlyStartDetail(status, message string) string {
	m := strings.ToLower(message)
	capacity := strings.Contains(m, "insufficient") || strings.Contains(m, "unschedulable") || strings.Contains(m, "no node") || strings.Contains(m, "capacity")
	pull := strings.Contains(m, "imagepull") || strings.Contains(m, "errimage") || strings.Contains(m, "pulling image") || strings.Contains(m, "back-off pulling")
	switch status {
	case "pending":
		switch {
		case capacity:
			return "Waiting for a machine with room to start the builder."
		case pull:
			return "Fetching the builder's environment."
		default:
			return "Preparing the builder's environment."
		}
	case "failed":
		switch {
		case capacity:
			return "The builder could not start because no machine has room right now. Try again shortly."
		case pull:
			return "The builder's environment could not be fetched. This is a platform problem, not yours: try again shortly."
		case strings.Contains(m, "oomkilled"):
			return "The builder ran out of memory and stopped."
		default:
			return "The builder's environment failed to start."
		}
	case "missing":
		return "The builder is not running."
	}
	return ""
}
