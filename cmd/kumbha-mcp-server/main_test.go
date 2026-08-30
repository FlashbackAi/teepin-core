// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestClient points a teepinClient at a local mux instead of the real
// control plane, mirroring the vLLM/Anthropic provider tests' own
// newTest* helpers in pkg/inference.
func newTestClient(t *testing.T, mux *http.ServeMux) *teepinClient {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &teepinClient{
		baseURL:   srv.URL,
		token:     "test-session-token",
		sessionID: "sess-test",
		http:      srv.Client(),
		slowHTTP:  srv.Client(),
	}
}

func TestPresentDeploymentPlan_ComputesCostFromLiveRates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/billing/pricing", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-session-token" {
			t.Errorf("Authorization header = %q, want the session token as a bearer credential", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"cpu_price_per_core_hour":1.25,"memory_price_per_gb_hour":0.60,"storage_price_per_gb_month":0.10}`))
	})
	c := newTestClient(t, mux)

	result, _, err := c.presentDeploymentPlan(context.Background(), &mcp.CallToolRequest{}, presentDeploymentPlanArgs{
		Resources: []resourceRequest{
			{Name: "web", CPUUnits: 2, MemoryGB: 4, StorageGB: 0},
		},
	})
	if err != nil {
		t.Fatalf("presentDeploymentPlan: %v", err)
	}

	text := result.Content[0].(*mcp.TextContent).Text
	var plan struct {
		Resources []struct {
			Name        string  `json:"name"`
			CostPerHour float64 `json:"cost_per_hour"`
		} `json:"resources"`
		TotalCostPerHour float64 `json:"total_cost_per_hour"`
	}
	if err := json.Unmarshal([]byte(text), &plan); err != nil {
		t.Fatalf("result is not the expected JSON shape: %v\ngot: %s", err, text)
	}

	// 2 cores * $1.25 + 4GB * $0.60 = $2.50 + $2.40 = $4.90/hr
	want := 2*1.25 + 4*0.60
	if plan.Resources[0].CostPerHour != want {
		t.Errorf("cost_per_hour = %v, want %v", plan.Resources[0].CostPerHour, want)
	}
	if plan.TotalCostPerHour != want {
		t.Errorf("total_cost_per_hour = %v, want %v", plan.TotalCostPerHour, want)
	}
}

func TestPresentDeploymentPlan_IncludesStorageConvertedFromGBMonth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/billing/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"cpu_price_per_core_hour":0,"memory_price_per_gb_hour":0,"storage_price_per_gb_month":0.10}`))
	})
	c := newTestClient(t, mux)

	result, _, err := c.presentDeploymentPlan(context.Background(), &mcp.CallToolRequest{}, presentDeploymentPlanArgs{
		Resources: []resourceRequest{{Name: "db", StorageGB: 100}},
	})
	if err != nil {
		t.Fatalf("presentDeploymentPlan: %v", err)
	}

	var plan struct {
		TotalCostPerHour float64 `json:"total_cost_per_hour"`
	}
	json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &plan)

	// The same hoursPerMonth conversion pkg/billing/collector.go uses —
	// this must match what the customer is actually billed, not a second
	// formula that could drift from it.
	want := 100 * 0.10 / hoursPerMonth
	if plan.TotalCostPerHour < want*0.9999 || plan.TotalCostPerHour > want*1.0001 {
		t.Errorf("total_cost_per_hour = %v, want ~%v", plan.TotalCostPerHour, want)
	}
}

func TestPresentDeploymentPlan_RejectsEmptyResources(t *testing.T) {
	c := newTestClient(t, http.NewServeMux())
	result, _, err := c.presentDeploymentPlan(context.Background(), &mcp.CallToolRequest{}, presentDeploymentPlanArgs{})
	if err != nil {
		t.Fatalf("presentDeploymentPlan: %v", err)
	}
	if !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "at least one resource") {
		t.Errorf("got %q, want a message about needing at least one resource", result.Content[0].(*mcp.TextContent).Text)
	}
}

