// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// teepin-mcp-server is the Kumbha agent's ONLY tool surface — a thin MCP
// wrapper mapping each teepin.* verb onto the exact same internal API a
// human uses from the console, never a second implementation with its own
// logic to keep in sync (KUMBHA-DESIGN.md: "Kumbha has no console page of
// its own" / "the CloudFormation model"). It is launched by the OpenHands
// wrapper (deploy/kumbha-agent/run.py) as a stdio MCP server and holds the
// agent's own short-lived, session-scoped credential — never a
// platform-wide token.
//
// create_instance, deploy, and attach_domain are HARD-gated on the
// session's deploy_approved flag, checked server-side (against the
// control plane, not trusted from the agent process) before any real
// provisioning call — a prompt-injected agent cannot talk its way past
// this the way it could a prompt instruction. present_deployment_plan is
// deliberately NOT gated: it is how the agent shows the customer what
// approval would cost, so it must work before approval exists.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	client, err := newTeepinClient()
	if err != nil {
		log.Fatalf("teepin-mcp-server: %v", err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "teepin", Version: "0.1.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "present_deployment_plan",
		Description: "Show the customer an itemised infrastructure cost estimate " +
			"(Resource / Sized / Cost per hour and per month) and wait for their " +
			"approval before calling create_instance, deploy, or attach_domain. " +
			"Always call this BEFORE attempting to provision anything real.",
	}, client.presentDeploymentPlan)

	mcp.AddTool(server, &mcp.Tool{
		Name: "create_instance",
		Description: "Create a running Teepin compute instance from a container image. " +
			"Requires the customer to have approved the deployment plan first — call " +
			"present_deployment_plan and wait if you have not done that yet.",
	}, client.createInstance)

	mcp.AddTool(server, &mcp.Tool{
		Name: "deploy",
		Description: "Build the current workspace into a container image and run it as " +
			"a Teepin instance. Requires the customer to have approved the deployment " +
			"plan first.",
	}, client.deploy)

	mcp.AddTool(server, &mcp.Tool{
		Name: "attach_domain",
		Description: "Report the public endpoint for a Teepin instance. Requires the " +
			"customer to have approved the deployment plan first.",
	}, client.attachDomain)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("teepin-mcp-server: %v", err)
	}
}

// teepinClient calls the control plane's REAL customer-facing APIs — the
// exact same endpoints a human hits from the console — authenticated with
// the agent's own session-scoped token.
type teepinClient struct {
	baseURL   string
	token     string
	sessionID string
	http      *http.Client
}

