// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package teepinbuild

import (
	"strings"
	"testing"
)

// The pod's own message names images, registries and nodes; none of that may
// reach a customer, but the cause (capacity, fetching the environment, memory)
// should.
func TestFriendlyStartDetail(t *testing.T) {
	for _, tc := range []struct {
		name, status, msg, want string
	}{
		{"waiting for room", "pending", "0/3 nodes are available: Insufficient cpu", "room to start"},
		{"fetching the image", "pending", "Back-off pulling image \"880254196251.dkr.ecr.us-east-1.amazonaws.com/teepin/kumbha-agent-dev:latest\"", "Fetching the builder"},
		{"just starting", "pending", "", "Preparing the builder"},
		{"cannot fetch the image", "failed", "ImagePullBackOff", "could not be fetched"},
		{"no room", "failed", "Unschedulable", "no machine has room"},
		{"out of memory", "failed", "OOMKilled", "ran out of memory"},
		{"failed for another reason", "failed", "CrashLoopBackOff", "stopped with an error"},
		{"gone", "missing", "", "not running"},
		{"running says nothing", "running", "", ""},
		{"a normal end says nothing", "terminated", "Completed", ""},
	} {
		got := friendlyStartDetail(tc.status, tc.msg)
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q does not contain %q", tc.name, got, tc.want)
		}
		for _, leak := range []string{"ecr", "amazonaws", "880254196251", "build-agent-dev"} {
			if strings.Contains(strings.ToLower(got), leak) {
				t.Errorf("%s: the detail leaks %q: %q", tc.name, leak, got)
			}
		}
	}
}
