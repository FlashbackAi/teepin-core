// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// deployedSessionMux serves a session with the given approval and instance,
// an instance of the given size, and (counting calls) the pricing endpoint.
func deployedSessionMux(approved bool, appInstanceID string, cpu int, memory string, storage int, pricingCalls *atomic.Int32) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/build/sessions/sess-test", func(w http.ResponseWriter, r *http.Request) {
		approvedJSON := "false"
		if approved {
			approvedJSON = "true"
		}
		w.Write([]byte(`{"deploy_approved":` + approvedJSON + `,"app_instance_id":"` + appInstanceID + `"}`))
	})
	mux.HandleFunc("/v1/compute/instances/inst-live", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"cpu_units":` + itoa(cpu) + `,"memory":"` + memory + `","storage_gb":` + itoa(storage) + `}`))
	})
	mux.HandleFunc("/v1/billing/pricing", func(w http.ResponseWriter, r *http.Request) {
		pricingCalls.Add(1)
		w.Write([]byte(`{"cpu_price_per_core_hour":1,"memory_price_per_gb_hour":1,"storage_price_per_gb_month":1}`))
	})
	return mux
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func planFor(res ...resourceRequest) presentDeploymentPlanArgs {
	return presentDeploymentPlanArgs{Resources: res, Verification: testVerification}
}

// An already-deployed, approved app, asked for at the size it has, is an
// update: no cost estimate, no fresh approval, and the agent is told to deploy.
func TestPresentDeploymentPlan_UpdatingADeployedAppNeedsNoPlan(t *testing.T) {
	var pricing atomic.Int32
	c := newTestClient(t, deployedSessionMux(true, "inst-live", 1, "1GB", 8, &pricing))

	text := presentText(t, c, planFor(resourceRequest{Name: "app", CPUUnits: 1, MemoryGB: 1, StorageGB: 8}))

	if isPricedPlan(text) {
		t.Fatalf("a plan was presented for an update: %s", text)
	}
	if !strings.Contains(text, "No new plan or approval is needed") || !strings.Contains(text, "call deploy") {
		t.Errorf("the agent is not told to just deploy: %s", text)
	}
	if pricing.Load() != 0 {
		t.Error("pricing was fetched, so a cost estimate was being built")
	}
}

// Asking for less than is running is still an update.
func TestPresentDeploymentPlan_SmallerThanDeployedNeedsNoPlan(t *testing.T) {
	var pricing atomic.Int32
	c := newTestClient(t, deployedSessionMux(true, "inst-live", 2, "4GB", 8, &pricing))
	text := presentText(t, c, planFor(resourceRequest{Name: "app", CPUUnits: 1, MemoryGB: 1}))
	if isPricedPlan(text) || !strings.Contains(text, "No new plan") {
		t.Errorf("got %s", text)
	}
}

// Asking for MORE is a real new cost and must go through the plan.
func TestPresentDeploymentPlan_LargerThanDeployedIsStillPlanned(t *testing.T) {
	cases := []resourceRequest{
		{Name: "app", CPUUnits: 2, MemoryGB: 1, StorageGB: 8},
		{Name: "app", CPUUnits: 1, MemoryGB: 2, StorageGB: 8},
		{Name: "app", CPUUnits: 1, MemoryGB: 1, StorageGB: 20},
	}
	for _, r := range cases {
		var pricing atomic.Int32
		c := newTestClient(t, deployedSessionMux(true, "inst-live", 1, "1GB", 8, &pricing))
		if text := presentText(t, c, planFor(r)); !isPricedPlan(text) {
			t.Errorf("%+v: a bigger request was not presented as a plan: %s", r, text)
		}
	}
}

// Adding a second resource is something new to approve.
func TestPresentDeploymentPlan_ExtraResourceIsStillPlanned(t *testing.T) {
	var pricing atomic.Int32
	c := newTestClient(t, deployedSessionMux(true, "inst-live", 1, "1GB", 8, &pricing))
	text := presentText(t, c, planFor(
		resourceRequest{Name: "app", CPUUnits: 1, MemoryGB: 1},
		resourceRequest{Name: "db", CPUUnits: 1, MemoryGB: 1, StorageGB: 10},
	))
	if !isPricedPlan(text) {
		t.Errorf("a second resource skipped the plan: %s", text)
	}
}

// The first deployment, or one that was never approved, is planned as before.
func TestPresentDeploymentPlan_FirstDeploymentIsPlanned(t *testing.T) {
	for name, mux := range map[string]*http.ServeMux{
		"nothing deployed yet": deployedSessionMux(true, "", 1, "1GB", 8, new(atomic.Int32)),
		"never approved":       deployedSessionMux(false, "inst-live", 1, "1GB", 8, new(atomic.Int32)),
	} {
		c := newTestClient(t, mux)
		if text := presentText(t, c, planFor(resourceRequest{Name: "app", CPUUnits: 1, MemoryGB: 1})); !isPricedPlan(text) {
			t.Errorf("%s: no plan was presented: %s", name, text)
		}
	}
}

// When the state cannot be read the customer is asked, never skipped.
func TestPresentDeploymentPlan_UnreadableStateFallsBackToAPlan(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/build/sessions/sess-test", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("/v1/billing/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"cpu_price_per_core_hour":1,"memory_price_per_gb_hour":1,"storage_price_per_gb_month":1}`))
	})
	c := newTestClient(t, mux)
	if text := presentText(t, c, planFor(resourceRequest{Name: "app", CPUUnits: 1, MemoryGB: 1})); !isPricedPlan(text) {
		t.Errorf("an unreadable session skipped the plan: %s", text)
	}

	// Same when the instance cannot be read or its size is unparseable.
	for name, instance := range map[string]string{"server error": "", "odd memory": `{"cpu_units":1,"memory":"lots","storage_gb":8}`} {
		m := http.NewServeMux()
		m.HandleFunc("/v1/build/sessions/sess-test", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"deploy_approved":true,"app_instance_id":"inst-live"}`))
		})
		m.HandleFunc("/v1/compute/instances/inst-live", func(w http.ResponseWriter, r *http.Request) {
			if instance == "" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Write([]byte(instance))
		})
		m.HandleFunc("/v1/billing/pricing", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"cpu_price_per_core_hour":1,"memory_price_per_gb_hour":1,"storage_price_per_gb_month":1}`))
		})
		cc := newTestClient(t, m)
		if text := presentText(t, cc, planFor(resourceRequest{Name: "app", CPUUnits: 1, MemoryGB: 1})); !isPricedPlan(text) {
			t.Errorf("%s: a plan was skipped on a guess: %s", name, text)
		}
	}
}

