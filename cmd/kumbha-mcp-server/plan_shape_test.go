// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callPlan sends arguments to present_deployment_plan through a real MCP session,
// the way the agent's model does, and returns the text it reads back.
func callPlan(t *testing.T, args map[string]any) (string, bool) {
	t.Helper()
	c := newTestClient(t, http.NewServeMux())
	server := mcp.NewServer(&mcp.Implementation{Name: "teepin", Version: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "present_deployment_plan", Description: "plan"}, c.presentDeploymentPlan)

	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "present_deployment_plan", Arguments: args})
	if err != nil {
		return "protocol error: " + err.Error(), true
	}
	var b strings.Builder
	for _, content := range res.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

// A model that sends an empty "resources" array (valid to the schema, useless to
// the plan) is told what it sent and shown a correct call. A live GLM-5.3 build
// got a bare "at least one resource is required" fifteen times and never fixed it.
func TestPresentDeploymentPlan_EmptyResourcesRefusalShowsTheShape(t *testing.T) {
	text, _ := callPlan(t, map[string]any{"resources": []any{}, "verification": "x"})
	for _, want := range []string{"at least one resource is required", "non-empty array", "fields: resources, verification", "cpu_units", `"name": "guestbook"`} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal %q does not mention %q", text, want)
		}
	}
}

// The MCP layer rejects other wrong shapes before the handler runs; this pins
// that they are still errors (not silently accepted) so the example in the tool
// description is what teaches the shape.
func TestPresentDeploymentPlan_WrongShapesAreRejectedByTheSchema(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"unknown field": {"resource_list": []any{}, "resources": []any{}},
		"missing":       {},
		"string array":  {"resources": `[{"name":"a","cpu_units":1}]`},
	} {
		text, _ := callPlan(t, args)
		if strings.Contains(text, "Total") || strings.Contains(text, "cost_per_hour") {
			t.Errorf("%s: a malformed call produced a plan: %q", name, text)
		}
	}
}
