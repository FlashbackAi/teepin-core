// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"testing"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
	"github.com/FlashbackAi/teepin-core/pkg/modelprobe"
	"github.com/FlashbackAi/teepin-core/pkg/teepinbuild"
)

// fakeStatusCluster implements only ListInstanceStatuses — the only
// cluster.Client method hiddenWorkloadAdapter calls. Every other method
// panics on a nil embedded interface if ever reached, which would fail
// the test loudly rather than silently doing the wrong thing.
type fakeStatusCluster struct {
	cluster.Client
	statuses []cluster.InstanceStatus
}

func (f *fakeStatusCluster) ListInstanceStatuses(context.Context, cluster.Scope) ([]cluster.InstanceStatus, error) {
	return f.statuses, nil
}

// The whole point of hiddenWorkloadAdapter: it must recognise all three
// hidden pod types by their name prefix, size each correctly, group by
// node, skip a pod that has already finished (terminated), and never
// count a normal (non-hidden) customer instance — that is already
// counted by ListNodeCapacity's own compute.instances sum.
func TestHiddenWorkloadAdapter_RecognisesEachPodType(t *testing.T) {
	fc := &fakeStatusCluster{statuses: []cluster.InstanceStatus{
		{PodName: "build-agent-abcd1234-f3a91", NodeName: "srialla", Hidden: true, Status: compute.StatusRunning},
		{PodName: "build-shot-abcd1234-f3a91", NodeName: "srialla", Hidden: true, Status: compute.StatusRunning},
		{PodName: "kaniko-build-session1-f3a91", NodeName: "fblabs01", Hidden: true, Status: compute.StatusPending},
		// A normal customer instance — Hidden=false — must never be
		// counted here; it's already in the DB-based sum.
		{PodName: "quiet-grove-8524-pod", NodeName: "srialla", Hidden: false, Status: compute.StatusRunning},
		// A finished build sitting in "terminated" holds nothing.
		{PodName: "kaniko-build-finished-f3a91", NodeName: "srialla", Hidden: true, Status: compute.StatusTerminated},
	}}

	a := newHiddenWorkloadAdapter(fc, 2, 4, 2, 4)
	usage, err := a.HiddenUsageByNode(context.Background())
	if err != nil {
		t.Fatalf("HiddenUsageByNode: %v", err)
	}

	srialla := usage["srialla"]
	wantCPU := 2 + teepinbuild.ScreenshotCPUUnits
	wantMem := 4 + teepinbuild.ScreenshotMemoryGB
	if srialla.CPUCores != wantCPU || srialla.MemoryGB != wantMem {
		t.Errorf("srialla usage = %+v, want agent(2/4) + screenshot(%d/%d) = %d/%d",
			srialla, teepinbuild.ScreenshotCPUUnits, teepinbuild.ScreenshotMemoryGB, wantCPU, wantMem)
	}

	fblabs := usage["fblabs01"]
	if fblabs.CPUCores != 2 || fblabs.MemoryGB != 4 {
		t.Errorf("fblabs01 usage = %+v, want the pending Kaniko build's 2/4", fblabs)
	}

	if _, ok := usage["fblabs01"]; !ok || len(usage) != 2 {
		t.Errorf("usage map = %+v, want exactly {srialla, fblabs01} (terminated build must not add a third entry)", usage)
	}
}

// A pod carrying no recognised prefix (a future hidden pod type this
// adapter was never updated for) must count nothing rather than guess —
// silently miscounting a size is worse than silently under-counting one
// unrecognised pod, which at least degrades no worse than before this
// adapter existed.
func TestHiddenWorkloadAdapter_UnrecognisedPrefixCountsNothing(t *testing.T) {
	fc := &fakeStatusCluster{statuses: []cluster.InstanceStatus{
		{PodName: "some-future-hidden-pod-abcd", NodeName: "srialla", Hidden: true, Status: compute.StatusRunning},
	}}

	a := newHiddenWorkloadAdapter(fc, 2, 4, 2, 4)
	usage, err := a.HiddenUsageByNode(context.Background())
	if err != nil {
		t.Fatalf("HiddenUsageByNode: %v", err)
	}
	if len(usage) != 0 {
		t.Errorf("usage = %+v, want empty (no recognised hidden pod)", usage)
	}
}

func TestBuilderCapabilities_FollowsEvidenceThenTheDeclaredFlags(t *testing.T) {
	passed := func(c modelprobe.Capability) modelprobe.Check {
		return modelprobe.Check{Capability: c, Status: modelprobe.StatusPassed}
	}
	failed := func(c modelprobe.Capability) modelprobe.Check {
		return modelprobe.Check{Capability: c, Status: modelprobe.StatusFailed}
	}

	for _, tc := range []struct {
		name        string
		declared    modelcatalog.Model
		rep         *modelprobe.Report
		unavailable string
		wantTools   bool
		wantWhy     string
	}{
		{"declared tools, never checked: allowed (a model registered before checks existed)",
			modelcatalog.Model{SupportsTools: true}, nil, "", true, ""},
		{"no tools declared, never checked: not for building",
			modelcatalog.Model{}, nil, "", false, notBuildCapable},
		{"declared tools but failed its check: not for building",
			modelcatalog.Model{SupportsTools: true}, &modelprobe.Report{Checks: []modelprobe.Check{failed(modelprobe.CapTools)}}, "", false, notBuildCapable},
		{"not declared but passed its check: allowed",
			modelcatalog.Model{}, &modelprobe.Report{Checks: []modelprobe.Check{passed(modelprobe.CapTools)}}, "", true, ""},
		{"an outage reason is kept ahead of the tools reason",
			modelcatalog.Model{}, nil, "Temporarily unavailable", false, "Temporarily unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := builderCapabilities(tc.declared, tc.rep, tc.unavailable)
			if v.Tools != tc.wantTools || v.Unavailable != tc.wantWhy {
				t.Errorf("tools = %v why = %q; want %v %q", v.Tools, v.Unavailable, tc.wantTools, tc.wantWhy)
			}
		})
	}

	// Vision follows evidence per model, not one global switch.
	if v := builderCapabilities(modelcatalog.Model{SupportsTools: true, SupportsVision: true},
		&modelprobe.Report{Checks: []modelprobe.Check{failed(modelprobe.CapVision)}}, ""); v.Vision {
		t.Error("a model that failed its vision check must not be treated as seeing images")
	}
}
