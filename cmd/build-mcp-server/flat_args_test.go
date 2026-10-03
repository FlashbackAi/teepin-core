// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// GLM-5.3's tool calls arrive with every list of objects empty, while plain
// text and numbers come through (2026-10-02). These tests pin that every tool
// that took a list of objects can now be called with plain top-level fields,
// and that the older list forms still work for a model that fills them.

func TestPlanResources_TopLevelAppComesFirstThenTheList(t *testing.T) {
	got := presentDeploymentPlanArgs{
		Name: "web", CPUUnits: 1, MemoryGB: 2,
		Resources: []resourceRequest{{Name: "db", StorageGB: 10}},
	}.allResources()
	if len(got) != 2 || got[0] != (resourceRequest{Name: "web", CPUUnits: 1, MemoryGB: 2}) || got[1].Name != "db" {
		t.Fatalf("resources = %+v", got)
	}
	// Sizes without a name still describe the app.
	if got := (presentDeploymentPlanArgs{CPUUnits: 1}).allResources(); len(got) != 1 || got[0].Name != "app" {
		t.Errorf("unnamed app = %+v", got)
	}
	// Nothing at the top level and an empty list is nothing at all.
	if got := (presentDeploymentPlanArgs{Resources: []resourceRequest{}}).allResources(); len(got) != 0 {
		t.Errorf("empty = %+v", got)
	}
}

func TestPresentDeploymentPlan_TopLevelAppIsPricedLikeAListedOne(t *testing.T) {
	c := newTestClient(t, planPricingMux())
	flat := presentText(t, c, presentDeploymentPlanArgs{Name: "web", CPUUnits: 2, MemoryGB: 1, Verification: testVerification})
	listed := presentText(t, c, presentDeploymentPlanArgs{
		Resources: []resourceRequest{{Name: "web", CPUUnits: 2, MemoryGB: 1}}, Verification: testVerification,
	})
	type plan struct {
		Resources []struct {
			Name        string  `json:"name"`
			CostPerHour float64 `json:"cost_per_hour"`
		} `json:"resources"`
		Total float64 `json:"total_cost_per_hour"`
	}
	var a, b plan
	if json.Unmarshal([]byte(flat), &a) != nil || json.Unmarshal([]byte(listed), &b) != nil {
		t.Fatalf("not plans:\n%s\n%s", flat, listed)
	}
	if len(a.Resources) != 1 || a.Resources[0].Name != "web" || a.Total != b.Total || a.Total != 3 {
		t.Errorf("flat %+v, listed %+v", a, b)
	}
}

func TestAskUser_TopLevelOptionsMakeACard(t *testing.T) {
	text := askText(t, askUserArgs{
		Question: "How should I fill the table?",
		Option1:  "Sample data", Option1Description: "Invent realistic rows",
		Option2: "Totals only",
		// The empty list GLM sends alongside must not get in the way.
		Options:    []questionOption{},
		AllowOther: true,
	})
	var card struct {
		Kind    string           `json:"kind"`
		Options []questionOption `json:"options"`
	}
	if err := json.Unmarshal([]byte(text), &card); err != nil || card.Kind != "question" {
		t.Fatalf("not a card: %s", text)
	}
	if len(card.Options) != 2 || card.Options[0] != (questionOption{"Sample data", "Invent realistic rows"}) || card.Options[1].Label != "Totals only" {
		t.Errorf("options = %+v", card.Options)
	}
}

func TestAskUser_GapsAreSkippedAndTheTopLevelRulesStillApply(t *testing.T) {
	// option_1 and option_3 set: two answers, in order.
	if got := (askUserArgs{Option1: "A", Option3: "C"}).allOptions(); len(got) != 2 || got[1].Label != "C" {
		t.Errorf("options = %+v", got)
	}
	// One answer is still refused, and the refusal shows the top-level shape.
	text := askText(t, askUserArgs{Question: "q?", Option1: "only"})
	if isCard(text, "question") || !strings.Contains(text, "option_1") || !strings.Contains(text, "got 1") {
		t.Errorf("refusal = %s", text)
	}
	// Duplicates across the two forms are caught.
	if text := askText(t, askUserArgs{Question: "q?", Option1: "Same", Options: []questionOption{{Label: "same"}}}); isCard(text, "question") {
		t.Errorf("duplicate labels produced a card: %s", text)
	}
}

