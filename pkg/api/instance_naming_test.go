// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"regexp"
	"testing"
)

// dns1123LabelRE mirrors Kubernetes' own label-value / DNS-1123-label
// validation (lowercase alphanumeric, '-' allowed except at the ends) —
// generateInstanceID's output is used as both, so it must satisfy this
// regardless of which words rand.IntN happens to pick.
var dns1123LabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestGenerateInstanceID_FormatIsStable(t *testing.T) {
	for i := 0; i < 500; i++ {
		id := generateInstanceID()

		if len(id) > 63 {
			t.Fatalf("generateInstanceID() = %q is %d chars, over the 63-char Kubernetes label / DNS label limit", id, len(id))
		}
		if !dns1123LabelRE.MatchString(id) {
			t.Fatalf("generateInstanceID() = %q is not a valid Kubernetes label value / DNS-1123 label", id)
		}

		parts := regexp.MustCompile(`-`).Split(id, 3)
		if len(parts) != 3 {
			t.Fatalf("generateInstanceID() = %q, want exactly 3 hyphen-separated parts (adjective-noun-hex)", id)
		}
		if len(parts[2]) != 4 {
			t.Errorf("generateInstanceID() = %q, want a 4-character hex suffix, got %q", id, parts[2])
		}
		if !regexp.MustCompile(`^[0-9a-f]{4}$`).MatchString(parts[2]) {
			t.Errorf("generateInstanceID() = %q, suffix %q is not lowercase hex", id, parts[2])
		}
	}
}

func TestGenerateInstanceID_WordListsAreCleanDNSLabels(t *testing.T) {
	for _, list := range [][]string{instanceNameAdjectives, instanceNameNouns} {
		for _, w := range list {
			if !dns1123LabelRE.MatchString(w) {
				t.Errorf("word %q is not a valid DNS-1123 label fragment on its own", w)
			}
		}
	}
}
