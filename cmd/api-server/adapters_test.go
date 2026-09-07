// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"testing"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
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
		{PodName: "kumbha-agent-abcd1234-f3a91", NodeName: "srialla", Hidden: true, Status: compute.StatusRunning},
		{PodName: "kumbha-shot-abcd1234-f3a91", NodeName: "srialla", Hidden: true, Status: compute.StatusRunning},
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
	wantCPU := 2 + kumbha.ScreenshotCPUUnits
	wantMem := 4 + kumbha.ScreenshotMemoryGB
	if srialla.CPUCores != wantCPU || srialla.MemoryGB != wantMem {
		t.Errorf("srialla usage = %+v, want agent(2/4) + screenshot(%d/%d) = %d/%d",
			srialla, kumbha.ScreenshotCPUUnits, kumbha.ScreenshotMemoryGB, wantCPU, wantMem)
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