// A malformed resource (all zero — the shape a wrong-field-name call
// silently collapses to, since json.Unmarshal never rejects an unknown or
// missing field) must be rejected with an actionable message, not priced
// at $0.00 and reported as a success. Found live 2026-08-24: an agent sent
// resources shaped like {"resource":"Compute instance","spec":"...",
// "cost_per_hour":"$0.015"} — none of those keys exist on resourceRequest,
// so every resource silently priced at $0 and "succeeded" six times in a
// row before the agent gave up trying different shapes.
func TestPresentDeploymentPlan_RejectsAllZeroResource(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/billing/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"cpu_price_per_core_hour":1,"memory_price_per_gb_hour":1,"storage_price_per_gb_month":1}`))
	})
	c := newTestClient(t, mux)

	result, _, err := c.presentDeploymentPlan(context.Background(), &mcp.CallToolRequest{}, presentDeploymentPlanArgs{
		Resources: []resourceRequest{{Name: "pomodoro-instance"}},
	})
	if err != nil {
		t.Fatalf("presentDeploymentPlan: %v", err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "pomodoro-instance") || !strings.Contains(text, "cpu_units") {
		t.Errorf("got %q, want a rejection naming the resource and the required fields", text)
	}
}

// A storage-only resource (no CPU/memory — a legitimate line item, e.g. a
// standalone volume) must NOT be rejected by the all-zero check above: it
// has a real, non-zero storage_gb, just zero compute.
func TestPresentDeploymentPlan_AllowsStorageOnlyResource(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/billing/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"cpu_price_per_core_hour":1,"memory_price_per_gb_hour":1,"storage_price_per_gb_month":1}`))
	})
	c := newTestClient(t, mux)

	result, _, err := c.presentDeploymentPlan(context.Background(), &mcp.CallToolRequest{}, presentDeploymentPlanArgs{
		Resources: []resourceRequest{{Name: "db", StorageGB: 10}},
	})
	if err != nil {
		t.Fatalf("presentDeploymentPlan: %v", err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, "must specify") {
		t.Errorf("storage-only resource was rejected: %s", text)
	}
}

func TestCreateInstance_BlockedBeforeApproval(t *testing.T) {
	mux := http.NewServeMux()
	called := false
	mux.HandleFunc("/v1/kumbha/sessions/sess-test", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"deploy_approved":false}`))
	})
	mux.HandleFunc("/v1/compute/instances", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{}`))
	})
	c := newTestClient(t, mux)

	result, _, err := c.createInstance(context.Background(), &mcp.CallToolRequest{}, createInstanceArgs{
		Name: "app", Image: "nginx:latest", CPUUnits: 1, MemoryGB: 1,
	})
	if err != nil {
		t.Fatalf("createInstance: %v", err)
	}
	if called {
		t.Error("POST /v1/compute/instances was called despite the session not being approved")
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "present_deployment_plan") {
		t.Errorf("got %q, want guidance to call present_deployment_plan", text)
	}
}

func TestCreateInstance_ProceedsAfterApproval(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/kumbha/sessions/sess-test", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"deploy_approved":true}`))
	})
	mux.HandleFunc("/v1/compute/instances", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-session-token" {
			t.Errorf("Authorization = %q, want the session token — this IS the real customer-facing API call", got)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["image"] != "nginx:latest" {
			t.Errorf("image = %v, want nginx:latest", body["image"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"inst-abc123","status":"running","endpoint":"https://inst-abc123.teepin.com","price_per_hour":0.20}`))
	})
	c := newTestClient(t, mux)

	result, _, err := c.createInstance(context.Background(), &mcp.CallToolRequest{}, createInstanceArgs{
		Name: "app", Image: "nginx:latest", CPUUnits: 1, MemoryGB: 1,
	})
	if err != nil {
		t.Fatalf("createInstance: %v", err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "inst-abc123") || !strings.Contains(text, "https://inst-abc123.teepin.com") {
		t.Errorf("got %q, want the created instance's id and endpoint", text)
	}
}

func TestDeploy_HonestStubStillChecksApproval(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/kumbha/sessions/sess-test", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"deploy_approved":false}`))
	})
	c := newTestClient(t, mux)

	result, _, err := c.deploy(context.Background(), &mcp.CallToolRequest{}, deployArgs{Name: "app"})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "present_deployment_plan") {
		t.Error("deploy did not enforce the approval gate before falling through to its stub message")
	}
}

// When the control plane has no build pipeline configured, POST .../build
// 404s (see BuildKumbhaSession's own s.kumbhaBuild == nil check) — deploy
// must translate that into an honest message, not a raw HTTP error.
//
// The message must also read as a DEAD END rather than a hint, because an
// agent treats a suggested alternative as something to go hunt for: found
// live 2026-08-25, the earlier "...use create_instance with that image
// instead" phrasing sent one build into a dozen-turn, thousands-of-tokens
// search for a workaround that does not exist. These assertions pin the
// properties that actually stop that search, not the exact prose.
func TestDeploy_Approved_NoBuildPipelineConfiguredIsADeadEndNotAHint(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/kumbha/sessions/sess-test", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"deploy_approved":true}`))
	})
	// No handler registered for .../build — ServeMux answers 404, exactly
	// what the real control plane returns when kumbhaBuild is nil.
	c := newTestClient(t, mux)

	result, _, err := c.deploy(context.Background(), &mcp.CallToolRequest{}, deployArgs{Name: "app"})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	text := result.Content[0].(*mcp.TextContent).Text

	if !strings.Contains(text, "NOT POSSIBLE") {
		t.Errorf("got %q, want an unambiguous statement that deployment cannot happen here", text)
	}
	// The specific instruction that ends the loop rather than inviting one.
	if !strings.Contains(text, "Do NOT try to work around this") {
		t.Errorf("got %q, want an explicit instruction not to search for a workaround", text)
	}
	if !strings.Contains(text, "Tell the customer") {
		t.Errorf("got %q, want the agent told what to do instead (report back, not retry)", text)
	}
}

