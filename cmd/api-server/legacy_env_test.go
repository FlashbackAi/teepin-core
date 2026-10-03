// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import "testing"

func TestAdoptLegacyBuildEnv(t *testing.T) {
	env := map[string]string{"TEEPIN_BUILD_AGENT_CPU_UNITS": "8"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	set := func(k, v string) error { env[k] = v; return nil }

	adopted := adoptLegacyBuildEnv([]string{
		"TEEPIN_KUMBHA_ENABLED=true",
		"TEEPIN_KUMBHA_AGENT_CPU_UNITS=2", // the new name is already set: it wins
		"TEEPIN_KUMBHA_BUILD_IMAGE_PULL_SECRET=pull-secret",
		"OTHER=x",
	}, lookup, set)

	if env["TEEPIN_BUILD_ENABLED"] != "true" {
		t.Errorf("enabled not adopted: %v", env)
	}
	if env["TEEPIN_BUILD_AGENT_CPU_UNITS"] != "8" {
		t.Errorf("a setting under its new name was overwritten: %v", env["TEEPIN_BUILD_AGENT_CPU_UNITS"])
	}
	if env["TEEPIN_BUILD_APP_IMAGE_PULL_SECRET"] != "pull-secret" {
		t.Errorf("renamed setting not mapped: %v", env)
	}
	if len(adopted) != 2 {
		t.Errorf("adopted = %v, want the two that were used", adopted)
	}
}
