// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// sessionAllowedRoutes is the complete list of API routes a Teepin Build
// session-scoped credential (MintSessionToken) may call. Everything else is
// refused with 403 before any handler runs.
//
// Why an allowlist: the agent pod holds this token in its environment and
// runs a terminal tool, so anything a prompt-injected agent can read it can
// also replay directly against the API, without going through the MCP tools
// that carry the "customer approved the plan" check. Most Teepin Build handlers
// only require "a project and an account" (Server.requireScope), which a
// session token satisfies — so without this, the agent could approve its own
// deploys, raise its own budget, or create sessions and instances at will.
// Default-deny means a route added later is closed to agents until someone
// decides, here, that it should be open.
//
// Keys are gin route patterns ("METHOD /path/:param"), matched against
// gin.Context.FullPath(), so :id parameters are compared as patterns, not as
// concrete values. Handlers that take a session id in the path must still
// verify it equals the token's own SessionID (see sessionScopedOnly in
// pkg/api) — this list only decides which routes are reachable at all.
//
// One token type serves three callers, and the list is their union:
//   - the agent (LLM gateway, follow-up polling, workspace upload, and the
//     teepin-mcp-server's pricing / create_instance / deploy / status calls),
//   - the screenshot pod (screenshot upload),
//   - the Kaniko build init container (workspace archive fetch).
var sessionAllowedRoutes = map[string]bool{
	// LLM gateway — the agent's model calls.
	"POST /v1/build/chat/completions": true,

	// Session status and follow-up messages (run.py's poll loop; the MCP
	// server's deploy_approved read).
	"GET /v1/build/sessions/:id":               true,
	"GET /v1/build/sessions/:id/messages/poll": true,

	// Workspace: agent upload, and the archive fetch the build init
	// container and screenshot flow use.
	"PUT /v1/build/sessions/:id/workspace":         true,
	"GET /v1/build/sessions/:id/workspace/archive": true,

	// Screenshot pod upload.
	"POST /v1/build/sessions/:id/screenshot": true,

	// The agent records each deployment plan it presents, so the customer's
	// approval can name it. (Approving is the customer's, and not here.)
	"POST /v1/build/sessions/:id/plans": true,

	// The agent has the platform's image reader describe an image the customer
	// attached, because the builder model cannot see images. Billed to the build.
	"POST /v1/build/sessions/:id/describe-image": true,

	// teepin-mcp-server verbs. deploy and create_instance are additionally
	// gated server-side on the session's deploy_approved flag.
	"POST /v1/build/sessions/:id/deploy": true,
	"GET /v1/billing/pricing":            true,
	"POST /v1/compute/instances":         true,
	"GET /v1/compute/instances/:id":      true,
}

// SessionMayCall reports whether a session-scoped credential is allowed to
// call the route with this method and gin route pattern. An empty pattern —
// gin found no matching route — is refused: there is nothing to allow.
func SessionMayCall(method, routePattern string) bool {
	if routePattern == "" {
		return false
	}
	// "/v1/kumbha/" is Teepin Build's former route prefix, still served until
	// every running agent image calls "/v1/build/" (see cmd/api-server).
	if strings.HasPrefix(routePattern, "/v1/kumbha/") {
		routePattern = "/v1/build/" + strings.TrimPrefix(routePattern, "/v1/kumbha/")
	}
	return sessionAllowedRoutes[method+" "+routePattern]
}

// enforceSessionRoutes returns true when the request may proceed. For any
// credential that is not session-scoped it is a no-op; for a session token
// it aborts with 403 unless the route is on the allowlist.
func enforceSessionRoutes(c *gin.Context, p *Principal) bool {
	if p.SessionID == uuid.Nil || SessionMayCall(c.Request.Method, c.FullPath()) {
		return true
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"error": "this credential is not permitted to call this endpoint",
	})
	return false
}