// deploy's "not possible here" verdict must be reserved for the one case it
// describes. Other 404s (session or instance gone) are reported as they are.
func TestDeploy_OnlyAMissingBuildPipelineIsADeadEnd(t *testing.T) {
	deployWith := func(body string) string {
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/build/sessions/sess-test", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"deploy_approved":true}`))
		})
		mux.HandleFunc("/v1/build/sessions/sess-test/deploy", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, body, http.StatusNotFound)
		})
		c := newTestClient(t, mux)
		res, _, err := c.deploy(context.Background(), &mcp.CallToolRequest{}, deployArgs{
			Name: "app", CPUUnits: 1, MemoryGB: 1, Ports: []portArg{{Container: 8080}},
		})
		if err != nil {
			t.Fatalf("deploy: %v", err)
		}
		return res.Content[0].(*mcp.TextContent).Text
	}

	if got := deployWith(`{"error":"Teepin Build: build pipeline is not available on this deployment"}`); !strings.Contains(got, "DEPLOYMENT IS NOT POSSIBLE") {
		t.Errorf("a missing build pipeline should still be the dead end, got: %s", got)
	}
	for _, body := range []string{
		`{"error":"this session's previously deployed instance no longer exists in the cluster"}`,
		`{"error":"this session's previously deployed instance no longer exists"}`,
		`{"error":"session not found"}`,
	} {
		got := deployWith(body)
		if strings.Contains(got, "DEPLOYMENT IS NOT POSSIBLE") {
			t.Errorf("%s was reported as \"no build pipeline\": %s", body, got)
		}
		if !strings.Contains(got, "The deploy failed") || !strings.Contains(got, strings.Trim(strings.Split(body, `"`)[3], " ")) {
			t.Errorf("the real reason was not passed on for %s: %s", body, got)
		}
	}
}
