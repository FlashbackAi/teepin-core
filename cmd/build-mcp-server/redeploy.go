// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Updating an app that is already running is not a new purchase. A build
// session that has deployed an app, and had its cost approved, can change the
// code and call deploy again: the control plane swaps the image in the same
// instance, at the same size, so there is nothing new to cost out and nothing
// new for the customer to approve.
//
// present_deployment_plan used to be called for every deploy anyway (its
// description said "always call this before provisioning anything"), which
// showed the customer a fresh cost estimate and an "already approved" plan
// for a change that cost nothing extra. It now answers such a call directly,
// unless the request needs MORE than the running app has, in which case it is
// a real new cost and goes through the normal plan.

// deployedApp is what the session already has running under an approved plan.
type deployedApp struct {
	InstanceID string
	CPUUnits   int
	MemoryGB   int
	StorageGB  int
}

// approvedDeployment returns the session's running, approved app, or nil when
// there is none (nothing deployed yet, or the plan was never approved). Any
// error means "cannot tell", and callers must then treat it as no app, so the
// customer is asked rather than a cost silently skipped.
func (c *teepinClient) approvedDeployment(ctx context.Context) (*deployedApp, error) {
	var sess struct {
		DeployApproved bool   `json:"deploy_approved"`
		AppInstanceID  string `json:"app_instance_id"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/build/sessions/"+c.sessionID, nil, &sess); err != nil {
		return nil, err
	}
	if !sess.DeployApproved || sess.AppInstanceID == "" {
		return nil, nil
	}

	var inst struct {
		CPUUnits  int    `json:"cpu_units"`
		Memory    string `json:"memory"`
		StorageGB int    `json:"storage_gb"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/compute/instances/"+sess.AppInstanceID, nil, &inst); err != nil {
		return nil, err
	}
	var memGB int
	if _, err := fmt.Sscanf(strings.TrimSpace(inst.Memory), "%dGB", &memGB); err != nil {
		return nil, fmt.Errorf("unreadable memory size %q on instance %s", inst.Memory, sess.AppInstanceID)
	}
	return &deployedApp{InstanceID: sess.AppInstanceID, CPUUnits: inst.CPUUnits, MemoryGB: memGB, StorageGB: inst.StorageGB}, nil
}

// fitsDeployed reports whether a plan asks for nothing the running app does
// not already have: exactly one resource, no larger in CPU, memory or
// storage. More than one resource means something new is being added, which
// needs approving.
func fitsDeployed(app *deployedApp, requested []resourceRequest) bool {
	if app == nil || len(requested) != 1 {
		return false
	}
	r := requested[0]
	return r.CPUUnits <= app.CPUUnits && r.MemoryGB <= app.MemoryGB && r.StorageGB <= app.StorageGB
}

const alreadyDeployedMessage = "No new plan or approval is needed. This app is already deployed (instance %s: " +
	"%d vCPU, %d GB RAM) and its cost was approved, and what you described does not need more resources than that. " +
	"Changing the code of a running app costs nothing extra. Check your change locally, then call deploy to " +
	"update the running app in place. Only present a new plan if you need MORE CPU, memory or storage than it has now."

// errPipelineUnavailable is the control plane's own words for "this
// deployment has no image build pipeline".
const pipelineUnavailableText = "build pipeline is not available"