func newTeepinClient() (*teepinClient, error) {
	baseURL := os.Getenv("TEEPIN_API_BASE_URL")
	token := os.Getenv("TEEPIN_SESSION_TOKEN")
	sessionID := os.Getenv("TEEPIN_SESSION_ID")
	if baseURL == "" || token == "" || sessionID == "" {
		return nil, fmt.Errorf("TEEPIN_API_BASE_URL, TEEPIN_SESSION_TOKEN and TEEPIN_SESSION_ID are all required")
	}
	return &teepinClient{
		baseURL:   baseURL,
		token:     token,
		sessionID: sessionID,
		http:      &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// doJSON calls the control plane and decodes a JSON response. body may be
// nil (GET); out may be nil (no response body expected).
func (c *teepinClient) doJSON(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, string(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// deployApproved checks the session's pre-deploy cost-approval gate —
// server-side, against the control plane, never trusted from the agent
// process itself. See KUMBHA-DESIGN.md's "Pre-deploy cost approval".
func (c *teepinClient) deployApproved(ctx context.Context) (bool, error) {
	var sess struct {
		DeployApproved bool `json:"deploy_approved"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/kumbha/sessions/"+c.sessionID, nil, &sess); err != nil {
		return false, err
	}
	return sess.DeployApproved, nil
}

// textResult is the common shape every tool below returns: one text block
// with either a real result or an explanatory message — never a Go error
// for "not approved yet" or "not available yet", since those are things
// the agent should read and act on (wait, or try something else), not
// treatable as a broken tool call.
func textResult(format string, args ...any) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}, nil, nil
}

const notApprovedMessage = "The customer has not approved the deployment plan yet. " +
	"Call present_deployment_plan with your resource requirements, then wait — " +
	"do not retry this tool until the customer has approved."

// --- present_deployment_plan ---

type resourceRequest struct {
	Name      string `json:"name" jsonschema:"a short label for this resource, e.g. the app's name"`
	CPUUnits  int    `json:"cpu_units" jsonschema:"vCPU cores"`
	MemoryGB  int    `json:"memory_gb"`
	StorageGB int    `json:"storage_gb,omitempty" jsonschema:"persistent volume size; 0 for none"`
}

type presentDeploymentPlanArgs struct {
	Resources []resourceRequest `json:"resources" jsonschema:"every compute resource this build will need"`
}

// hoursPerMonth mirrors pkg/billing/collector.go's own constant — the
// same 730-hour conversion the platform's actual billing uses, so the
// estimate the customer approves here matches what they are later
// charged, not a second formula that could drift from it.
const hoursPerMonth = 730.0

func (c *teepinClient) presentDeploymentPlan(ctx context.Context, req *mcp.CallToolRequest, args presentDeploymentPlanArgs) (*mcp.CallToolResult, any, error) {
	if len(args.Resources) == 0 {
		return textResult("at least one resource is required")
	}

	// The real, live admin-configured rates — GET /v1/billing/pricing is
	// the same numbers a human sees on their invoice, never a separate
	// estimate formula (KUMBHA-DESIGN.md: "using the real billing.pricing
	// rates — same numbers a human sees").
	var pricing struct {
		CPUPricePerCoreHour    float64 `json:"cpu_price_per_core_hour"`
		MemoryPricePerGBHour   float64 `json:"memory_price_per_gb_hour"`
		StoragePricePerGBMonth float64 `json:"storage_price_per_gb_month"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/billing/pricing", nil, &pricing); err != nil {
		return nil, nil, fmt.Errorf("failed to read live pricing: %w", err)
	}

	type lineOut struct {
		Name         string  `json:"name"`
		CPUUnits     int     `json:"cpu_units"`
		MemoryGB     int     `json:"memory_gb"`
		StorageGB    int     `json:"storage_gb"`
		CostPerHour  float64 `json:"cost_per_hour"`
		CostPerMonth float64 `json:"cost_per_month"`
	}
	plan := struct {
		Resources         []lineOut `json:"resources"`
		TotalCostPerHour  float64   `json:"total_cost_per_hour"`
		TotalCostPerMonth float64   `json:"total_cost_per_month"`
	}{}

	for _, r := range args.Resources {
		hourly := float64(r.CPUUnits)*pricing.CPUPricePerCoreHour + float64(r.MemoryGB)*pricing.MemoryPricePerGBHour
		if r.StorageGB > 0 {
			hourly += float64(r.StorageGB) * pricing.StoragePricePerGBMonth / hoursPerMonth
		}
		plan.Resources = append(plan.Resources, lineOut{
			Name: r.Name, CPUUnits: r.CPUUnits, MemoryGB: r.MemoryGB, StorageGB: r.StorageGB,
			CostPerHour: hourly, CostPerMonth: hourly * hoursPerMonth,
		})
		plan.TotalCostPerHour += hourly
		plan.TotalCostPerMonth += hourly * hoursPerMonth
	}

	// The response is JSON, not prose: the console's event handler
	// special-cases an observation from this tool to render the
	// Deployment Plan table/modal rather than a plain timeline entry —
	// see the plan's M5. This tool result still flows through the normal
	// OpenHands event pipeline (an MCP call is an ordinary ActionEvent/
	// ObservationEvent pair to the SDK), so no separate plumbing is
	// needed to get it to the console.
	body, err := json.Marshal(plan)
	if err != nil {
		return nil, nil, fmt.Errorf("encode deployment plan: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil, nil
}

// --- create_instance ---

type portArg struct {
	Container int    `json:"container"`
	Protocol  string `json:"protocol,omitempty" jsonschema:"tcp or udp; defaults to tcp"`
}

type createInstanceArgs struct {
	Name      string            `json:"name"`
	Image     string            `json:"image" jsonschema:"a container image reference already pushed somewhere pullable"`
	CPUUnits  int               `json:"cpu_units"`
	MemoryGB  int               `json:"memory_gb"`
	StorageGB int               `json:"storage_gb,omitempty"`
	Ports     []portArg         `json:"ports,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Command   []string          `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
}

func (c *teepinClient) createInstance(ctx context.Context, req *mcp.CallToolRequest, args createInstanceArgs) (*mcp.CallToolResult, any, error) {
	approved, err := c.deployApproved(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to check deployment approval: %w", err)
	}
	if !approved {
		return textResult(notApprovedMessage)
	}

	instance, err := c.provisionInstance(ctx, args.Name, args.Image, args.CPUUnits, args.MemoryGB, args.StorageGB, args.Ports, args.Env, args.Command, args.Args)
	if err != nil {
		return nil, nil, err
	}
	return textResult("Created instance %s (status: %s). Endpoint: %s. Cost: $%.4f/hour.",
		instance.ID, instance.Status, instance.Endpoint, instance.Price)
}

// provisionInstance is the shared implementation behind create_instance and
// deploy's final step — both end at the exact same real customer-facing
// API, POST /v1/compute/instances, authenticated with the session token
// (the CloudFormation parallel: the resulting instance belongs to the
// customer's project like any other, billed the same way, manageable from
// the console with zero dependency on this session afterward).
func (c *teepinClient) provisionInstance(ctx context.Context, name, image string, cpuUnits, memoryGB, storageGB int, ports []portArg, env map[string]string, command, args []string) (*provisionedInstance, error) {
	body := map[string]any{
		"name":       name,
		"image":      image,
		"cpu_units":  cpuUnits,
		"memory":     fmt.Sprintf("%dGB", memoryGB),
		"storage_gb": storageGB,
		"env":        env,
		"command":    command,
		"args":       args,
	}
	if len(ports) > 0 {
		portsOut := make([]map[string]any, len(ports))
		for i, p := range ports {
			portsOut[i] = map[string]any{"container": p.Container, "protocol": p.Protocol}
		}
		body["ports"] = portsOut
	}

	var instance provisionedInstance
	if err := c.doJSON(ctx, http.MethodPost, "/v1/compute/instances", body, &instance); err != nil {
		return nil, fmt.Errorf("failed to create instance: %w", err)
	}
	return &instance, nil
}

type provisionedInstance struct {
	ID       string  `json:"id"`
	Status   string  `json:"status"`
	Endpoint string  `json:"endpoint"`
	Price    float64 `json:"price_per_hour"`
}

// --- deploy ---

type deployArgs struct {
	Name           string            `json:"name"`
	DockerfilePath string            `json:"dockerfile_path,omitempty" jsonschema:"path to the Dockerfile within the workspace; defaults to Dockerfile"`
	CPUUnits       int               `json:"cpu_units"`
	MemoryGB       int               `json:"memory_gb"`
	StorageGB      int               `json:"storage_gb,omitempty"`
	Ports          []portArg         `json:"ports,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
}

// deploy builds the current workspace into an image (Kaniko, via the
// control plane — see /v1/kumbha/sessions/:id/build) and runs it as a
// real Teepin instance. If no build pipeline is configured on this
// deployment, the control plane's own 404 for that endpoint surfaces
// here as a clear "not available" message rather than a confusing raw
// error — the honest-degradation behaviour this verb had before the
// build pipeline existed, now reached only when the platform genuinely
// lacks one.
func (c *teepinClient) deploy(ctx context.Context, req *mcp.CallToolRequest, args deployArgs) (*mcp.CallToolResult, any, error) {
	approved, err := c.deployApproved(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to check deployment approval: %w", err)
	}
	if !approved {
		return textResult(notApprovedMessage)
	}

	dockerfilePath := args.DockerfilePath
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}

	var built struct {
		ImageRef string `json:"image_ref"`
	}
	buildErr := c.doJSON(ctx, http.MethodPost, "/v1/kumbha/sessions/"+c.sessionID+"/build",
		map[string]any{"dockerfile_path": dockerfilePath}, &built)
	if buildErr != nil {
		if isNotAvailable(buildErr) {
			return textResult("Building the workspace into a running instance is not available on " +
				"this deployment (no source-to-image build pipeline is configured). If an image for " +
				"this app already exists in a registry, use create_instance with that image instead.")
		}
		return textResult("The build failed: %s", buildErr.Error())
	}

	instance, err := c.provisionInstance(ctx, args.Name, built.ImageRef, args.CPUUnits, args.MemoryGB, args.StorageGB, args.Ports, args.Env, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	return textResult("Built %s and created instance %s (status: %s). Endpoint: %s. Cost: $%.4f/hour.",
		built.ImageRef, instance.ID, instance.Status, instance.Endpoint, instance.Price)
}

// isNotAvailable reports whether an error from doJSON came from a 404 —
// the shape the control plane returns when a capability (like the build
// pipeline) simply isn't configured on this deployment, as opposed to a
// genuine build failure that should be surfaced verbatim.
func isNotAvailable(err error) bool {
	return strings.Contains(err.Error(), "status 404")
}

// --- attach_domain ---

type attachDomainArgs struct {
	InstanceID string `json:"instance_id"`
}

// attachDomain is an HONEST STUB: every instance already gets a
// `https://<instance-id>.<platform-domain>` endpoint automatically at
// creation (create_instance's response reports it) — a customer-chosen
// subdomain name and full bring-your-own-domain TLS are both real,
// separate, unbuilt work (KUMBHA-DESIGN.md's "Custom domains" section),
// not something this tool can do yet.
func (c *teepinClient) attachDomain(ctx context.Context, req *mcp.CallToolRequest, args attachDomainArgs) (*mcp.CallToolResult, any, error) {
	approved, err := c.deployApproved(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to check deployment approval: %w", err)
	}
	if !approved {
		return textResult(notApprovedMessage)
	}

	var instance struct {
		Endpoint string `json:"endpoint"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/compute/instances/"+args.InstanceID, nil, &instance); err != nil {
		return nil, nil, fmt.Errorf("failed to look up instance: %w", err)
	}
	return textResult("Instance %s is already reachable at %s (assigned automatically at creation). "+
		"Custom domain names are not available on this deployment yet.", args.InstanceID, instance.Endpoint)
}