// TestDeploy_Approved_BuildsAndCreatesInstance covers the unified path:
// deploy now calls the SAME /v1/kumbha/sessions/:id/deploy endpoint the
// console IDE's own Deploy button calls (build + create in one control-
// plane round trip), not a separate build-then-create-instance sequence
// of its own — see deploy's own doc comment on why that convergence is
// what makes app_instance_id tracking (migration 026) actually hold
// regardless of which side triggered the deploy.
func TestDeploy_Approved_BuildsAndCreatesInstance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/kumbha/sessions/sess-test", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"deploy_approved":true}`))
	})
	mux.HandleFunc("/v1/kumbha/sessions/sess-test/deploy", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["dockerfile_path"] != "Dockerfile" {
			t.Errorf("dockerfile_path = %v, want the default Dockerfile", body["dockerfile_path"])
		}
		if body["name"] != "app" {
			t.Errorf("name = %v, want app", body["name"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image_ref":"registry.teepin.cloud/teepin-app-abc123:sess-tes","instance_id":"inst-xyz789","status":"running","endpoint":"https://inst-xyz789.teepin.com","price_per_hour":0.20}`))
	})
	c := newTestClient(t, mux)

	result, _, err := c.deploy(context.Background(), &mcp.CallToolRequest{}, deployArgs{Name: "app", CPUUnits: 1, MemoryGB: 1})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "inst-xyz789") || !strings.Contains(text, "teepin-app-abc123") {
		t.Errorf("got %q, want the built image ref and the created instance id", text)
	}
}

// TestDeploy_UsesSlowHTTPClientNotTheFastOnesShortTimeout is the
// regression test for a real live bug (found 2026-08-30): deploy
// triggers a full Kaniko build server-side before the control plane even
// responds (up to build.DefaultConfig().Timeout, 15 minutes), but this
// tool's HTTP call was sharing the SAME client every fast, non-building
// tool uses (a 30s ceiling) — any real build taking longer than that (the
// ordinary case) tripped the CLIENT's own timeout and reported a false
// "deploy failed" while the build was often still genuinely in progress
// server-side. Proven here by handing the client a fast HTTP client whose
// timeout is far shorter than the handler's own delay, and a separate
// slow one with none — deploy must succeed via the slow one regardless.
func TestDeploy_UsesSlowHTTPClientNotTheFastOnesShortTimeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/kumbha/sessions/sess-test", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"deploy_approved":true}`))
	})
	mux.HandleFunc("/v1/kumbha/sessions/sess-test/deploy", func(w http.ResponseWriter, r *http.Request) {
		// Simulate a slow-but-real Kaniko build: response headers don't
		// arrive until after the FAST client's own timeout would already
		// have expired.
		time.Sleep(60 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image_ref":"img:v1","instance_id":"inst-slow1","status":"running","endpoint":"https://inst-slow1.teepin.com","price_per_hour":0.1}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &teepinClient{
		baseURL:   srv.URL,
		token:     "test-session-token",
		sessionID: "sess-test",
		http:      &http.Client{Timeout: 20 * time.Millisecond}, // deliberately shorter than the handler's delay
		slowHTTP:  srv.Client(),                                 // no artificial ceiling, like deployHTTPTimeout in production
	}

	result, _, err := c.deploy(context.Background(), &mcp.CallToolRequest{}, deployArgs{Name: "app"})
	if err != nil {
		t.Fatalf("deploy: %v — want it to succeed via slowHTTP even though the fast client's own timeout is far shorter than the handler's delay", err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "inst-slow1") {
		t.Errorf("got %q, want the created instance id", text)
	}
}

func TestDeploy_Approved_BuildFailureIsReportedNotSwallowed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/kumbha/sessions/sess-test", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"deploy_approved":true}`))
	})
	mux.HandleFunc("/v1/kumbha/sessions/sess-test/deploy", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"build failed: exit code 1: Error","log":"COPY failed: file not found"}`, http.StatusUnprocessableEntity)
	})
	c := newTestClient(t, mux)

	result, _, err := c.deploy(context.Background(), &mcp.CallToolRequest{}, deployArgs{Name: "app"})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "build failed") {
		t.Errorf("got %q, want the build failure reason surfaced to the agent", text)
	}
}
