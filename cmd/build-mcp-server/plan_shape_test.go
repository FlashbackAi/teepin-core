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
// The call it is shown describes the app at the top level, the shape GLM can send.
func TestPresentDeploymentPlan_EmptyResourcesRefusalShowsTheShape(t *testing.T) {
	text, _ := callPlan(t, map[string]any{"resources": []any{}, "verification": "x"})
	for _, want := range []string{"at least one resource is required", "top level", "fields: resources, verification", "cpu_units", `"name": "guestbook"`} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal %q does not mention %q", text, want)
		}
	}
	if strings.Contains(planExample, `"resources"`) {
		t.Errorf("the example still shows a resources list, the shape GLM loses: %s", planExample)
	}
}

// The exact shape a GLM build sends: the app at the top level and an empty
// "resources" list. It must be accepted by the schema and priced.
func TestPresentDeploymentPlan_TopLevelAppPassesTheSchemaAndIsPriced(t *testing.T) {
	c := newTestClient(t, planPricingMux())
	server := mcp.NewServer(&mcp.Implementation{Name: "teepin", Version: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "present_deployment_plan", Description: "plan"}, c.presentDeploymentPlan)
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "present_deployment_plan", Arguments: map[string]any{
		"name": "task-monitor", "cpu_units": 1, "memory_gb": 1, "resources": []any{}, "verification": testVerification,
	}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !isPricedPlan(text) || !strings.Contains(text, `"name":"task-monitor"`) || !strings.Contains(text, "cost_per_hour") {
		t.Fatalf("a top-level app was not priced: %s", text)
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