func TestAskUser_TheExampleUsesNoListOfObjects(t *testing.T) {
	if strings.Contains(askUserExample, `"options"`) {
		t.Errorf("the example still shows the list GLM loses: %s", askUserExample)
	}
}

func TestAllPorts(t *testing.T) {
	if got := allPorts(nil, 8080, ""); len(got) != 1 || got[0].Container != 8080 {
		t.Errorf("port only = %+v", got)
	}
	if got := allPorts([]portArg{{Container: 8080}}, 8080, "udp"); len(got) != 1 {
		t.Errorf("the same port twice = %+v", got)
	}
	if got := allPorts([]portArg{{Container: 9090}}, 8080, "udp"); len(got) != 2 || got[1] != (portArg{8080, "udp"}) {
		t.Errorf("both = %+v", got)
	}
	if got := allPorts(nil, 0, ""); got != nil {
		t.Errorf("none = %+v", got)
	}
}

func TestAllEnv(t *testing.T) {
	got, err := allEnv([]envVar{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}}, "B=3\n\n# a comment\n C = x=y \n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["A"] != "1" || got["B"] != "3" || got["C"] != " x=y" {
		t.Errorf("env = %q", got)
	}
	if got, err := allEnv(nil, ""); err != nil || got != nil {
		t.Errorf("empty = %v, %v (env is omitempty on both request bodies)", got, err)
	}
	for _, bad := range []string{"NOEQUALS", "=value"} {
		if _, err := allEnv(nil, bad); err == nil || !strings.Contains(err.Error(), "NAME=value") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

// What a GLM deploy sends (a plain port and env_vars, empty lists) reaches the
// deploy API as the same body a filled-in list would have produced.
func TestDeploy_TopLevelPortAndEnvReachTheAPI(t *testing.T) {
	var body map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/build/sessions/sess-test", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"deploy_approved":true}`))
	})
	mux.HandleFunc("/v1/build/sessions/sess-test/deploy", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"image_ref":"img:v1","instance_id":"inst-1","status":"running","endpoint":"https://inst-1.teepin.com","price_per_hour":0.1}`))
	})
	c := newTestClient(t, mux)
	_, _, err := c.deploy(context.Background(), &mcp.CallToolRequest{}, deployArgs{
		Name: "app", CPUUnits: 1, MemoryGB: 1, Port: 80, EnvVars: "MODE=live",
		Ports: []portArg{}, Env: []envVar{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ports, _ := body["ports"].([]any)
	if len(ports) != 1 || ports[0].(map[string]any)["container"] != float64(80) {
		t.Errorf("ports = %v", body["ports"])
	}
	if env, _ := body["env"].(map[string]any); env["MODE"] != "live" {
		t.Errorf("env = %v", body["env"])
	}
}

func TestDeploy_ABadEnvLineIsRefusedBeforeAnyBuild(t *testing.T) {
	built := false
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/build/sessions/sess-test", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"deploy_approved":true}`))
	})
	mux.HandleFunc("/v1/build/sessions/sess-test/deploy", func(w http.ResponseWriter, r *http.Request) { built = true })
	c := newTestClient(t, mux)
	res, _, err := c.deploy(context.Background(), &mcp.CallToolRequest{}, deployArgs{Name: "app", Port: 80, EnvVars: "oops"})
	if err != nil {
		t.Fatal(err)
	}
	if built || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "NAME=value") {
		t.Errorf("built=%v text=%s", built, res.Content[0].(*mcp.TextContent).Text)
	}
}

// The deploy schema accepts the top-level shape, so the agent framework's own
// validation lets it through to the handler.
func TestInputSchemaAcceptsTopLevelPortAndEnvVars(t *testing.T) {
	for _, schema := range []func() (any, error){
		func() (any, error) { return inputSchemaFor[deployArgs]() },
		func() (any, error) { return inputSchemaFor[createInstanceArgs]() },
	} {
		s, err := schema()
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(s)
		for _, field := range []string{`"port"`, `"protocol"`, `"env_vars"`} {
			if !strings.Contains(string(raw), field) {
				t.Errorf("schema lacks %s: %s", field, raw)
			}
		}
	}
	s, _ := inputSchemaFor[deployArgs]()
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	_ = json.Unmarshal([]byte(`{"name":"x","cpu_units":1,"memory_gb":1,"port":8080,"env_vars":"A=1","ports":[],"env":[]}`), &v)
	if err := rs.Validate(&v); err != nil {
		t.Fatalf("the GLM shape is rejected by the schema: %v", err)
	}
}
